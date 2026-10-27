// Package ofx reads the T-Bank XML statement without exposing source contents in errors.
package ofx

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"io"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fin-assist/bank-statement-data/internal/domain"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Parser implements the source admission and background parsing ports.
type Parser struct{}

var _ importing.Parser = Parser{}

var errMalformed = errors.New("malformed OFX statement")
var errInputTooLarge = errors.New("OFX parser input budget exceeded")

// These are parser resource ceilings, not new client-facing refusal codes.
const maxInputBytes = 10 << 20
const maxXMLDepth = 64

// A shortest complete transaction uses 82 bytes for four elements. At 10 MiB
// that is fewer than 512k elements. The independent work budget has twice that
// headroom while rejecting millions of tiny extension elements; it does not
// replace the HTTP file-size or domain limits.
const maxXMLElements = 1 << 20

func readSource(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxInputBytes {
		return nil, errInputTooLarge
	}
	return data, nil
}

// Validate checks source format and bank without interpreting transactions.
// Incomplete transaction XML is left to Parse, after durable admission.
func (Parser) Validate(r io.Reader) error {
	data, err := readSource(r)
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return importing.ErrWrongFormat
	}
	text := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if strings.HasPrefix(text, "<?xml ") {
		end := strings.Index(text, "?>")
		if end < 0 || attribute(text[:end], "encoding") != "" && !strings.EqualFold(attribute(text[:end], "encoding"), "UTF-8") {
			return importing.ErrWrongFormat
		}
	}
	if !strings.Contains(text, "<OFX") {
		if looksCSV(text) {
			return importing.ErrCSVNotAccepted
		}
		return importing.ErrWrongFormat
	}
	// Lexical scanning intentionally does not validate nesting of transaction bodies.
	root, header, accounts, banks, inAccount := false, false, 0, 0, false
	firstElement := true
	elements := 0
	scanner := tagScanner{text: text}
	for {
		start, end, found := scanner.next()
		if !found {
			break
		}
		cursor := end
		tag := text[start:end]
		if strings.HasPrefix(tag, "<!--") || strings.HasPrefix(tag, "<![CDATA[") {
			continue
		}
		if strings.HasPrefix(tag, "<!") {
			return importing.ErrWrongFormat
		}
		if !strings.HasPrefix(tag, "<?") && !strings.HasPrefix(tag, "</") {
			elements++
			if elements > maxXMLElements {
				return importing.ErrWrongFormat
			}
		}
		if !strings.HasPrefix(tag, "<?") && firstElement {
			if tag != "<OFX>" {
				return importing.ErrWrongFormat
			}
			firstElement = false
		}
		if strings.HasPrefix(tag, "<?OFX") && len(tag) > 5 && strings.ContainsAny(tag[5:6], " \t\r\n") {
			header = attribute(tag, "OFXHEADER") == "200" && attribute(tag, "VERSION") == "202"
		}
		switch strings.TrimSpace(tag) {
		case "<OFX>":
			root = true
		case "<BANKACCTFROM>":
			accounts++
			inAccount = true
		case "</BANKACCTFROM>":
			inAccount = false
		case "<BANKID>":
			if inAccount {
				end := strings.IndexByte(text[cursor:], '<')
				if end < 0 {
					return importing.ErrWrongFormat
				}
				if strings.TrimSpace(text[cursor:cursor+end]) != "T-BANK" {
					return importing.ErrWrongBank
				}
				banks++
			}
		}
	}
	if !root || !header || accounts != banks {
		return importing.ErrWrongFormat
	}
	if accounts == 0 {
		// A successful empty export has no account header or BANKID at all.
		if !strings.Contains(text, "<BANKMSGSRSV1") || !regexp.MustCompile(`<CODE>\s*0\s*</CODE>`).MatchString(text) {
			return importing.ErrWrongFormat
		}
	}
	return nil
}

