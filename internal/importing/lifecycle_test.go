package importing

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const lifeUser = "018f0000-0000-7000-8000-000000000001"
const lifeUpload = "018f0000-0000-7000-8000-000000000002"

type lifeTxnKey struct{}
type lifeTrace struct {
	mu     sync.Mutex
	events []string
}

func (l *lifeTrace) add(s string) { l.mu.Lock(); defer l.mu.Unlock(); l.events = append(l.events, s) }
func (l *lifeTrace) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.events {
		if e == s {
			n++
		}
	}
	return n
}

type lifeTx struct {
	trace         *lifeTrace
	before, after error
}

func (tx *lifeTx) Run(ctx context.Context, fn func(context.Context) error) error {
	if tx.before != nil {
		return tx.before
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx.trace.add("tx")
	if err := fn(context.WithValue(ctx, lifeTxnKey{}, true)); err != nil {
		return err
	}
	if tx.after != nil {
		return tx.after
	}
	tx.trace.add("commit")
	return nil
}
func (tx *lifeTx) Read(ctx context.Context, fn func(context.Context) error) error {
	return tx.Run(ctx, fn)
}

type lifeRepo struct {
	Repository
	trace         *lifeTrace
	errs          map[string]error
	upload        func() (Upload, error)
	state         Snapshot
	stage         Stage
	result        Result
	completed     *Period
	onFail        func()
	onFileDeleted func()
	changed       bool
	keys          []string
	remove        []Operation
	pending       func(context.Context) ([]Upload, error)
	cleanup       func(context.Context) ([]Upload, error)
	referenced    bool
}

func (r *lifeRepo) event(name string) error { r.trace.add(name); return r.errs[name] }
func (r *lifeRepo) LockUser(context.Context, string, bool) (string, error) {
	return "UTC", r.event("lock")
}
func (r *lifeRepo) Upload(context.Context, string, string) (Upload, error) {
	if err := r.event("upload"); err != nil {
		return Upload{}, err
	}
	return r.upload()
}
func (r *lifeRepo) Snapshot(context.Context, string) (Snapshot, error) {
	return r.state, r.event("snapshot")
}
func (r *lifeRepo) TimeZone(context.Context, string) (string, error) {
	return r.state.TimeZone, r.event("zone")
}
func (r *lifeRepo) Stage(context.Context, string, string, Document, time.Time) (Stage, error) {
	return r.stage, r.event("stage")
}
func (r *lifeRepo) Complete(_ context.Context, _, _ string, _ Stage, _ []Operation, result Result, p *Period, _ time.Time) error {
	r.result = result
	r.completed = p
	return r.event("complete")
}
func (r *lifeRepo) SaveUnread(context.Context, string, []Unread) error { return r.event("unread") }
func (r *lifeRepo) Progress(_ context.Context, _, _ string, p int) error {
	if p != 20 && p != 60 {
		panic("unexpected processing phase")
	}
	return r.event("progress")
}
func (r *lifeRepo) Fail(context.Context, string, string, string, time.Time) (bool, error) {
	if r.onFail != nil {
		r.onFail()
	}
	return r.changed, r.event("fail")
}
func (r *lifeRepo) FileDeleted(context.Context, string, string, time.Time) error {
	if r.onFileDeleted != nil {
		r.onFileDeleted()
	}
	return r.event("file-deleted")
}
func (r *lifeRepo) AbandonStaging(context.Context, string, string, time.Time) error {
	return r.event("abandon")
}
func (r *lifeRepo) Remove(context.Context, string, string, time.Time) ([]Operation, error) {
	return r.remove, r.event("remove")
}
func (r *lifeRepo) SaveRules(context.Context, string, []Operation, time.Time) error {
	return r.event("rules")
}
func (r *lifeRepo) SetTimezone(context.Context, string, string, int32, time.Time) error {
	return r.event("set-zone")
}
func (r *lifeRepo) IntentKeys(context.Context, string) ([]string, error) {
	return r.keys, r.event("intents")
}
func (r *lifeRepo) MarkFenced(context.Context, string, string, time.Time) error {
	return r.event("confirmed")
}
func (r *lifeRepo) Purge(context.Context, string) error { return r.event("purge-db") }
func (r *lifeRepo) Referenced(context.Context, string, string) (bool, error) {
	return r.referenced, r.event("reference")
}
func (r *lifeRepo) ReferencedBatch(_ context.Context, keys []string) (map[string]bool, error) {
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		out[key] = r.referenced
	}
	return out, r.event("reference-batch")
}
func (r *lifeRepo) Pending(ctx context.Context, _ int) ([]Upload, error) {
	if r.pending != nil {
		return r.pending(ctx)
	}
	return nil, r.event("pending")
}
func (r *lifeRepo) Cleanup(ctx context.Context, _ int, _ time.Time) ([]Upload, error) {
	if r.cleanup != nil {
		return r.cleanup(ctx)
	}
	return nil, r.event("cleanup")
}

