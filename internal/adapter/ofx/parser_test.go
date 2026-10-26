package ofx_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/adapter/ofx"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func statement(rows string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<?OFX OFXHEADER="200" VERSION="202"?>` + "\n<OFX>\n<BANKMSGSRSV1><STMTTRNRS><STMTRS>\n<BANKACCTFROM><BANKID>T-BANK</BANKID><ACCTID>00001111</ACCTID><ACCTTYPE>CHECKING</ACCTTYPE></BANKACCTFROM>\n<BANKTRANLIST><DTSTART>20260101</DTSTART><DTEND>20260131</DTEND>\n" + rows + "\n</BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>"
}

func transaction(extra string) string {
	return "<STMTTRN><DTPOSTED>20260102123456.000[+3:MSK]</DTPOSTED><TRNAMT>1.005</TRNAMT><FITID>synthetic</FITID>" + extra + "</STMTTRN>"
}

func parse(t *testing.T, source string) importing.Document {
	t.Helper()
	p := ofx.Parser{}
	if err := p.Validate(strings.NewReader(source)); err != nil {
		t.Fatal("admission rejected synthetic document", err)
	}
	doc, err := p.Parse(context.Background(), strings.NewReader(source))
	if err != nil {
		t.Fatal("parsing rejected synthetic document", err)
	}
	return doc
}

func TestUnitSourceAdmission(t *testing.T) {
	empty := `<?OFX OFXHEADER="200" VERSION="202"?><OFX><SIGNONMSGSRSV1><SONRS><STATUS><CODE>0</CODE></STATUS></SONRS></SIGNONMSGSRSV1><BANKMSGSRSV1/></OFX>`
	for _, source := range []string{empty, statement(""), "\ufeff" + statement(transaction(""))} {
		parse(t, source)
	}
	for _, tc := range []struct {
		name, source string
		want         error
	}{
		{"csv", "date;amount;description\nsynthetic;1;none", importing.ErrCSVNotAccepted},
		{"pdf", "%PDF-synthetic", importing.ErrWrongFormat},
		{"bank", strings.ReplaceAll(statement(""), "T-BANK", "OTHER"), importing.ErrWrongBank},
		{"missing-bank", strings.ReplaceAll(statement(""), "<BANKID>T-BANK</BANKID>", ""), importing.ErrWrongFormat},
		{"old-sgml", strings.ReplaceAll(statement(""), `OFXHEADER="200"`, `OFXHEADER="100"`), importing.ErrWrongFormat},
		{"wrong-version", strings.ReplaceAll(statement(""), `VERSION="202"`, `VERSION="200"`), importing.ErrWrongFormat},
		{"invalid-utf8", statement("") + string([]byte{0xff}), importing.ErrWrongFormat},
		{"encoding", strings.ReplaceAll(statement(""), "UTF-8", "windows-1251"), importing.ErrWrongFormat},
		{"doctype", strings.ReplaceAll(statement(""), "<OFX>", "<!DOCTYPE OFX><OFX>"), importing.ErrWrongFormat},
		{"outer-root", "<html>" + statement("") + "</html>", importing.ErrWrongFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := (ofx.Parser{}).Validate(strings.NewReader(tc.source)); !errors.Is(err, tc.want) {
				t.Fatal("wrong safe source classification", err)
			}
		})
	}
}

func TestUnitRawTextAndPhysicalLines(t *testing.T) {
	rows := "\n\n" + transaction("<NAME>  &amp;lt; &#171; &lt; &gt; &quot; &apos; &amp; x\u00a0 y  </NAME><MEMO> &amp;amp; </MEMO>")
	source := statement(rows)
	doc := parse(t, source)
	line := doc.Lines[0]
	wantLine := strings.Count(source[:strings.Index(source, "<STMTTRN>")], "\n") + 1
	if line.Number != wantLine || line.Description != "&lt; &#171; < > \" ' & x\u00a0 y" || line.BankCategory != "&amp;" {
		t.Fatal("raw XML text or physical line semantics changed")
	}
	if line.AmountMinor != 101 || line.Direction != "income" || line.Currency != "RUB" || line.OccurredAt == nil || line.OccurredAt.Format(time.RFC3339) != "2026-01-02T12:34:56+03:00" {
		t.Fatal("source money/date/default mapping changed")
	}
	if doc.Accounts[0].Mask == nil || *doc.Accounts[0].Mask != "1111" || doc.Accounts[0].Type != "checking" || len(doc.Periods) != 1 {
		t.Fatal("account or source period mapping changed")
	}
	if got := parse(t, statement(transaction("<NAME><![CDATA[&amp; <literal>]]></NAME>"))).Lines[0].Description; got != "&amp; <literal>" {
		t.Fatal("CDATA literal was unescaped")
	}
	line = parse(t, strings.ReplaceAll(source, ".000[+3:MSK]", ".123456")).Lines[0]
	if line.OccurredAt == nil || line.OccurredAt.Format(time.RFC3339Nano) != "2026-01-02T12:34:56.123456+03:00" {
		t.Fatal("fractional timestamp or default zone lost")
	}
}

