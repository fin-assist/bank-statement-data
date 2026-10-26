// Package ofx reads the T-Bank XML statement without exposing source contents in errors.
package ofx

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Parser implements the source admission and background parsing ports.
type Parser struct{}

var _ importing.Parser = Parser{}

var errMalformed = errors.New("malformed OFX statement")

// Validate checks source format and bank without interpreting transactions.
// Incomplete transaction XML is left to Parse, after durable admission.
func (Parser) Validate(r io.Reader) error {
	data, err := io.ReadAll(r)
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
	tokens := regexp.MustCompile(`(?s)<!--.*?-->|<!\[CDATA\[.*?\]\]>|<\?.*?\?>|<[^>]*>`).FindAllStringIndex(text, -1)
	root, header, accounts, banks, inAccount := false, false, 0, 0, false
	firstElement := true
	for _, span := range tokens {
		tag := text[span[0]:span[1]]
		if strings.HasPrefix(tag, "<!--") || strings.HasPrefix(tag, "<![CDATA[") {
			continue
		}
		if strings.HasPrefix(tag, "<!") {
			return importing.ErrWrongFormat
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
				end := strings.IndexByte(text[span[1]:], '<')
				if end < 0 {
					return importing.ErrWrongFormat
				}
				if strings.TrimSpace(text[span[1]:span[1]+end]) != "T-BANK" {
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

func attribute(tag, key string) string {
	re := regexp.MustCompile(`\b` + key + `\s*=\s*["']([^"']*)["']`)
	m := re.FindStringSubmatch(tag)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

func looksCSV(text string) bool {
	line, _, _ := strings.Cut(text, "\n")
	return strings.Count(line, ";") >= 2 || strings.Count(line, ",") >= 2 || strings.Count(line, "\t") >= 2
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
	data, err := io.ReadAll(contextReader{ctx, r})
	if err != nil {
		return importing.Document{}, err
	}
	if err := p.Validate(bytes.NewReader(data)); err != nil {
		return importing.Document{}, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	// Keep offsets against the exact decoder input, including original newlines.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	starts := []int{0}
	for i, c := range data {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	var doc importing.Document
	var stack []*node
	var account, period, fields map[string]string
	var rows []row
	lineNumber := 0
	rootSeen := false
	for {
		if err := ctx.Err(); err != nil {
			return importing.Document{}, err
		}
		offset := int(decoder.InputOffset())
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return importing.Document{}, errMalformed
		}
		switch v := token.(type) {
		case xml.Directive:
			return importing.Document{}, errMalformed
		case xml.StartElement:
			if v.Name.Space != "" || len(v.Attr) != 0 || len(stack) == 0 && (rootSeen || v.Name.Local != "OFX") {
				return importing.Document{}, errMalformed
			}
			if len(stack) == 0 {
				rootSeen = true
			}
			stack = append(stack, &node{name: v.Name.Local})
			switch v.Name.Local {
			case "STMTTRNRS":
				if account != nil {
					return importing.Document{}, errMalformed
				}
				account, period, rows = map[string]string{}, map[string]string{}, nil
			case "STMTTRN":
				if fields != nil || account == nil {
					return importing.Document{}, errMalformed
				}
				fields = map[string]string{}
				lineNumber = sort.Search(len(starts), func(i int) bool { return starts[i] > offset })
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
			if fields != nil {
				fields[n.name] = value
			} else if account != nil {
				account[n.name] = value
				if n.name == "DTSTART" || n.name == "DTEND" {
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
	if len(chars) > 64 || !other && len(chars) < 4 {
		return errMalformed
	}
	kind := strings.ToLower(account["ACCTTYPE"])
	if kind == "" {
		kind = "checking"
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`).MatchString(kind) {
		return errMalformed
	}
	a := importing.Account{Key: key, Type: kind, OtherBank: other}
	if !other {
		mask := string(chars[len(chars)-4:])
		a.Mask = &mask
	}
	doc.Accounts = append(doc.Accounts, a)
	from, to := parseDate(period["DTSTART"]), parseDate(period["DTEND"])
	if from != nil && to != nil {
		doc.Periods = append(doc.Periods, importing.Period{AccountKey: key, From: *from, To: *to})
	}
	for _, source := range rows {
		f := source.fields
		line := importing.Line{Number: source.line, AccountKey: key, FITID: f["FITID"], OccurredAt: parseDate(f["DTPOSTED"]), Currency: f["CURSYM"], Description: unescape(f["NAME"]), BankCategory: unescape(f["MEMO"])}
		if line.Currency == "" {
			line.Currency = "RUB"
		}
		amount, negative, ok := parseMoney(f["TRNAMT"])
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
		case line.FITID == "":
			line.Reason = "truncated"
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

func hasField(fields map[string]string, key string) bool {
	_, ok := fields[key]
	return ok
}

func parseMoney(raw string) (int64, bool, bool) {
	if !regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?$`).MatchString(raw) {
		return 0, false, false
	}
	negative := strings.HasPrefix(raw, "-") && strings.Trim(raw, "-0.") != ""
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "-"), "+")
	whole, fraction, _ := strings.Cut(raw, ".")
	fraction += "00"
	n := new(big.Int)
	n.SetString(whole+fraction[:2], 10)
	if len(fraction) > 2 && fraction[2] >= '5' {
		n.Add(n, big.NewInt(1))
	}
	if !n.IsInt64() {
		return 0, negative, false
	}
	return n.Int64(), negative, true
}

func parseDate(raw string) *time.Time {
	m := regexp.MustCompile(`^([0-9]{8})([0-9]{6})?(?:\.[0-9]+)?(?:\[([+-]?[0-9]{1,2})(?::[A-Za-z]+)?\])?$`).FindStringSubmatch(raw)
	if m == nil {
		return nil
	}
	clock := m[2]
	if clock == "" {
		clock = "000000"
	}
	offset := 3
	if m[3] != "" {
		offset, _ = strconv.Atoi(m[3])
	}
	if offset < -23 || offset > 23 {
		return nil
	}
	t, err := time.ParseInLocation("20060102150405", m[1]+clock, time.FixedZone("", offset*3600))
	if err != nil {
		return nil
	}
	return &t
}