type lifeObjects struct {
	ObjectStore
	t                            *testing.T
	trace                        *lifeTrace
	body                         string
	openErr, deleteErr, closeErr error
	list                         func(string, string) (ObjectPage, error)
	opened                       func(context.Context)
}

func (o *lifeObjects) outside(ctx context.Context) {
	o.t.Helper()
	if ctx.Value(lifeTxnKey{}) == true {
		o.t.Error("remote object I/O inside transaction")
	}
}

type lifeBody struct {
	io.Reader
	closed func() error
}

func (b *lifeBody) Close() error { return b.closed() }
func (o *lifeObjects) Open(ctx context.Context, _ string) (io.ReadCloser, error) {
	o.outside(ctx)
	o.trace.add("open")
	if o.opened != nil {
		o.opened(ctx)
	}
	if o.openErr != nil {
		return nil, o.openErr
	}
	return &lifeBody{Reader: strings.NewReader(o.body), closed: func() error { o.trace.add("close"); return o.closeErr }}, nil
}
func (o *lifeObjects) Delete(ctx context.Context, _ string) error {
	o.outside(ctx)
	o.trace.add("fence")
	return o.deleteErr
}
func (o *lifeObjects) List(ctx context.Context, prefix, cursor string) (ObjectPage, error) {
	o.outside(ctx)
	o.trace.add("list")
	if o.list != nil {
		return o.list(prefix, cursor)
	}
	return ObjectPage{}, nil
}

type lifeParser struct {
	trace      *lifeTrace
	doc        Document
	err        error
	panicValue any
}

func (p *lifeParser) Validate(io.Reader) error { return nil }
func (p *lifeParser) Parse(_ context.Context, r io.Reader) (Document, error) {
	p.trace.add("parse")
	if p.panicValue != nil {
		panic(p.panicValue)
	}
	_, err := io.Copy(io.Discard, r)
	if err != nil {
		return Document{}, err
	}
	return p.doc, p.err
}

type lifeDeleted struct {
	trace *lifeTrace
	alive bool
	err   error
}

