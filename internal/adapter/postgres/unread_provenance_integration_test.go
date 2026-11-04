package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/ofx"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

const provenancePeriodStart = "<DTSTART>20260901000000[+3:MSK]</DTSTART>"
const provenancePeriodEnd = "<DTEND>20260930235959[+3:MSK]</DTEND>"
const provenanceOperationSource = "operations"
const provenanceTagSource = "tags"
const provenanceFixture = `<?xml version="1.0" encoding="utf-8"?>
<?OFX OFXHEADER="200" VERSION="202"?>
<OFX><BANKMSGSRSV1><STMTTRNRS><STMTRS><BANKACCTFROM><BANKID>T-BANK</BANKID><ACCTID>provenance-account</ACCTID><ACCTTYPE>CHECKING</ACCTTYPE></BANKACCTFROM><BANKTRANLIST>{{period}}
<STMTTRN><DTPOSTED>20260901120000[+3:MSK]</DTPOSTED><TRNAMT>-1</TRNAMT><FITID>first</FITID></STMTTRN>
<STMTTRN><DTPOSTED>20260930120000[+3:MSK]</DTPOSTED><TRNAMT>-1</TRNAMT><FITID>last</FITID></STMTTRN>
{{rows}}</BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>`
const provenanceBrokenRows = `<STMTTRN><DTPOSTED>20260915120000[+3:MSK]</DTPOSTED><TRNAMT>-1</TRNAMT></STMTTRN>
<STMTTRN><TRNAMT>-1</TRNAMT></STMTTRN>
<STMTTRN><DTPOSTED>20260910120000[+3:MSK]</DTPOSTED><TRNAMT>invalid</TRNAMT><FITID>repair</FITID></STMTTRN>`
const provenanceRepairedRow = `<STMTTRN><DTPOSTED>20260910120000[+3:MSK]</DTPOSTED><TRNAMT>-1</TRNAMT><FITID>repair</FITID></STMTTRN>`

func provenanceDocument(t *testing.T, period, rows string) importing.Document {
	t.Helper()
	body := strings.NewReplacer("{{period}}", period, "{{rows}}", rows).Replace(provenanceFixture)
	doc, err := (ofx.Parser{}).Parse(t.Context(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func completeProvenanceDocument(t *testing.T, f *importPG, user string, doc importing.Document) string {
	t.Helper()
	id := uuid.NewString()
	err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if err := f.admitted(ctx, user, id); err != nil {
			return err
		}
		stage, err := f.repo.Stage(ctx, user, id, doc, f.now)
		if err != nil {
			return err
		}
		ops, err := importing.ApplyRules(ctx, stage.Snapshot.Operations, stage.Snapshot.Categories, func() (string, error) { return uuid.NewString(), nil })
		if err != nil {
			return err
		}
		period := importing.SourcePeriod(doc, time.UTC)
		result, err := importing.BuildResult(ctx, importing.ResultInput{Document: doc, Operations: ops, NewIDs: stage.NewIDs, Now: f.now, Zone: time.UTC})
		if err != nil {
			return err
		}
		if err = f.repo.Complete(ctx, user, id, stage, ops, result, period, f.now); err != nil {
			return err
		}
		state, err := f.repo.Snapshot(ctx, user)
		if err != nil {
			return err
		}
		lines, err := importing.ResolveUnread(ctx, state.Uploads, state.Unread, state.Sources, time.UTC)
		if err != nil {
			return err
		}
		return f.repo.SaveUnread(ctx, user, lines)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	return id
}

func TestIntegrationUnreadClosureRetainsSourceRangeProvenance(t *testing.T) {
	f := importRepository(t)
	for _, tc := range []struct {
		name, period, source string
		wantResolved         int
	}{
		{"missing", "", provenanceOperationSource, 1},
		{"start_only", provenancePeriodStart, provenanceOperationSource, 1},
		{"end_only", provenancePeriodEnd, provenanceOperationSource, 1},
		{"both", provenancePeriodStart + provenancePeriodEnd, provenanceTagSource, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := uuid.NewString()
			old := completeProvenanceDocument(t, f, user, provenanceDocument(t, provenancePeriodStart+provenancePeriodEnd, provenanceBrokenRows))
			newer := completeProvenanceDocument(t, f, user, provenanceDocument(t, tc.period, provenanceRepairedRow))
			stored, err := f.repo.Upload(t.Context(), user, newer)
			if err != nil || stored.Period == nil || stored.Period.Source != tc.source {
				t.Fatal("single-upload read lost source proof", stored, err)
			}
			var state importing.Snapshot
			if err := f.db.Read(t.Context(), func(ctx context.Context) error {
				var err error
				state, err = f.repo.Snapshot(ctx, user)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			resolved := 0
			for _, line := range state.Unread {
				if line.UploadID == old && line.ResolvedByUpload == newer && line.Resolution == "read" {
					resolved++
				}
			}
			if resolved != tc.wantResolved {
				t.Fatalf("durable closure count=%d, want %d", resolved, tc.wantResolved)
			}
		})
	}
}

func TestIntegrationLegacyRangeRemainsUnknownUntilSourceRecovery(t *testing.T) {
	f := importRepository(t)
	user := uuid.NewString()
	doc := provenanceDocument(t, provenancePeriodStart+provenancePeriodEnd, provenanceRepairedRow)
	id := completeProvenanceDocument(t, f, user, doc)
	if _, err := f.db.Primary().Exec(t.Context(), `UPDATE upload SET statement_range_source=NULL,statement_from_instant=NULL,statement_to_instant=NULL WHERE user_id=$1 AND id=$2`, user, id); err != nil {
		t.Fatal(err)
	}
	stored, err := f.repo.Upload(t.Context(), user, id)
	if err != nil || stored.Period == nil || stored.Period.Source != "" {
		t.Fatal("legacy dates were misrepresented as tags", stored, err)
	}
	recovery := postgres.NewRecalculations(f.db, 0)
	missing, err := recovery.PendingCoverage(t.Context(), user, 1)
	if err != nil || len(missing) != 1 {
		t.Fatal("missing provenance was not recoverable", missing, err)
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := f.repo.LockUser(ctx, user, false); err != nil {
			return err
		}
		saved, err := recovery.SaveCoverage(ctx, missing[0], importing.SourcePeriod(doc, time.UTC), "UTC")
		if err == nil && !saved {
			t.Error("source recovery was not persisted")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stored, err = f.repo.Upload(t.Context(), user, id)
	if err != nil || stored.Period == nil || stored.Period.Source != provenanceTagSource {
		t.Fatal("recovered tag proof was lost on read", stored, err)
	}
}
