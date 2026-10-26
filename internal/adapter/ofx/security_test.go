package ofx

import (
	"context"
	"errors"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func securityEmpty(body string) string {
	return `<?OFX OFXHEADER="200" VERSION="202"?><OFX><BANKMSGSRSV1><STATUS><CODE>0</CODE></STATUS>` + body + `</BANKMSGSRSV1></OFX>`
}

func TestUnitSecurityElementBudgetHasExactInclusiveBoundary(t *testing.T) {
	// OFX, BANKMSGSRSV1, STATUS and CODE consume four actual start elements.
	for _, tc := range []struct {
		name, tag string
		extra     int
		want      error
	}{
		{"self-closing-at-limit", "<X/>", 0, nil},
		{"paired-at-limit", "<X></X>", 0, nil},
		{"one-over-limit", "<X/>", 1, importing.ErrWrongFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := securityEmpty(strings.Repeat(tc.tag, maxXMLElements-4+tc.extra))
			if len(source) > maxInputBytes {
				t.Fatal("fixture exceeds independent byte budget")
			}
			if err := (Parser{}).Validate(strings.NewReader(source)); !errors.Is(err, tc.want) {
				t.Fatal("incorrect element boundary", err)
			}
		})
	}
}

func TestUnitSecurityWideXMLIsRejectedWithoutMaterializingTagIndexes(t *testing.T) {
	source := securityEmpty(strings.Repeat("<X/>", 2_500_000))
	if len(source) > maxInputBytes {
		t.Fatal("fixture must reach element guard before byte guard")
	}
	// io.ReadAll's geometric growth plus one byte-to-string copy fit comfortably
	// within eight input buffers. Retaining millions of []int indexes does not.
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := (Parser{}).Validate(strings.NewReader(source))
	runtime.ReadMemStats(&after)
	if !errors.Is(err, importing.ErrWrongFormat) {
		t.Fatal("wide shallow XML was admitted", err)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("validation allocated %d bytes for %d-byte synthetic input", allocated, len(source))
	if allocated > uint64(8*maxInputBytes) {
		t.Fatalf("validation allocated %d bytes for %d-byte input", allocated, len(source))
	}
	if _, err := (Parser{}).Parse(context.Background(), strings.NewReader(source)); !errors.Is(err, importing.ErrWrongFormat) {
		t.Fatal("background parsing bypassed element budget", err)
	}
}

func TestUnitSecurityLexicalDelimitersDoNotCountEmbeddedMarkup(t *testing.T) {
	embedded := strings.Repeat("<X/>", maxXMLElements+1)
	for _, tc := range []struct{ name, body string }{
		{"comment", "<!-- " + embedded + " -->"},
		{"cdata", "<![CDATA[" + embedded + "]]>"},
		{"processing-instruction", "<?extension " + embedded + "?>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := (Parser{}).Validate(strings.NewReader(securityEmpty(tc.body))); err != nil {
				t.Fatal("embedded text counted as XML elements", err)
			}
		})
	}
}

func TestUnitSecurityScannerKeepsOriginalOffsetsAndIncompleteTokenSemantics(t *testing.T) {
	source := "prefix\n<!-- <hidden/> > -->\n<![CDATA[<text/> >]]>\n<?extension value=\"<literal/>\"?>\n<OFX></OFX>tail"
	scanner := tagScanner{text: source}
	cursor := 0
	for _, want := range []string{"<!-- <hidden/> > -->", "<![CDATA[<text/> >]]>", `<?extension value="<literal/>"?>`, "<OFX>", "</OFX>"} {
		start, end, found := scanner.next()
		expected := cursor + strings.Index(source[cursor:], want)
		if !found || start != expected || source[start:end] != want {
			t.Fatal("scanner shifted raw source offsets")
		}
		cursor = end
	}
	if _, _, found := scanner.next(); found {
		t.Fatal("trailing text became a tag")
	}
	for _, token := range []string{"<", "<!--", "<![CDATA[", "<?"} {
		s := tagScanner{text: token}
		if _, _, found := s.next(); found {
			t.Fatal("unterminated token acquired a fabricated end")
		}
	}
	// No terminator exists anywhere in the suffix. Repeated malformed PIs must
	// not rescan the remaining multi-megabyte suffix once per token.
	source = securityEmpty(strings.Repeat("<?x>", maxXMLElements))
	if err := (Parser{}).Validate(strings.NewReader(source)); err != nil {
		t.Fatal("admission began validating transaction nesting", err)
	}
	if _, err := (Parser{}).Parse(context.Background(), strings.NewReader(source)); !errors.Is(err, errMalformed) {
		t.Fatal("malformed XML did not fail background parsing", err)
	}
}