func TestUnitMoneyAndUnreadPriority(t *testing.T) {
	for _, tc := range []struct {
		amount string
		minor  int64
		debit  bool
		bad    bool
	}{
		{"1.004", 100, false, false}, {"1.005", 101, false, false}, {"-1.005", 101, true, false},
		{"-0", 0, false, false}, {"-0.001", 0, true, false}, {"92233720368547758.07", 9223372036854775807, false, false},
		{"92233720368547758.075", 0, false, true}, {"NaN", 0, false, true}, {"1e9", 0, false, true}, {"1,2", 0, false, true},
	} {
		source := strings.ReplaceAll(transaction(""), ">1.005<", ">"+tc.amount+"<")
		line := parse(t, statement(source)).Lines[0]
		if tc.bad {
			if line.Reason != "bad_amount" || line.Description != "" {
				t.Fatal("invalid money not classified safely")
			}
		} else if line.Reason != "" || line.AmountMinor != tc.minor || (line.Direction == "expense") != tc.debit {
			t.Fatal("exact money/sign rounding failed")
		}
	}
	for _, tc := range []struct{ fields, reason string }{
		{"<TRNAMT>bad</TRNAMT>", "no_date"},
		{"<DTPOSTED></DTPOSTED><TRNAMT>bad</TRNAMT>", "bad_date"},
		{"<DTPOSTED>20260230</DTPOSTED><TRNAMT>1</TRNAMT>", "bad_date"},
		{"<DTPOSTED>20260102[+99:X]</DTPOSTED><TRNAMT>1</TRNAMT>", "bad_date"},
		{"<DTPOSTED>20260102</DTPOSTED><TRNAMT>bad</TRNAMT>", "bad_amount"},
		{"<DTPOSTED>20260102</DTPOSTED><TRNAMT>1</TRNAMT>", "truncated"},
	} {
		line := parse(t, statement("<STMTTRN>"+tc.fields+"<NAME>private synthetic</NAME></STMTTRN>")).Lines[0]
		if line.Reason != tc.reason || line.Description != "" {
			t.Fatal("unread precedence or safe metadata failed")
		}
	}
	line := parse(t, statement(transaction("<TRNTYPE>DEBIT</TRNTYPE>"))).Lines[0]
	if line.Direction != "expense" {
		t.Fatal("DEBIT flag did not override positive amount")
	}
}

func TestUnitAccountSkipAndOtherBank(t *testing.T) {
	source := strings.ReplaceAll(statement(transaction("")), "<ACCTID>00001111</ACCTID>", "")
	doc := parse(t, source)
	if len(doc.Accounts)+len(doc.Lines)+len(doc.Periods) != 0 {
		t.Fatal("missing account ID did not skip entire statement")
	}
	doc = parse(t, strings.ReplaceAll(statement(transaction("")), "00001111", "OB$synthetic"))
	if !doc.Accounts[0].OtherBank || doc.Accounts[0].Mask != nil {
		t.Fatal("external account identity mapping failed")
	}
	source = strings.ReplaceAll(statement(transaction("")), "00001111", "x")
	source = strings.ReplaceAll(source, "CHECKING", "Future-Account-Type")
	doc = parse(t, source)
	if doc.Accounts[0].Key != "x" || doc.Accounts[0].Type != "future-account-type" || doc.Accounts[0].Mask == nil || *doc.Accounts[0].Mask != "x" {
		t.Fatal("opaque short identity or unknown account type rejected")
	}
}

func TestUnitParserResourceBudgets(t *testing.T) {
	p := ofx.Parser{}
	// Five existing ancestor elements plus 59 extension elements reach depth 64.
	parse(t, statement(strings.Repeat("<X>", 59)+strings.Repeat("</X>", 59)))
	deep := statement(strings.Repeat("<X>", 60) + strings.Repeat("</X>", 60))
	if _, err := p.Parse(context.Background(), strings.NewReader(deep)); err == nil {
		t.Fatal("excessive XML nesting accepted")
	}
	const budget = 10 << 20
	valid := statement("")
	if err := p.Validate(strings.NewReader(valid + strings.Repeat(" ", budget-len(valid)))); err != nil {
		t.Fatal("exact input budget rejected", err)
	}
	for _, background := range []bool{false, true} {
		r := &countingReader{Reader: strings.NewReader(strings.Repeat(" ", budget+1000))}
		var err error
		if background {
			_, err = p.Parse(context.Background(), r)
		} else {
			err = p.Validate(r)
		}
		var refusal importing.Refusal
		if err == nil || errors.As(err, &refusal) || r.read != budget+1 {
			t.Fatal("input resource ceiling not bounded independently of transport refusal", r.read, err)
		}
	}
}