func (d *lifeDeleted) Deleted(ctx context.Context, _ string) (bool, error) {
	d.trace.add("deleted")
	if ctx.Value(lifeTxnKey{}) != nil {
		return false, errors.New("watch admission inside a data transaction")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return !d.alive, d.err
}
func (d *lifeDeleted) Guard(context.Context, string) (bool, error) {
	d.trace.add("guard")
	return d.alive, d.err
}
func (d *lifeDeleted) Purge(ctx context.Context, user string, fn func(context.Context, string) error) error {
	d.trace.add("retire")
	if d.err != nil {
		return d.err
	}
	return fn(ctx, user)
}

type lifeBarrier struct {
	trace          *lifeTrace
	err, unlockErr error
}

func (b *lifeBarrier) Shared(context.Context, string) (func() error, error) {
	b.trace.add("shared")
	return func() error { b.trace.add("unlock"); return b.unlockErr }, b.err
}
func (b *lifeBarrier) Exclusive(context.Context, string) (func() error, error) {
	b.trace.add("exclusive")
	return func() error { b.trace.add("unlock"); return b.unlockErr }, b.err
}

type lifeFixture struct {
	s       *Service
	repo    *lifeRepo
	tx      *lifeTx
	objects *lifeObjects
	parser  *lifeParser
	deleted *lifeDeleted
	barrier *lifeBarrier
	trace   *lifeTrace
	now     time.Time
	upload  Upload
}

func newLife(t *testing.T) *lifeFixture {
	t.Helper()
	f := &lifeFixture{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), trace: &lifeTrace{}}
	f.upload = Upload{ID: lifeUpload, UserID: lifeUser, Status: "processing", UploadedAt: f.now.Add(-time.Second), ObjectKey: lifeUser + "/" + lifeUpload, SizeBytes: 7, Digest: sha256.Sum256([]byte("fixture"))}
	op := ruleOperation("op", 1, "expense", f.now.Add(-24*time.Hour))
	op.BankCategory = "shop"
	at := op.At
	doc := Document{Lines: []Line{{AccountKey: op.AccountKey, FITID: op.FITID, Number: 1, OccurredAt: &at, AmountMinor: 100, Currency: "RUB", Direction: "expense"}}}
	f.repo = &lifeRepo{trace: f.trace, errs: make(map[string]error), changed: true, state: Snapshot{TimeZone: "UTC", Categories: ruleCategories()}, stage: Stage{Snapshot: Snapshot{TimeZone: "UTC", Categories: ruleCategories(), Operations: []Operation{op}}, NewIDs: map[string]bool{op.ID: true}}}
	f.repo.upload = func() (Upload, error) { return f.upload, nil }
	f.tx = &lifeTx{trace: f.trace}
	f.objects = &lifeObjects{t: t, trace: f.trace, body: "fixture"}
	f.parser = &lifeParser{trace: f.trace, doc: doc}
	f.deleted = &lifeDeleted{trace: f.trace, alive: true}
	f.barrier = &lifeBarrier{trace: f.trace}
	f.s = &Service{tx: f.tx, repo: f.repo, objects: f.objects, parser: f.parser, deleted: f.deleted, barrier: f.barrier, now: func() time.Time { return f.now }, newID: func() (string, error) { return "pair", nil }}
	return f
}

func TestUnitProcessPublishesOnlyVerifiedSourceFacts(t *testing.T) {
	f := newLife(t)
	if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err != nil {
		t.Fatal(err)
	}
	if f.trace.count("stage") != 1 || f.trace.count("complete") != 1 || f.trace.count("unread") != 1 || f.trace.count("close") != 1 || f.trace.count("fail") != 0 {
		t.Fatal("incomplete atomic processing", f.trace)
	}
	if f.repo.result.NewCount != 1 || f.repo.result.OperationCount != 1 || f.repo.result.Totals == nil || f.repo.result.Totals.ExpenseMinor != 100 || f.repo.completed == nil {
		t.Fatal("result not computed from staged facts")
	}
	for _, change := range []func(*lifeFixture){
		func(f *lifeFixture) { f.upload.Digest = sha256.Sum256([]byte("different")) },
		func(f *lifeFixture) { f.upload.SizeBytes++ },
		func(f *lifeFixture) { f.parser.err = errors.New("invalid source") },
		func(f *lifeFixture) { f.objects.closeErr = errors.New("close failed") },
		func(f *lifeFixture) { f.objects.openErr = errors.New("storage unavailable") },
	} {
		f := newLife(t)
		change(f)
		f.repo.onFail = func() { f.upload.Status = "failed"; f.upload.ErrorCode = "processing_failed" }
		if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err != nil {
			t.Fatal(err)
		}
		if f.trace.count("stage") != 0 || f.trace.count("complete") != 0 || f.trace.count("fail") != 1 || f.trace.count("fence") != 1 || f.trace.count("file-deleted") != 1 {
			t.Fatal("unverified source published or not erased", f.trace)
		}
	}
}

