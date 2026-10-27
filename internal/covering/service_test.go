package covering

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type txFake struct{ err error }

func (t txFake) Run(ctx context.Context, fn func(context.Context) error) error {
	if t.err != nil {
		return t.err
	}
	return fn(ctx)
}
func (t txFake) Read(ctx context.Context, fn func(context.Context) error) error {
	return t.Run(ctx, fn)
}

type repoFake struct {
	uploads                      []Upload
	pendingErr, lockErr, saveErr error
	saved                        int
	failAt                       int
	casMiss                      bool
}

func (r *repoFake) PendingCoverage(_ context.Context, _ string, limit int) ([]Upload, error) {
	return append([]Upload{}, r.uploads[:min(limit, len(r.uploads))]...), r.pendingErr
}
func (r *repoFake) LockUser(context.Context, string, bool) (string, error) { return "UTC", r.lockErr }
func (r *repoFake) SaveCoverage(_ context.Context, u Upload, p *importing.Period, _ string) (bool, error) {
	if r.saveErr != nil && (r.failAt == 0 || r.saved+1 == r.failAt) {
		return false, r.saveErr
	}
	if p != nil && (p.RawFrom == nil || p.Source != "tags") {
		panic("source metadata lost")
	}
	if r.casMiss {
		return false, nil
	}
	for i, row := range r.uploads {
		if row.ID == u.ID {
			r.uploads = append(r.uploads[:i], r.uploads[i+1:]...)
			break
		}
	}
	r.saved++
	return true, nil
}

type guardFake struct {
	live bool
	err  error
}

func (g *guardFake) Guard(context.Context, string) (bool, error)   { return g.live, g.err }
func (g *guardFake) Deleted(context.Context, string) (bool, error) { return !g.live, g.err }
func (g *guardFake) Purge(ctx context.Context, u string, fn func(context.Context, string) error) error {
	return fn(ctx, u)
}

type bodyFake struct {
	io.Reader
	closeErr error
}

func (b bodyFake) Close() error { return b.closeErr }

type objectFake struct {
	err, closeErr error
	before        func()
	body          string
	opens         int
}

func (o *objectFake) Open(context.Context, string) (io.ReadCloser, error) {
	o.opens++
	if o.before != nil {
		o.before()
	}
	return bodyFake{strings.NewReader(o.body), o.closeErr}, o.err
}

type parserFake struct {
	err     error
	empty   bool
	readErr bool
}