type countingReader struct {
	*strings.Reader
	read int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func TestUnitFieldsRemainInTheirSourceContainers(t *testing.T) {
	// An extension cannot supply the ACCTID missing from BANKACCTFROM.
	source := strings.ReplaceAll(statement(transaction("")), "<ACCTID>00001111</ACCTID>", "")
	source = strings.ReplaceAll(source, "</BANKTRANLIST>", "</BANKTRANLIST><EXT><ACCTID>99992222</ACCTID></EXT>")
	if doc := parse(t, source); len(doc.Accounts)+len(doc.Lines)+len(doc.Periods) != 0 {
		t.Fatal("extension account ID bypassed missing-account skip")
	}
	source = statement(transaction("<EXT><FITID>unrelated</FITID><NAME>unrelated</NAME><CURSYM>USD</CURSYM></EXT>"))
	source = strings.ReplaceAll(source, "</BANKTRANLIST>", "</BANKTRANLIST><EXT><DTSTART>19990101</DTSTART><DTEND>19990131</DTEND></EXT>")
	doc := parse(t, source)
	if doc.Lines[0].FITID != "synthetic" || doc.Lines[0].Description != "" || doc.Lines[0].Currency != "RUB" || doc.Periods[0].From.Year() != 2026 {
		t.Fatal("extension fields changed transaction or source period")
	}
	// A transaction outside BANKTRANLIST must not become an accepted source row.
	source = strings.ReplaceAll(statement(""), "</STMTRS>", transaction("")+"</STMTRS>")
	if _, err := (ofx.Parser{}).Parse(context.Background(), strings.NewReader(source)); err == nil {
		t.Fatal("misplaced transaction accepted")
	}
}

func TestUnitMalformedTailAndCancellation(t *testing.T) {
	p := ofx.Parser{}
	valid := statement(transaction(""))
	for _, source := range []string{
		strings.ReplaceAll(valid, "</STMTTRN>", ""), strings.TrimSuffix(valid, "</OFX>"), valid + "<OFX/>",
	} {
		if err := p.Validate(strings.NewReader(source)); err != nil {
			t.Fatal("operation structure was parsed during admission")
		}
		if _, err := p.Parse(context.Background(), strings.NewReader(source)); err == nil || strings.Contains(err.Error(), "synthetic") {
			t.Fatal("malformed XML silently accepted or leaked source")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Parse(ctx, strings.NewReader(valid)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
	if _, err := p.Parse(context.Background(), failingReader{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("reader failure ignored")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestIntegrationPublishedCalculationFixtures(t *testing.T) {
	// Copies from contracts 471482b fixtures/calc; only aggregate facts are printed on failure.
	for _, tc := range []struct {
		file            string
		accounts, valid int
		unread          map[string]int
	}{
		{"00-2025-10-15_2025-10-31.ofx", 3, 76, nil},
		{"01-2025-10-15_2026-06-30.ofx", 3, 1261, nil},
		{"02-2026-06-20_2026-08-07.ofx", 3, 342, nil},
		{"03-2026-08-20_2026-09-25.ofx", 3, 261, nil},
		{"04-edge-cases-2026-09.ofx", 1, 8, map[string]int{"bad_amount": 1, "truncated": 1, "no_date": 1}},
		{"05-edge-cases-fixed-2026-09.ofx", 1, 10, nil},
		{"06-empty-2026-09.ofx", 1, 0, nil},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + tc.file)
			if err != nil {
				t.Fatal(err)
			}
			doc := parse(t, string(data))
			valid, unread := 0, map[string]int{}
			for _, line := range doc.Lines {
				if line.Reason == "" {
					valid++
				} else {
					unread[line.Reason]++
				}
			}
			if len(doc.Accounts) != tc.accounts || len(doc.Periods) != tc.accounts || valid != tc.valid || len(unread) != len(tc.unread) {
				t.Fatal("fixture aggregate mismatch", len(doc.Accounts), len(doc.Periods), valid, unread)
			}
			for reason, count := range tc.unread {
				if unread[reason] != count {
					t.Fatal("fixture unread aggregate mismatch", reason, unread[reason])
				}
			}
		})
	}
}