func TestUnitSecurityRepeatedHeaderPIsKeepValidationAllocationBounded(t *testing.T) {
	const pi = `<?OFX OFXHEADER="200" VERSION="202"?>`
	empty := securityEmpty("")
	source := strings.Repeat(pi, (maxInputBytes-len(empty))/len(pi)) + empty
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := (Parser{}).Validate(strings.NewReader(source))
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal("bounded repeated PI input changed admission semantics", err)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("validation allocated %d bytes for %d-byte repeated-PI input", allocated, len(source))
	if allocated > uint64(8*maxInputBytes) {
		t.Fatalf("header scanning allocated %d bytes for %d-byte input", allocated, len(source))
	}
}

func FuzzUnitSecurityHeaderAttributesPreserveLegacyMatching(f *testing.F) {
	for _, seed := range []string{
		`<?OFX OFXHEADER="200" VERSION="202"?>`,
		`xOFXHEADER='bad' OFXHEADER='200'`,
		`VERSION = "202'`,
		"encoding\t=\f'UTF-8'",
		`OFXHEADER='unterminated`,
		`OFXHEADER=none VERSION='202'`,
		`_OFXHEADER='bad' VERSION='202'`,
		`яOFXHEADER='200'`,
		`OFXHEADERX='bad' OFXHEADER='200'`,
		`<?OFX junk="OFXHEADER='200'" VERSION='202'?>`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, tag string) {
		for _, key := range []string{"OFXHEADER", "VERSION", "encoding"} {
			legacy := regexp.MustCompile(`\b` + key + `\s*=\s*["']([^"']*)["']`).FindStringSubmatch(tag)
			want := ""
			if len(legacy) == 2 {
				want = legacy[1]
			}
			if attribute(tag, key) != want {
				t.Fatal("header attribute matching changed")
			}
		}
	})
}

func TestUnitSecurityParseDoesNotIndexEverySourceNewline(t *testing.T) {
	empty := securityEmpty("")
	source := securityEmpty(strings.Repeat("\n", maxInputBytes-len(empty)))
	if len(source) != maxInputBytes {
		t.Fatal("fixture must reach the exact parser byte budget")
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	document, err := (Parser{}).Parse(context.Background(), strings.NewReader(source))
	runtime.ReadMemStats(&after)
	if err != nil || len(document.Accounts) != 0 || len(document.Lines) != 0 || len(document.Periods) != 0 {
		t.Fatal("successful whitespace-only export changed", err)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("Parse allocated %d bytes for %d-byte newline input", allocated, len(source))
	// The bounded input copies, XML token buffer and raw character span remain
	// linear in bytes. A growing []int entry per newline exceeds this margin.
	if allocated > uint64(24*maxInputBytes) {
		t.Fatalf("Parse allocated %d bytes for %d-byte input", allocated, len(source))
	}
}

func TestUnitSecurityMonotonicLineCursorPreservesPhysicalCRLFAndTokenOffsets(t *testing.T) {
	const header = "\ufeff<?OFX OFXHEADER=\"200\" VERSION=\"202\"?>\r\n<OFX>\r\n<BANKMSGSRSV1><STMTTRNRS><STMTRS><BANKACCTFROM><BANKID>T-BANK</BANKID><ACCTID>synthetic0001</ACCTID><ACCTTYPE>CHECKING</ACCTTYPE></BANKACCTFROM><BANKTRANLIST><DTSTART>20260901</DTSTART><DTEND>20260930</DTEND>\r\n"
	first := `<STMTTRN><DTPOSTED>20260912</DTPOSTED><TRNAMT>1</TRNAMT><FITID>first</FITID><NAME><![CDATA[synthetic` + "\r\n" + `line]]></NAME></STMTTRN>`
	second := `<STMTTRN><DTPOSTED>20260913</DTPOSTED><TRNAMT>2</TRNAMT><FITID>second</FITID><NAME>&amp;lt; &#10;</NAME></STMTTRN>`
	source := header + first + "\r\n<!-- comment\r\n<ignored/> -->\r\n" + second + `</BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>`
	document, err := (Parser{}).Parse(context.Background(), strings.NewReader(source))
	if err != nil || len(document.Lines) != 2 {
		t.Fatal("valid synthetic rows lost", err)
	}
	firstStart := strings.Index(source, "<STMTTRN>")
	secondStart := firstStart + len(first) + strings.Index(source[firstStart+len(first):], "<STMTTRN>")
	for i, start := range []int{firstStart, secondStart} {
		expected := 1 + strings.Count(source[:start], "\n")
		if document.Lines[i].Number != expected {
			t.Fatalf("row %d has physical line %d, expected %d", i, document.Lines[i].Number, expected)
		}
	}
	if document.Lines[1].Description != "&lt; &#10;" {
		t.Fatal("line accounting changed single-pass entity semantics")
	}
}