// tagScanner retains only its cursor and three failed-terminator flags. Once a
// terminator is absent from the suffix it cannot occur in any later suffix;
// remembering that fact keeps repeated incomplete special tokens linear too.
type tagScanner struct {
	text    string
	cursor  int
	missing [3]bool
}

// next returns original byte offsets without collecting or allocating tag indexes.
// Incomplete special tokens retain the old generic-tag fallback: malformed
// transaction XML still belongs to Parse rather than synchronous admission.
func (s *tagScanner) next() (start, end int, found bool) {
	relative := strings.IndexByte(s.text[s.cursor:], '<')
	if relative < 0 {
		return 0, 0, false
	}
	start = s.cursor + relative
	tail := s.text[start:]
	skip, terminator, kind := 1, ">", -1
	switch {
	case strings.HasPrefix(tail, "<!--"):
		skip, terminator, kind = 4, "-->", 0
	case strings.HasPrefix(tail, "<![CDATA["):
		skip, terminator, kind = 9, "]]>", 1
	case strings.HasPrefix(tail, "<?"):
		skip, terminator, kind = 2, "?>", 2
	}
	if kind < 0 || !s.missing[kind] {
		if offset := strings.Index(tail[skip:], terminator); offset >= 0 {
			s.cursor = start + skip + offset + len(terminator)
			return start, s.cursor, true
		}
		if kind >= 0 {
			s.missing[kind] = true
		}
	}
	if kind >= 0 {
		if offset := strings.IndexByte(tail, '>'); offset >= 0 {
			s.cursor = start + offset + 1
			return start, s.cursor, true
		}
	}
	return 0, 0, false
}

func attribute(tag, key string) string {
	if key == "" {
		return ""
	}
	for cursor := 0; cursor < len(tag); {
		relative := strings.Index(tag[cursor:], key)
		if relative < 0 {
			return ""
		}
		start := cursor + relative
		cursor = start + len(key)
		// Preserve the old RE2 ASCII word boundary, including attribute-like text
		// inside PI data; this helper intentionally is not a new XML schema parser.
		if start > 0 && attributeWord(tag[start-1]) {
			continue
		}
		end := cursor
		for end < len(tag) && attributeSpace(tag[end]) {
			end++
		}
		if end >= len(tag) || tag[end] != '=' {
			continue
		}
		end++
		for end < len(tag) && attributeSpace(tag[end]) {
			end++
		}
		if end >= len(tag) || tag[end] != '\'' && tag[end] != '"' {
			continue
		}
		end++
		if close := strings.IndexAny(tag[end:], "\"'"); close >= 0 {
			return tag[end : end+close]
		}
	}
	return ""
}