func (parserFake) Validate(io.Reader) error { return nil }
func (p parserFake) Parse(_ context.Context, r io.Reader) (importing.Document, error) {
	if !p.readErr {
		_, _ = io.Copy(io.Discard, r)
	}
	at := time.Date(2026, 9, 1, 0, 30, 0, 0, time.FixedZone("MSK", 3*3600))
	doc := importing.Document{Periods: []importing.Period{{From: at, To: at}}}
	if p.empty {
		doc.Periods = nil
	}
	return doc, p.err
}
func fixture(t *testing.T) (*Service, *repoFake, *objectFake, *guardFake) {
	t.Helper()
	body := "original exact file"
	r := &repoFake{uploads: []Upload{{ID: "one", User: "u", Key: "opaque", Size: int64(len(body)), Digest: sha256.Sum256([]byte(body))}}}
	o := &objectFake{body: body}
	g := &guardFake{live: true}
	s, e := New(txFake{}, r, g, o, parserFake{})
	if e != nil {
		t.Fatal(e)
	}
	return s, r, o, g
}
func TestUnitMetadataRecoveryBoundsAndDurablePerFileProgress(t *testing.T) {
	s, r, o, _ := fixture(t)
	base := r.uploads[0]
	r.uploads = nil
	for i := range 20 {
		u := base
		u.ID = string(rune('a' + i))
		r.uploads = append(r.uploads, u)
	}
	ready, e := s.Recover(t.Context(), "u")
	if e != nil || ready || r.saved != 16 || o.opens != 16 {
		t.Fatal("unbounded/no durable progress", ready, e, r.saved, o.opens)
	}
	ready, e = s.Recover(t.Context(), "u")
	if e != nil || !ready || r.saved != 20 || o.opens != 20 {
		t.Fatal("restart lost remaining metadata", ready, e, r.saved, o.opens)
	}
}
func TestUnitMetadataRecoveryFailureDoesNotGuessOrRewriteData(t *testing.T) {
	boom := errors.New("private provider failure")
	for _, kind := range []string{"object", "close", "parser", "hash", "size", "invalid-size", "pending", "guard", "lock", "save", "transaction", "cancel", "cas", "missing-user"} {
		t.Run(kind, func(t *testing.T) {
			s, r, o, g := fixture(t)
			ctx := t.Context()
			wantErr := true
			switch kind {
			case "object":
				o.err = boom
			case "close":
				o.closeErr = boom
			case "parser":
				s.parser = parserFake{err: boom}
			case "hash":
				r.uploads[0].Digest = [32]byte{}
			case "size":
				r.uploads[0].Size++
			case "invalid-size":
				r.uploads[0].Size = 0
			case "pending":
				r.pendingErr = boom
			case "guard":
				g.err = boom
			case "lock":
				r.lockErr = boom
			case "save":
				r.saveErr = boom
			case "transaction":
				s.tx = txFake{err: boom}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "cas":
				r.casMiss = true
				wantErr = false
			case "missing-user":
				r.lockErr = importing.ErrNotFound
				wantErr = false
			}
			ready, e := s.Recover(ctx, "u")
			if (e != nil) != wantErr || ready {
				t.Fatal("recovery falsely completed", ready, e)
			}
			if kind != "close" && r.saved != 0 {
				t.Fatal("failed input wrote metadata")
			}
			if (kind == "object" || kind == "close" || kind == "parser") && e != nil && strings.Contains(e.Error(), "private provider") {
				t.Fatal("provider details leaked")
			}
		})
	}
}
func TestUnitMetadataRecoveryDeletionDuringReadDoesNotResurrect(t *testing.T) {
	s, r, o, g := fixture(t)
	o.before = func() { g.live = false; r.uploads = nil }
	ready, e := s.Recover(t.Context(), "u")
	if e != nil || !ready || r.saved != 0 {
		t.Fatal("late IO recreated erased upload", ready, e, r.saved)
	}
}
func TestUnitMetadataRecoveryResumesAfterOneCommittedFile(t *testing.T) {
	s, r, o, _ := fixture(t)
	second := r.uploads[0]
	second.ID = "two"
	r.uploads = append(r.uploads, second)
	r.failAt = 2
	r.saveErr = errors.New("commit failed")
	if ready, e := s.Recover(t.Context(), "u"); e == nil || ready || r.saved != 1 || len(r.uploads) != 1 {
		t.Fatal("partial progress not retained", ready, e)
	}
	r.saveErr = nil
	if ready, e := s.Recover(t.Context(), "u"); e != nil || !ready || r.saved != 2 || o.opens != 3 {
		t.Fatal("restart reread already completed file", ready, e, o.opens)
	}
}
func TestUnitMetadataRecoveryRequiresEveryBoundary(t *testing.T) {
	s, _, _, _ := fixture(t)
	for i := range 5 {
		tx, repo, deleted, objects, parser := s.tx, s.repo, s.deleted, s.objects, s.parser
		switch i {
		case 0:
			tx = nil
		case 1:
			repo = nil
		case 2:
			deleted = nil
		case 3:
			objects = nil
		case 4:
			parser = nil
		}
		if v, e := New(tx, repo, deleted, objects, parser); v != nil || e == nil {
			t.Fatal("missing dependency accepted", i)
		}
	}
}

func TestUnitMetadataRecoveryEmptyOriginalHasKnownNoCoverage(t *testing.T) {
	s, r, _, _ := fixture(t)
	s.parser = parserFake{empty: true}
	if ready, e := s.Recover(t.Context(), "u"); e != nil || !ready || r.saved != 1 {
		t.Fatal("empty original not marked known", ready, e)
	}
}