func TestUnitProcessResumesOnlyActiveJobsAndHonorsStop(t *testing.T) {
	for _, status := range []string{"done", "failed", "deleted", "staging"} {
		f := newLife(t)
		f.upload.Status = status
		if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err != nil || f.trace.count("open") != 0 {
			t.Fatal(status, err)
		}
	}
	f := newLife(t)
	f.repo.errs["upload"] = ErrNotFound
	if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err != nil {
		t.Fatal(err)
	}
	f = newLife(t)
	failure := errors.New("read failure")
	f.repo.errs["upload"] = failure
	if err := f.s.Process(t.Context(), lifeUser, lifeUpload); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	f = newLife(t)
	f.upload.UploadedAt = f.now.Add(-4 * time.Minute)
	f.repo.onFail = func() { f.upload.Status = "failed" }
	if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err != nil || f.trace.count("open") != 0 || f.trace.count("fence") != 1 {
		t.Fatal("stale job not failed before parse", err)
	}
	f = newLife(t)
	f.deleted.alive = false
	if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err != nil || f.trace.count("open") != 0 {
		t.Fatal("retired user processed", err)
	}
	f = newLife(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.objects.opened = func(context.Context) { cancel() }
	f.parser.err = context.Canceled
	if err := f.s.Process(ctx, lifeUser, lifeUpload); !errors.Is(err, context.Canceled) || f.trace.count("fail") != 0 {
		t.Fatal("pod stop erased recoverable job", err)
	}
}

func TestUnitProcessingFailureAndTransactionErrorsStayConservative(t *testing.T) {
	for _, point := range []string{"stage", "complete", "unread", "snapshot"} {
		f := newLife(t)
		f.repo.errs[point] = errors.New("storage failure")
		f.repo.onFail = func() { f.upload.Status = "failed" }
		if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err != nil || f.trace.count("fail") != 1 || f.trace.count("fence") != 1 {
			t.Fatal(point, err, f.trace)
		}
	}
	f := newLife(t)
	f.parser.err = errors.New("parse failure")
	f.repo.changed = false
	if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err != nil || f.trace.count("fence") != 0 {
		t.Fatal("unchanged failed-row guard ignored", err)
	}
	f = newLife(t)
	f.parser.err = errors.New("parse failure")
	f.repo.errs["fail"] = errors.New("unknown fail commit")
	if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err == nil || f.trace.count("fence") != 0 {
		t.Fatal("erased file after unconfirmed failure", err)
	}
	f = newLife(t)
	f.repo.stage.Snapshot.TimeZone = "invalid-zone"
	f.repo.onFail = func() { f.upload.Status = "failed" }
	if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err != nil || f.trace.count("complete") != 0 || f.trace.count("fence") != 1 {
		t.Fatal("invalid calendar published", err)
	}
	f = newLife(t)
	f.repo.stage.Snapshot.Operations[0].BankCategory = "unknown"
	f.repo.onFail = func() { f.upload.Status = "failed" }
	if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err != nil || f.trace.count("complete") != 0 {
		t.Fatal("invalid accounting published", err)
	}
}

func TestUnitWatchReconnectsAndConfirmsErasureBeforeTerminal(t *testing.T) {
	for _, reason := range []string{"cancelled", "processing_failed"} {
		f := newLife(t)
		f.upload.Status = "failed"
		f.upload.ErrorCode = reason
		f.repo.onFileDeleted = func() { f.upload.FileDeletedAt = &f.now }
		var received []Event
		err := f.s.Watch(t.Context(), lifeUser, lifeUpload, func(e Event) error {
			if f.trace.count("fence") != 1 || f.trace.count("file-deleted") != 1 {
				t.Error("terminal before confirmed fence")
			}
			received = append(received, e)
			return nil
		})
		if err != nil || len(received) != 1 || received[0].Error != reason || received[0].TimeZone != "UTC" {
			t.Fatal(err, received)
		}
	}
	f := newLife(t)
	f.upload.Status = "done"
	f.upload.Result = &Result{NewCount: 7}
	if err := f.s.Watch(t.Context(), lifeUser, lifeUpload, func(e Event) error {
		if e.Result != f.upload.Result || e.Error != "" {
			t.Error("lost saved result")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"staging", "deleted", "invalid", "done", "failed"} {
		f := newLife(t)
		f.upload.Status = state
		f.upload.FileDeletedAt = &f.now
		f.upload.ErrorCode = "private-invalid"
		err := f.s.Watch(t.Context(), lifeUser, lifeUpload, func(Event) error { t.Error("invalid terminal published"); return nil })
		if state == "staging" || state == "deleted" {
			if !errors.Is(err, ErrNotFound) {
				t.Fatal(state, err)
			}
		} else if err == nil {
			t.Fatal(state, "accepted invalid durable result")
		}
	}
	f = newLife(t)
	f.upload.Status = "failed"
	f.objects.deleteErr = errors.New("cloud unknown")
	if err := f.s.Watch(t.Context(), lifeUser, lifeUpload, func(Event) error { t.Error("unconfirmed failure exposed"); return nil }); err == nil || f.trace.count("file-deleted") != 0 {
		t.Fatal(err)
	}
}

func TestUnitWatchProgressHonorsCancellationAndSendFailure(t *testing.T) {
	f := newLife(t)
	f.upload.Percent = 42
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	err := f.s.Watch(ctx, lifeUser, lifeUpload, func(e Event) error {
		calls++
		if e.Percent != 42 || e.TimeZone != "UTC" || e.Result != nil {
			t.Error("bad current progress")
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal(err, calls)
	}
	f = newLife(t)
	failure := errors.New("stream closed")
	if err = f.s.Watch(t.Context(), lifeUser, lifeUpload, func(Event) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	for _, point := range []string{"upload", "zone"} {
		f = newLife(t)
		f.repo.errs[point] = failure
		if err = f.s.Watch(t.Context(), lifeUser, lifeUpload, func(Event) error { t.Error("read error emitted event"); return nil }); !errors.Is(err, failure) {
			t.Fatal(point, err)
		}
	}
	f = newLife(t)
	f.upload.UploadedAt = f.now.Add(-3 * time.Minute)
	f.repo.onFail = func() { f.upload.Status = "failed"; f.upload.ErrorCode = "processing_failed" }
	f.repo.onFileDeleted = func() { f.upload.FileDeletedAt = &f.now }
	if err = f.s.Watch(t.Context(), lifeUser, lifeUpload, func(e Event) error {
		if e.Error != "processing_failed" {
			t.Error("stale job terminal missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnitDeleteCommitsCancellationOrSourceRemovalBeforeFence(t *testing.T) {
	for _, state := range []string{"processing", "done", "failed", "deleted"} {
		f := newLife(t)
		f.upload.Status = state
		f.repo.onFail = func() { f.upload.Status = "failed"; f.upload.ErrorCode = "cancelled" }
		// Repository removal is already integration-tested; this port response models
		// the terminal row observed by the subsequent cleanup transaction.
		if state == "done" {
			reads := 0
			f.repo.upload = func() (Upload, error) {
				reads++
				u := f.upload
				if reads > 1 {
					u.Status = "deleted"
				}
				return u, nil
			}
		}
		if err := f.s.Delete(t.Context(), lifeUser, lifeUpload); err != nil {
			t.Fatal(state, err)
		}
		if f.trace.count("fence") != 1 || f.trace.count("file-deleted") != 1 || f.trace.count("shared") != 1 || f.trace.count("unlock") != 1 {
			t.Fatal(state, f.trace)
		}
		if state == "processing" && f.trace.count("fail") != 1 {
			t.Fatal("cancellation not persisted")
		}
		if state == "done" && (f.trace.count("remove") != 1 || f.trace.count("rules") != 1 || f.trace.count("unread") != 1) {
			t.Fatal("source/rules/unread not recomputed")
		}
	}
	f := newLife(t)
	f.upload.Status = "staging"
	if err := f.s.Delete(t.Context(), lifeUser, lifeUpload); !errors.Is(err, ErrNotFound) || f.trace.count("fence") != 0 {
		t.Fatal(err)
	}
	f = newLife(t)
	f.upload.Status = "deleted"
	f.upload.FileDeletedAt = &f.now
	if err := f.s.Delete(t.Context(), lifeUser, lifeUpload); err != nil || f.trace.count("fence") != 0 {
		t.Fatal("repeated deletion erased fence", err)
	}
}

func TestUnitDeleteErrorsNeverAcknowledgeUnconfirmedErasure(t *testing.T) {
	failure := errors.New("private storage error")
	for _, point := range []string{"upload", "lock", "snapshot", "remove", "rules", "unread"} {
		f := newLife(t)
		f.upload.Status = "done"
		f.repo.errs[point] = failure
		if err := f.s.Delete(t.Context(), lifeUser, lifeUpload); !errors.Is(err, failure) || f.trace.count("fence") != 0 || f.trace.count("unlock") != 1 {
			t.Fatal(point, err, f.trace)
		}
	}
	f := newLife(t)
	f.repo.onFail = func() { f.upload.Status = "failed" }
	f.tx.after = failure
	if err := f.s.Delete(t.Context(), lifeUser, lifeUpload); !errors.Is(err, failure) || f.trace.count("fence") != 0 {
		t.Fatal("unknown commit fenced", err)
	}
	f = newLife(t)
	f.upload.Status = "failed"
	f.objects.deleteErr = failure
	if err := f.s.Delete(t.Context(), lifeUser, lifeUpload); !errors.Is(err, failure) || f.trace.count("file-deleted") != 0 {
		t.Fatal(err)
	}
	f = newLife(t)
	f.upload.Status = "deleted"
	f.upload.FileDeletedAt = &f.now
	f.barrier.unlockErr = failure
	if err := f.s.Delete(t.Context(), lifeUser, lifeUpload); !errors.Is(err, failure) {
		t.Fatal("unlock uncertainty hidden", err)
	}
	f = newLife(t)
	f.barrier.err = failure
	if err := f.s.Delete(t.Context(), lifeUser, lifeUpload); !errors.Is(err, failure) || f.trace.count("tx") != 0 {
		t.Fatal(err)
	}
	f = newLife(t)
	f.deleted.alive = false
	if err := f.s.Delete(t.Context(), lifeUser, lifeUpload); !errors.Is(err, ErrDeleted) || f.trace.count("fence") != 0 {
		t.Fatal("retired user mutated", err)
	}
}

func TestUnitCleanupStagingRequiresCommitAndConfirmedCloudFence(t *testing.T) {
	f := newLife(t)
	f.upload.Status = "staging"
	if err := f.s.cleanupFile(t.Context(), lifeUser, lifeUpload); err != nil || f.trace.count("abandon") != 1 || f.trace.count("fence") != 1 || f.trace.count("file-deleted") != 1 {
		t.Fatal(err, f.trace)
	}
	failure := errors.New("unknown commit")
	for _, point := range []string{"abandon", "upload", "lock"} {
		f = newLife(t)
		f.upload.Status = "staging"
		f.repo.errs[point] = failure
		if err := f.s.cleanupFile(t.Context(), lifeUser, lifeUpload); !errors.Is(err, failure) || f.trace.count("fence") != 0 {
			t.Fatal(point, err)
		}
	}
	f = newLife(t)
	f.upload.Status = "staging"
	f.tx.after = failure
	if err := f.s.cleanupFile(t.Context(), lifeUser, lifeUpload); !errors.Is(err, failure) || f.trace.count("fence") != 0 {
		t.Fatal("uncommitted abandonment fenced", err)
	}
	f = newLife(t)
	f.upload.Status = "failed"
	f.repo.errs["file-deleted"] = failure
	if err := f.s.cleanupFile(t.Context(), lifeUser, lifeUpload); !errors.Is(err, failure) || f.trace.count("fence") != 1 {
		t.Fatal(err)
	}
}

func TestUnitTimezoneValidationAndRetirementDoNotResurrectUser(t *testing.T) {
	for _, zone := range []string{"", "Local", "../UTC", "/UTC", "invalid-zone"} {
		f := newLife(t)
		var input *InputError
		if err := f.s.SetTimezone(t.Context(), lifeUser, zone, 3); !errors.As(err, &input) || f.trace.count("tx") != 0 {
			t.Fatal(zone, err)
		}
	}
	f := newLife(t)
	if err := f.s.SetTimezone(t.Context(), lifeUser, "Europe/Moscow", 3); err != nil || f.trace.count("set-zone") != 1 || f.trace.count("lock") != 1 {
		t.Fatal(err)
	}
	f = newLife(t)
	f.deleted.alive = false
	if err := f.s.SetTimezone(t.Context(), lifeUser, "UTC", 3); err != nil || f.trace.count("set-zone") != 0 || f.trace.count("lock") != 0 {
		t.Fatal(err)
	}
	failure := errors.New("DB unavailable")
	f = newLife(t)
	f.deleted.err = failure
	if err := f.s.SetTimezone(t.Context(), lifeUser, "UTC", 3); !errors.Is(err, failure) || f.trace.count("lock") != 0 {
		t.Fatal(err)
	}
	for _, point := range []string{"lock", "set-zone"} {
		f = newLife(t)
		f.repo.errs[point] = failure
		if err := f.s.SetTimezone(t.Context(), lifeUser, "UTC", 3); !errors.Is(err, failure) {
			t.Fatal(point, err)
		}
	}
}

func TestUnitPurgeRetiresThenFencesAllPagesBeforeDeletingDatabase(t *testing.T) {
	f := newLife(t)
	f.repo.keys = []string{f.upload.ObjectKey}
	lists := 0
	f.objects.list = func(prefix, cursor string) (ObjectPage, error) {
		if prefix != lifeUser+"/" || f.trace.count("exclusive") != 1 {
			t.Error("wrong private scope")
		}
		lists++
		switch lists {
		case 1:
			if cursor != "" {
				t.Error(cursor)
			}
			return ObjectPage{Next: "empty-page"}, nil
		case 2:
			if cursor != "empty-page" {
				t.Error(cursor)
			}
			return ObjectPage{Objects: []Object{{Key: lifeUser + "/018f0000-0000-7000-8000-000000000003"}}}, nil
		default:
			t.Error("extra page")
			return ObjectPage{}, nil
		}
	}
	if err := f.s.Purge(t.Context(), strings.ToUpper(lifeUser)); err != nil {
		t.Fatal(err)
	}
	if lists != 2 || f.trace.count("fence") != 2 || f.trace.count("confirmed") != 2 || f.trace.count("purge-db") != 1 || f.trace.count("guard") != 0 || f.trace.count("unlock") != 1 {
		t.Fatal(f.trace)
	}
	positions := map[string]int{}
	for i, e := range f.trace.events {
		if _, exists := positions[e]; !exists {
			positions[e] = i
		}
	}
	order := []string{"retire", "commit", "exclusive", "intents", "fence", "confirmed", "purge-db"}
	for _, event := range order {
		if _, present := positions[event]; !present {
			t.Fatal("missing required purge phase", event, f.trace)
		}
	}
	for i := 1; i < len(order); i++ {
		if positions[order[i-1]] >= positions[order[i]] {
			t.Fatal("unsafe purge phase order", f.trace)
		}
	}
}

func TestUnitPurgeStopsBeforeDatabaseDeletionOnEveryUncertainPhase(t *testing.T) {
	failure := errors.New("private provider error")
	changes := []func(*lifeFixture){
		func(f *lifeFixture) { f.tx.after = failure },
		func(f *lifeFixture) { f.deleted.err = failure },
		func(f *lifeFixture) { f.barrier.err = failure },
		func(f *lifeFixture) { f.repo.errs["intents"] = failure },
		func(f *lifeFixture) { f.repo.keys = []string{"another-user/" + lifeUpload} },
		func(f *lifeFixture) { f.repo.keys = []string{f.upload.ObjectKey}; f.objects.deleteErr = failure },
		func(f *lifeFixture) { f.repo.keys = []string{f.upload.ObjectKey}; f.repo.errs["confirmed"] = failure },
		func(f *lifeFixture) {
			f.objects.list = func(string, string) (ObjectPage, error) { return ObjectPage{}, failure }
		},
		func(f *lifeFixture) {
			f.objects.list = func(string, string) (ObjectPage, error) {
				return ObjectPage{Objects: []Object{{Key: lifeUser + "/../escape"}}}, nil
			}
		},
		func(f *lifeFixture) {
			f.objects.list = func(string, string) (ObjectPage, error) { return ObjectPage{Next: "cycle"}, nil }
		},
	}
	for i, change := range changes {
		f := newLife(t)
		change(f)
		if err := f.s.Purge(t.Context(), lifeUser); err == nil || f.trace.count("purge-db") != 0 {
			t.Fatal(i, err, f.trace)
		}
	}
	for _, user := range []string{"", "invalid", "00000000-0000-0000-0000-000000000000"} {
		f := newLife(t)
		if err := f.s.Purge(t.Context(), user); !errors.Is(err, ErrInput) || f.trace.count("tx") != 0 {
			t.Fatal(user, err)
		}
	}
	f := newLife(t)
	f.repo.errs["purge-db"] = failure
	if err := f.s.Purge(t.Context(), lifeUser); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	f = newLife(t)
	f.barrier.unlockErr = failure
	if err := f.s.Purge(t.Context(), lifeUser); !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestUnitCommonStatementPeriodUsesAllAccountsAndReadableFallback(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	a := time.Date(2026, 2, 1, 22, 0, 0, 0, time.UTC)
	b := a.Add(48 * time.Hour)
	c := a.Add(24 * time.Hour)
	p := statementPeriod(Document{Periods: []Period{{From: c, To: b}, {From: a, To: c}}}, zone)
	if p == nil || !p.From.Equal(startDay(a, zone)) || !p.To.Equal(startDay(b, zone)) {
		t.Fatal(p)
	}
	p = statementPeriod(Document{Lines: []Line{{OccurredAt: &b}, {OccurredAt: &a}, {OccurredAt: &c, Reason: "invalid_date"}, {}}}, zone)
	if p == nil || !p.From.Equal(startDay(a, zone)) || !p.To.Equal(startDay(b, zone)) {
		t.Fatal(p)
	}
	if p = statementPeriod(Document{Lines: []Line{{OccurredAt: &a, Reason: "invalid_amount"}, {}}}, zone); p != nil {
		t.Fatal("unread date created coverage")
	}
	want := Period{From: a, To: b}
	got := completedPeriods([]Upload{{Status: "done", Period: &want}, {Status: "done"}, {Status: "failed", Period: &want}, {Status: "processing", Period: &want}})
	if len(got) != 1 || got[0] != want {
		t.Fatal(got)
	}
	for _, key := range []string{lifeUser + "/" + lifeUpload + "/extra", lifeUser + "/invalid", lifeUser + "/00000000-0000-0000-0000-000000000000", "different/" + lifeUpload} {
		if ownedObjectKey(lifeUser, key) {
			t.Fatal("unsafe key", key)
		}
	}
}

func TestUnitWatchRetiredExpiredUploadStopsWithoutFailureLoop(t *testing.T) {
	f := newLife(t)
	f.upload.UploadedAt = f.now.Add(-4 * time.Minute)
	f.deleted.alive = false
	err := f.s.Watch(t.Context(), lifeUser, lifeUpload, func(Event) error { t.Error("retired result emitted"); return nil })
	if !errors.Is(err, ErrDeleted) || f.trace.count("upload") != 0 || f.trace.count("guard") != 0 || f.trace.count("fail") != 0 {
		t.Fatal("retired watch did not stop", err, f.trace)
	}
	if err := f.s.Process(t.Context(), lifeUser, lifeUpload); err != nil {
		t.Fatal("worker retirement became a retry error", err)
	}
}