func attributeWord(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

func attributeSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func looksCSV(text string) bool {
	line, _, _ := strings.Cut(text, "\n")
	if strings.Count(line, ";") >= 2 || strings.Count(line, ",") >= 2 || strings.Count(line, "\t") >= 2 {
		return true
	}
	// Two-column CSV is still CSV. Require a consistent second record to avoid
	// classifying an arbitrary sentence with one comma as a tabular export.
	for _, delimiter := range []rune{';', ',', '\t'} {
		reader := csv.NewReader(strings.NewReader(text))
		reader.Comma = delimiter
		first, err := reader.Read()
		if err != nil || len(first) < 2 {
			continue
		}
		if second, err := reader.Read(); err == nil && len(second) == len(first) {
			return true
		}
	}
	return false
}

type node struct {
	name string
	text strings.Builder
}

type row struct {
	line   int
	fields map[string]string
}

// Parse validates XML structure and returns neutral source rows. Reader errors,
// cancellation and malformed XML never include source data in the returned error.
func (p Parser) Parse(ctx context.Context, r io.Reader) (importing.Document, error) {
	data, err := readSource(contextReader{ctx, r})
	if err != nil {
		return importing.Document{}, err
	}
	if err := p.Validate(bytes.NewReader(data)); err != nil {
		return importing.Document{}, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	// Keep offsets against the exact decoder input, including original newlines.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	// Decoder offsets are monotonic; count only the range since the previous row
	// instead of retaining one index per source newline.
	lineOffset := 0
	var doc importing.Document
	var stack []*node
	var account, period, fields map[string]string
	var rows []row
	lineNumber := 1
	rootSeen := false
	for {
		if err := ctx.Err(); err != nil {
			return importing.Document{}, err
		}
		offset := int(decoder.InputOffset())
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return importing.Document{}, errMalformed
		}
		switch v := token.(type) {
		case xml.Directive:
			return importing.Document{}, errMalformed
		case xml.StartElement:
			if len(stack) >= maxXMLDepth || v.Name.Space != "" || len(v.Attr) != 0 || len(stack) == 0 && (rootSeen || v.Name.Local != "OFX") {
				return importing.Document{}, errMalformed
			}
			if len(stack) == 0 {
				rootSeen = true
			}
			stack = append(stack, &node{name: v.Name.Local})
			parent := ""
			if len(stack) > 1 {
				parent = stack[len(stack)-2].name
			}
			switch v.Name.Local {
			case "STMTTRNRS":
				if account != nil || parent != "BANKMSGSRSV1" {
					return importing.Document{}, errMalformed
				}
				account, period, rows = map[string]string{}, map[string]string{}, nil
			case "BANKACCTFROM", "BANKTRANLIST":
				if account == nil || parent != "STMTRS" {
					return importing.Document{}, errMalformed
				}
			case "STMTTRN":
				if fields != nil || account == nil || parent != "BANKTRANLIST" {
					return importing.Document{}, errMalformed
				}
				fields = map[string]string{}
				lineNumber += bytes.Count(data[lineOffset:offset], []byte{'\n'})
				lineOffset = offset
			}
		case xml.CharData:
			if len(stack) == 0 {
				if len(bytes.TrimSpace(v)) != 0 {
					return importing.Document{}, errMalformed
				}
				continue
			}
			raw := string(data[offset:int(decoder.InputOffset())])
			if strings.HasPrefix(raw, "<![CDATA[") {
				// CDATA is already literal; protect ampersands from the later single pass.
				raw = strings.ReplaceAll(string(v), "&", "&amp;")
			}
			stack[len(stack)-1].text.WriteString(raw)
		case xml.EndElement:
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			value := strings.TrimSpace(n.text.String())
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1].name
			}
			if fields != nil {
				// Transaction fields are direct children, except CURSYM under CURRENCY.
				if parent == "STMTTRN" || parent == "CURRENCY" && n.name == "CURSYM" && len(stack) > 1 && stack[len(stack)-2].name == "STMTTRN" {
					fields[n.name] = value
				}
			} else if account != nil {
				if parent == "BANKACCTFROM" {
					account[n.name] = value
				}
				if parent == "BANKTRANLIST" && (n.name == "DTSTART" || n.name == "DTEND") {
					period[n.name] = value
				}
			}
			switch n.name {
			case "STMTTRN":
				rows = append(rows, row{lineNumber, fields})
				fields = nil
			case "STMTTRNRS":
				if err := appendStatement(&doc, account, period, rows); err != nil {
					return importing.Document{}, err
				}
				account = nil
			}
		}
	}
	if !rootSeen || len(stack) != 0 {
		return importing.Document{}, errMalformed
	}
	return doc, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func appendStatement(doc *importing.Document, account, period map[string]string, rows []row) error {
	key := account["ACCTID"]
	if key == "" {
		return nil
	}
	chars := []rune(key)
	other := strings.HasPrefix(key, "OB$")
	if len(chars) > 64 {
		return errMalformed
	}
	kind := strings.ToLower(account["ACCTTYPE"])
	if kind == "" {
		kind = domain.AccountKindChecking
	}
	a := importing.Account{Key: key, Type: kind, OtherBank: other}
	if !other {
		mask := string(chars[max(0, len(chars)-4):])
		a.Mask = &mask
	}
	doc.Accounts = append(doc.Accounts, a)
	from, to := parseDate(period["DTSTART"]), parseDate(period["DTEND"])
	if from != nil && to != nil {
		if from.After(*to) {
			return errMalformed
		}
		doc.Periods = append(doc.Periods, importing.Period{AccountKey: key, From: *from, To: *to})
	}
	for _, source := range rows {
		f := source.fields
		line := importing.Line{Number: source.line, AccountKey: key, FITID: f["FITID"], OccurredAt: parseDate(f["DTPOSTED"]), Currency: f["CURSYM"], Description: prefixRunes(unescape(f["NAME"]), 1000), BankCategory: unescape(f["MEMO"])}
		oversizedFITID := utf8.RuneCountInString(line.FITID) > 255
		if line.Currency == "" {
			line.Currency = "RUB"
		}
		amount, negative, ok := parseMoney(f["TRNAMT"], line.Currency)
		line.AmountMinor = amount
		line.Direction = "income"
		if negative || strings.EqualFold(f["TRNTYPE"], "DEBIT") {
			line.Direction = "expense"
		}
		switch {
		case !hasField(f, "DTPOSTED"):
			line.Reason = "no_date"
		case line.OccurredAt == nil:
			line.Reason = "bad_date"
		case !ok:
			line.Reason = "bad_amount"
		case line.FITID == "" || oversizedFITID:
			line.Reason = "truncated"
		}
		if oversizedFITID {
			// Invalid source IDs cannot be persisted as unread metadata or shortened into a different key.
			line.FITID = ""
		}
		if line.Reason != "" {
			// Unread metadata must never persist raw descriptions or malformed source values.
			line.Description, line.BankCategory, line.Currency, line.Direction = "", "", "", ""
			line.AmountMinor = 0
		}
		doc.Lines = append(doc.Lines, line)
	}
	return nil
}

func unescape(text string) string {
	return strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&apos;", "'").Replace(text)
}

func prefixRunes(value string, limit int) string {
	count := 0
	for offset := range value {
		if count == limit {
			return strings.Clone(value[:offset])
		}
		count++
	}
	return value
}

func hasField(fields map[string]string, key string) bool {
	_, ok := fields[key]
	return ok
}

func parseMoney(raw, currency string) (int64, bool, bool) {
	scale, known := currencyMinorUnits(currency)
	if !known {
		return 0, false, false
	}
	if !regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?$`).MatchString(raw) {
		return 0, false, false
	}
	negative := strings.HasPrefix(raw, "-") && strings.Trim(raw, "-0.") != ""
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "-"), "+")
	whole, fraction, _ := strings.Cut(raw, ".")
	whole = strings.TrimLeft(whole, "0")
	if len(whole) > 19-scale { // Even before rounding, this cannot fit int64 minor units.
		return 0, negative, false
	}
	if whole == "" {
		whole = "0"
	}
	fraction += strings.Repeat("0", scale)
	n := new(big.Int)
	n.SetString(whole+fraction[:scale], 10)
	if len(fraction) > scale && fraction[scale] >= '5' {
		n.Add(n, big.NewInt(1))
	}
	if !n.IsInt64() {
		return 0, negative, false
	}
	return n.Int64(), negative, true
}

func parseDate(raw string) *time.Time {
	m := regexp.MustCompile(`^([0-9]{8})([0-9]{6})?(?:\.([0-9]+))?(?:\[([+-]?[0-9]{1,2})(?::[A-Za-z]+)?\])?$`).FindStringSubmatch(raw)
	if m == nil {
		return nil
	}
	clock := m[2]
	if clock == "" {
		clock = "000000"
	}
	offset := 3
	if m[4] != "" {
		offset, _ = strconv.Atoi(m[4])
	}
	if offset < -23 || offset > 23 {
		return nil
	}
	value := m[1] + clock
	if m[3] != "" {
		value += "." + m[3]
	}
	t, err := time.ParseInLocation("20060102150405", value, time.FixedZone("", offset*3600))
	if err != nil {
		return nil
	}
	return &t
}
