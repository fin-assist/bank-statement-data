package importing_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	idem "github.com/fin-assist/go-idempotency"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type uploadTransactionContext struct{}
type uploadTransactionNumber struct{}
type uploadKeyCallback struct{}

type uploadTransactions struct {
	active      atomic.Int32
	before      func() error
	after       func() error
	knownCommit func(context.Context)
	sequence    int
}

func (*uploadTransactions) Read(context.Context, func(context.Context) error) error {
	return errors.New("Upload must not use read-only transactions")
}

func (tx *uploadTransactions) Run(ctx context.Context, fn func(context.Context) error) error {
	if tx.before != nil {
		if err := tx.before(); err != nil {
			return err
		}
	}
	tx.active.Add(1)
	defer tx.active.Add(-1)
	tx.sequence++
	ctx = context.WithValue(context.WithValue(ctx, uploadTransactionContext{}, true), uploadTransactionNumber{}, tx.sequence)
	if err := fn(ctx); err != nil {
		return err
	}
	if tx.after != nil {
		if err := tx.after(); err != nil {
			return err
		}
	}
	if tx.knownCommit != nil {
		tx.knownCommit(ctx)
	}
	return nil
}

type uploadRepository struct {
	importing.Repository
	t *testing.T
	// Insert attempts are recorded; this is not a fake transactional database.
	inserted                 []importing.Upload
	referenced               bool
	refErr                   error
	probes                   int
	stageTx                  int
	stageKnownCommitted      bool
	promotes                 int
	promoteTx                int
	abandons                 int
	retirementTx             int
	retirementKnownCommitted bool
	abandonErr               error
	onAbandon                func()
}

func (r *uploadRepository) LockUser(ctx context.Context, _ string, _ bool) (string, error) {
	r.t.Helper()
	if ctx.Value(uploadTransactionContext{}) != true {
		r.t.Error("user guard escaped transaction")
	}
	return "Europe/Moscow", nil
}
func (r *uploadRepository) InsertUpload(ctx context.Context, upload importing.Upload) error {
	if ctx.Value(uploadTransactionContext{}) != true {
		r.t.Error("insert escaped transaction")
	}
	if ctx.Value(uploadKeyCallback{}) == true || upload.Status != "staging" {
		r.t.Error("upload insert must create private staging before the key completion callback")
	}
	r.stageTx, _ = ctx.Value(uploadTransactionNumber{}).(int)
	r.inserted = append(r.inserted, upload)
	return nil
}
func (r *uploadRepository) PromoteUpload(ctx context.Context, user, id string, now time.Time) error {
	if ctx.Value(uploadTransactionContext{}) != true || ctx.Value(uploadKeyCallback{}) != true || !r.stageKnownCommitted {
		r.t.Error("promotion must run in the key callback after a known staging commit")
	}
	if len(r.inserted) == 0 || r.inserted[0].UserID != user || r.inserted[0].ID != id || now.IsZero() {
		r.t.Error("promotion changed the staged upload identity or metadata")
	}
	r.promotes++
	r.promoteTx, _ = ctx.Value(uploadTransactionNumber{}).(int)
	return nil
}
func (r *uploadRepository) AbandonStaging(ctx context.Context, _, _ string, _ time.Time) error {
	if ctx.Value(uploadTransactionContext{}) != true || ctx.Value(uploadKeyCallback{}) == true {
		r.t.Error("conditional staging retirement escaped its fresh cleanup transaction")
	}
	r.abandons++
	r.retirementTx, _ = ctx.Value(uploadTransactionNumber{}).(int)
	if r.onAbandon != nil {
		r.onAbandon()
	}
	// Record only the conditional port call. Referenced remains the test's explicit
	// authoritative primary outcome, never a simulated SQL state transition.
	return r.abandonErr
}
func (r *uploadRepository) Referenced(ctx context.Context, _, _ string) (bool, error) {
	if ctx.Value(uploadTransactionContext{}) != true {
		r.t.Error("cleanup reference probe did not use a fresh primary transaction")
	}
	if !r.retirementKnownCommitted {
		r.t.Error("primary reference probe ran before a known staging retirement commit")
	}
	r.probes++
	return r.referenced, r.refErr
}

type uploadObjects struct {
	importing.ObjectStore
	t            *testing.T
	tx           *uploadTransactions
	puts         int
	deletes      []string
	key          string
	content      []byte
	size         int64
	putErr       error
	beforeDelete func()
	beforePut    func()
}

func (o *uploadObjects) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	if ctx.Value(uploadTransactionContext{}) == true || o.tx.active.Load() != 0 {
		o.t.Error("remote PUT ran inside transaction")
	}
	if o.beforePut != nil {
		o.beforePut()
	}
	o.puts++
	o.key, o.size = key, size
	var err error
	o.content, err = io.ReadAll(body)
	if err != nil {
		return err
	}
	return o.putErr
}
func (o *uploadObjects) Delete(ctx context.Context, key string) error {
	// The port records a logical 0B erasure fence, not an emulation of S3 storage.
	if ctx.Value(uploadTransactionContext{}) == true || o.tx.active.Load() != 0 {
		o.t.Error("remote DELETE ran inside transaction")
	}
	if o.beforeDelete != nil {
		o.beforeDelete()
	}
	o.deletes = append(o.deletes, key)
	return nil
}

type uploadBarrier struct {
	importing.ObjectBarrier
	acquires atomic.Int32
	releases atomic.Int32
	err      error
}

func (b *uploadBarrier) Shared(context.Context, string) (func() error, error) {
	b.acquires.Add(1)
	if b.err != nil {
		return nil, b.err
	}
	return func() error { b.releases.Add(1); return nil }, nil
}

type uploadParser struct {
	t     *testing.T
	tx    *uploadTransactions
	calls int
	err   error
	bytes []byte
}

func (p *uploadParser) Validate(r io.Reader) error {
	if p.tx.active.Load() != 0 {
		p.t.Error("source admission ran inside transaction")
	}
	p.calls++
	var err error
	p.bytes, err = io.ReadAll(r)
	if err != nil {
		return err
	}
	return p.err
}
func (*uploadParser) Parse(context.Context, io.Reader) (importing.Document, error) {
	return importing.Document{}, errors.New("unit upload must not start background parsing")
}

type uploadKeys struct {
	t           *testing.T
	reserves    int
	bound       []idem.Resource
	completions int
	releases    int
	request     idem.Request
	replay      idem.Result
	reserveErr  error
	doErr       error
	onReserve   func()
	doTx        int
}

func (k *uploadKeys) Reserve(ctx context.Context, r idem.Request) (idem.Reservation, idem.Result, error) {
	if ctx.Value(uploadTransactionContext{}) != true {
		k.t.Error("key reserve escaped transaction")
	}
	k.reserves++
	k.request = r
	if k.onReserve != nil {
		k.onReserve()
	}
	return idem.Reservation{}, k.replay, k.reserveErr
}
func (k *uploadKeys) BindResource(ctx context.Context, _ idem.Reservation, resource idem.Resource) error {
	if ctx.Value(uploadTransactionContext{}) != true {
		k.t.Error("resource fencing escaped its transaction")
	}
	k.bound = append(k.bound, resource)
	return nil
}
func (*uploadKeys) WithCompletion(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}
func (k *uploadKeys) DoReserved(ctx context.Context, _ idem.Reservation, callback func(context.Context) (idem.Response, error)) (idem.Result, error) {
	if ctx.Value(uploadTransactionContext{}) != true {
		k.t.Error("key completion escaped transaction")
	}
	k.completions++
	k.doTx, _ = ctx.Value(uploadTransactionNumber{}).(int)
	if k.doErr != nil {
		return idem.Result{}, k.doErr
	}
	// This port fake only invokes the effect; it implements no real cache/codec policy.
	response, err := callback(context.WithValue(ctx, uploadKeyCallback{}, true))
	return idem.Result{Response: response, Err: err}, nil
}
func (k *uploadKeys) Release(ctx context.Context, _ idem.Reservation) error {
	if ctx.Value(uploadTransactionContext{}) != true {
		k.t.Error("key release escaped transaction")
	}
	k.releases++
	return nil
}

type uploadTombstones struct {
	importing.Tombstones
	t     *testing.T
	alive bool
	calls int
	err   error
}

func (g *uploadTombstones) Guard(ctx context.Context, _ string) (bool, error) {
	if ctx.Value(uploadTransactionContext{}) != true {
		g.t.Error("tombstone check escaped transaction")
	}
	g.calls++
	return g.alive, g.err
}

type uploadFixture struct {
	t       *testing.T
	tx      *uploadTransactions
	repo    *uploadRepository
	objects *uploadObjects
	barrier *uploadBarrier
	parser  *uploadParser
	keys    *uploadKeys
	deleted *uploadTombstones
	user    string
	id      string
	now     time.Time
}

func newUploadFixture(t *testing.T) *uploadFixture {
	t.Helper()
	f := &uploadFixture{t: t, tx: &uploadTransactions{}, user: uuid.NewString(), id: uuid.NewString(), now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	f.repo = &uploadRepository{t: t}
	f.objects = &uploadObjects{t: t, tx: f.tx}
	f.barrier = &uploadBarrier{}
	f.parser = &uploadParser{t: t, tx: f.tx}
	f.keys = &uploadKeys{t: t}
	f.deleted = &uploadTombstones{t: t, alive: true}
	f.tx.knownCommit = func(ctx context.Context) {
		id, _ := ctx.Value(uploadTransactionNumber{}).(int)
		if id == f.repo.stageTx {
			f.repo.stageKnownCommitted = true
		}
		if id == f.repo.retirementTx {
			f.repo.retirementKnownCommitted = true
		}
	}
	f.objects.beforePut = func() {
		if !f.repo.stageKnownCommitted || len(f.repo.inserted) == 0 || f.repo.promotes != 0 {
			t.Error("PUT began before durable private staging or after premature promotion")
		}
	}
	f.repo.onAbandon = func() {
		if f.barrier.releases.Load() != 0 {
			t.Error("shared fence released before conditional staging cleanup")
		}
	}
	f.objects.beforeDelete = func() {
		if f.repo.abandons == 0 || f.repo.probes == 0 || f.barrier.releases.Load() != 0 {
			t.Error("file erasure fence ran without staging retirement, primary proof or shared barrier")
		}
	}
	return f
}

func (f *uploadFixture) service() *importing.Service {
	f.t.Helper()
	s, err := importing.New(f.tx, f.repo, f.objects, f.barrier, f.parser, f.keys, f.deleted, func() time.Time { return f.now }, func() (string, error) { return f.id, nil }, statementdatav1.StatementsService_UploadStatement_FullMethodName)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *uploadFixture) noEffects() {
	f.t.Helper()
	if f.keys.reserves != 0 || f.objects.puts != 0 || len(f.objects.deletes) != 0 || len(f.repo.inserted) != 0 || f.barrier.acquires.Load() != 0 {
		f.t.Fatal("pre-key refusal created effects")
	}
}

type uploadReadSpy struct {
	r     io.Reader
	reads atomic.Int32
	eof   atomic.Bool
}

func (r *uploadReadSpy) Read(p []byte) (int, error) {
	r.reads.Add(1)
	n, err := r.r.Read(p)
	if errors.Is(err, io.EOF) {
		r.eof.Store(true)
	}
	return n, err
}

func TestUnitUploadPreKeyRefusalsCreateNoEffects(t *testing.T) {
	for name, header := range map[string]importing.Header{
		"oversize hint": {FileName: "exact.ofx", SizeBytes: importing.MaxFileBytes + 1},
		"negative hint": {FileName: "exact.ofx", SizeBytes: -1},
		"NUL name":      {FileName: "private\x00name"},
		"invalid UTF8":  {FileName: string([]byte{0xff})},
	} {
		t.Run(name, func(t *testing.T) {
			f := newUploadFixture(t)
			body := &uploadReadSpy{r: strings.NewReader("not read")}
			_, err := f.service().Upload(context.Background(), f.user, header, body, []string{"bad-key"})
			if name == "oversize hint" {
				if !errors.Is(err, importing.ErrTooLarge) {
					t.Fatal("oversize hint did not precede invalid key")
				}
			} else {
				var input *importing.InputError
				if !errors.As(err, &input) || strings.Contains(err.Error(), "private") {
					t.Fatal("invalid header not safely rejected")
				}
			}
			if body.reads.Load() != 0 {
				t.Fatal("invalid preflight header consumed body")
			}
			f.noEffects()
		})
	}
	t.Run("actual oversize precedes key", func(t *testing.T) {
		f := newUploadFixture(t)
		_, err := f.service().Upload(context.Background(), f.user, importing.Header{FileName: "exact.ofx", SizeBytes: 1}, strings.NewReader(strings.Repeat("x", int(importing.MaxFileBytes+1))), []string{"bad-key"})
		if !errors.Is(err, importing.ErrTooLarge) {
			t.Fatal("actual byte budget did not precede key")
		}
		f.noEffects()
	})
}

func TestUnitUploadParserRefusalSkipsObjectAndInsert(t *testing.T) {
	for _, refusal := range []error{importing.ErrCSVNotAccepted, importing.ErrWrongFormat, importing.ErrWrongBank} {
		f := newUploadFixture(t)
		f.parser.err = refusal
		_, err := f.service().Upload(context.Background(), f.user, importing.Header{FileName: "exact.ofx"}, strings.NewReader("synthetic content"), []string{uuid.NewString()})
		if !errors.Is(err, refusal) || f.keys.completions != 1 || f.objects.puts != 0 || len(f.repo.inserted) != 0 || f.barrier.releases.Load() != 1 {
			t.Fatal("parser refusal bypassed reserved effect or wrote object")
		}
	}
}

func TestUnitUploadReplayGuardPrecedesCachedPayload(t *testing.T) {
	for _, guardState := range []string{"live", "deleted", "error"} {
		f := newUploadFixture(t)
		f.deleted.alive = guardState != "deleted"
		guardErr := errors.New("synthetic tombstone uncertainty")
		if guardState == "error" {
			f.deleted.err = guardErr
		}
		body, err := json.Marshal(importing.Accepted{UploadID: f.id, FileName: "original name.ofx"})
		if err != nil {
			t.Fatal(err)
		}
		f.keys.replay = idem.Result{Replayed: true, Response: idem.Response{Body: body, Resource: idem.Resource{Type: "upload", ID: f.id}}}
		if guardState != "live" {
			f.keys.replay.Response.Body = []byte("malformed cached body must not be decoded")
		}
		got, err := f.service().Upload(context.Background(), f.user, importing.Header{FileName: "request name.ofx"}, strings.NewReader("content"), []string{uuid.NewString()})
		if guardState != "live" {
			want := importing.ErrDeleted
			if guardState == "error" {
				want = guardErr
			}
			if !errors.Is(err, want) || f.keys.reserves != 0 {
				t.Fatal("tombstone did not fence cached replay")
			}
		} else if err != nil || got.UploadID != f.id || got.FileName != "original name.ofx" || f.deleted.calls == 0 {
			t.Fatal("replay lost cached resource or bypassed tombstone", err)
		}
		if f.parser.calls != 0 || f.objects.puts != 0 || len(f.repo.inserted) != 0 || len(f.keys.bound) != 0 {
			t.Fatal("cached replay repeated admission effects")
		}
	}
}

func TestUnitUploadReadsEOFFirstAndPreservesExactMetadata(t *testing.T) {
	f := newUploadFixture(t)
	content := "synthetic exact UTF8 bytes: \u0410\u0431\u0432 &amp;\n"
	filename := "  \u0412\u044b\u043f\u0438\u0441\u043a\u0430 " + strings.Repeat("\u044f", 260) + ".ofx  "
	body := &uploadReadSpy{r: strings.NewReader(content)}
	f.keys.onReserve = func() {
		if !body.eof.Load() {
			t.Error("idempotency reservation began before actual EOF")
		}
	}
	key := uuid.New()
	got, err := f.service().Upload(context.Background(), f.user, importing.Header{FileName: filename, SizeBytes: 1}, body, []string{strings.ToUpper(key.String())})
	if err != nil || got.UploadID != f.id || got.FileName != filename {
		t.Fatal("exact upload acceptance changed", err)
	}
	digest := sha256.Sum256([]byte(content))
	if f.keys.request.Hash != idem.HashUpload(digest, filename) || f.keys.request.Key != key.String() || f.keys.request.Scope != f.user || f.keys.request.Endpoint != statementdatav1.StatementsService_UploadStatement_FullMethodName {
		t.Fatal("published upload hash or normalized key scope changed")
	}
	if len(f.repo.inserted) == 0 || f.objects.puts != 1 || string(f.objects.content) != content || string(f.parser.bytes) != content || f.objects.size != int64(len(content)) {
		t.Fatal("actual file bytes/size not preserved")
	}
	u := f.repo.inserted[len(f.repo.inserted)-1]
	if u.FileName != filename || u.Digest != digest || u.SizeBytes != int64(len(content)) || u.ObjectKey != f.user+"/"+f.id || u.Status != "staging" || !u.UploadedAt.Equal(f.now) || len(f.objects.deletes) != 0 || f.repo.promotes != 1 || f.repo.promoteTx != f.keys.doTx || f.repo.stageTx == f.repo.promoteTx {
		t.Fatal("durable upload metadata or committed object changed")
	}
	if f.barrier.releases.Load() != 1 {
		t.Fatal("successful upload did not release object fence")
	}
}

type uploadReadFailure struct{ err error }

func (r uploadReadFailure) Read([]byte) (int, error) { return 0, r.err }

func TestUnitUploadReadAndCancellationFailuresHaveNoEffects(t *testing.T) {
	boom := errors.New("synthetic read failure")
	f := newUploadFixture(t)
	if _, err := f.service().Upload(context.Background(), f.user, importing.Header{FileName: "exact.ofx"}, uploadReadFailure{boom}, []string{uuid.NewString()}); !errors.Is(err, boom) {
		t.Fatal("read failure ignored")
	}
	f.noEffects()
	f = newUploadFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.service().Upload(ctx, f.user, importing.Header{FileName: "exact.ofx"}, strings.NewReader("content"), []string{uuid.NewString()}); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-canceled upload admitted effects")
	}
	f.noEffects()
	for _, keys := range [][]string{nil, {"", "other"}, {"bad"}, {uuid.Nil.String()}} {
		f := newUploadFixture(t)
		reader := &uploadReadSpy{r: strings.NewReader("content")}
		_, err := f.service().Upload(context.Background(), f.user, importing.Header{FileName: "exact.ofx"}, reader, keys)
		var input *importing.InputError
		if !errors.As(err, &input) || !reader.eof.Load() {
			t.Fatal("key checked before EOF or invalid key accepted")
		}
		f.noEffects()
	}
}

type uploadBlockingReader struct {
	entered chan struct{}
	allow   chan struct{}
	once    sync.Once
	opened  sync.Once
}

func (r *uploadBlockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.allow
	return 0, io.EOF
}
func (r *uploadBlockingReader) unblock() { r.opened.Do(func() { close(r.allow) }) }

func TestUnitUploadFourBufferSlotsAndCanceledWaiter(t *testing.T) {
	f := newUploadFixture(t)
	s := f.service()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var readers []*uploadBlockingReader
	results := make(chan error, 4)
	var wg sync.WaitGroup
	for range 4 {
		r := &uploadBlockingReader{entered: make(chan struct{}), allow: make(chan struct{})}
		readers = append(readers, r)
		wg.Go(func() {
			_, err := s.Upload(ctx, f.user, importing.Header{FileName: "exact.ofx"}, r, nil)
			results <- err
		})
	}
	t.Cleanup(func() {
		for _, r := range readers {
			r.unblock()
		}
		wg.Wait()
	})
	for _, r := range readers {
		select {
		case <-r.entered:
		case <-ctx.Done():
			t.Fatal("four admitted buffers did not start reading")
		}
	}
	waiter, stop := context.WithCancel(ctx)
	spy := &uploadReadSpy{r: strings.NewReader("must not be buffered")}
	waitResult := make(chan error, 1)
	go func() {
		_, err := s.Upload(waiter, f.user, importing.Header{FileName: "exact.ofx"}, spy, []string{uuid.NewString()})
		waitResult <- err
	}()
	stop()
	select {
	case err := <-waitResult:
		if !errors.Is(err, context.Canceled) || spy.reads.Load() != 0 {
			t.Fatal("fifth canceled waiter consumed an additional buffer")
		}
	case <-ctx.Done():
		t.Fatal("buffer waiter ignored cancellation")
	}
	for _, r := range readers {
		r.unblock()
	}
	for range readers {
		select {
		case err := <-results:
			var input *importing.InputError
			if !errors.As(err, &input) {
				t.Fatal("released buffer did not reach normal post-EOF key validation")
			}
		case <-ctx.Done():
			t.Fatal("buffer holders did not finish")
		}
	}
	f.noEffects()
	if _, err := s.Upload(ctx, f.user, importing.Header{FileName: "fresh.ofx"}, strings.NewReader("content"), []string{uuid.NewString()}); err != nil {
		t.Fatal("released buffer slot could not be reused", err)
	}
}

func TestUnitUploadUnknownCommitNeverDeletesReferencedOrUncertainObject(t *testing.T) {
	for _, outcome := range []string{"referenced", "probe error", "probe commit error", "unreferenced"} {
		t.Run(outcome, func(t *testing.T) {
			f := newUploadFixture(t)
			commitErr := errors.New("synthetic unknown COMMIT")
			failed := false
			f.tx.after = func() error {
				if !failed && f.objects.puts > 0 && f.repo.promotes > 0 {
					failed = true
					return commitErr
				}
				if outcome == "probe commit error" && f.repo.probes > 0 {
					return errors.New("synthetic primary check COMMIT uncertainty")
				}
				return nil
			}
			f.repo.referenced = outcome == "referenced"
			if outcome == "probe error" {
				f.repo.refErr = errors.New("synthetic primary uncertainty")
			}
			f.objects.beforeDelete = func() {
				if f.repo.probes == 0 || f.tx.active.Load() != 0 || f.barrier.releases.Load() != 0 {
					t.Error("object cleanup escaped fresh primary proof or shared fence")
				}
			}
			_, err := f.service().Upload(context.Background(), f.user, importing.Header{FileName: "exact.ofx"}, strings.NewReader("content"), []string{uuid.NewString()})
			if !errors.Is(err, commitErr) || f.objects.puts != 1 || f.repo.probes == 0 || f.keys.releases == 0 || f.repo.abandons == 0 {
				t.Fatal("unknown commit did not preserve original failure and attempt guarded cleanup", err)
			}
			if outcome == "unreferenced" {
				if len(f.objects.deletes) != 1 || f.objects.deletes[0] != f.objects.key {
					t.Fatal("proven unreferenced object not cleaned")
				}
			} else if len(f.objects.deletes) != 0 {
				t.Fatal("referenced or uncertain object was deleted after unknown commit")
			}
		})
	}
}

func TestUnitUploadTransactionalAndRemoteFailures(t *testing.T) {
	for _, phase := range []string{"before reservation", "reserve failure", "before staging", "unknown staging COMMIT", "before final callback", "key completion failure", "PUT failure"} {
		t.Run(phase, func(t *testing.T) {
			f := newUploadFixture(t)
			boom := errors.New("synthetic admission failure")
			switch phase {
			case "before reservation":
				f.tx.before = func() error { return boom }
			case "reserve failure":
				f.keys.reserveErr = boom
			case "before staging":
				failed := false
				f.tx.before = func() error {
					if !failed && f.parser.calls > 0 && f.objects.puts == 0 {
						failed = true
						return boom
					}
					return nil
				}
			case "unknown staging COMMIT":
				failed := false
				f.tx.after = func() error {
					if !failed && f.repo.stageTx > 0 && !f.repo.stageKnownCommitted {
						failed = true
						return boom
					}
					return nil
				}
			case "before final callback":
				failed := false
				f.tx.before = func() error {
					if f.objects.puts > 0 && !failed {
						failed = true
						return boom
					}
					return nil
				}
			case "key completion failure":
				f.keys.doErr = boom
			case "PUT failure":
				f.objects.putErr = boom
			}
			_, err := f.service().Upload(context.Background(), f.user, importing.Header{FileName: "exact.ofx"}, strings.NewReader("content"), []string{uuid.NewString()})
			if !errors.Is(err, boom) || f.repo.promotes != 0 {
				t.Fatal("admission failure lost or insert effect ran", err)
			}
			if phase == "before reservation" || phase == "reserve failure" {
				if f.objects.puts != 0 || len(f.repo.inserted) != 0 {
					t.Fatal("pre-admission failure wrote an object")
				}
			} else if phase == "before staging" || phase == "unknown staging COMMIT" {
				if f.objects.puts != 0 || len(f.objects.deletes) != 0 || f.repo.stageKnownCommitted || f.keys.releases == 0 {
					t.Fatal("unconfirmed staging transaction allowed remote effects")
				}
			} else if f.objects.puts != 1 || f.keys.releases == 0 || f.repo.probes == 0 || len(f.objects.deletes) != 1 || f.repo.abandons == 0 {
				t.Fatal("failed episode did not clean proven unreferenced potential object")
			}
		})
	}
}

func TestUnitUploadUncertainStagingRetirementCannotEraseObject(t *testing.T) {
	for _, uncertainty := range []string{"retirement error", "retirement COMMIT error"} {
		t.Run(uncertainty, func(t *testing.T) {
			f := newUploadFixture(t)
			original := errors.New("synthetic lost PUT reply")
			f.objects.putErr = original
			if uncertainty == "retirement error" {
				f.repo.abandonErr = errors.New("synthetic staging retirement failure")
			} else {
				f.tx.after = func() error {
					if f.repo.abandons > 0 {
						return errors.New("synthetic unknown staging retirement COMMIT")
					}
					return nil
				}
			}
			_, err := f.service().Upload(context.Background(), f.user, importing.Header{FileName: "exact.ofx"}, strings.NewReader("content"), []string{uuid.NewString()})
			if !errors.Is(err, original) || f.repo.abandons == 0 || len(f.objects.deletes) != 0 || f.repo.probes != 0 || f.barrier.releases.Load() != 1 {
				t.Fatal("unconfirmed staging retirement erased an object or replaced primary failure", err)
			}
		})
	}
}

func TestUnitUploadConstructorRequiresEveryDependency(t *testing.T) {
	for missing := range 10 {
		f := newUploadFixture(t)
		var tx importing.Transactions = f.tx
		var repo importing.Repository = f.repo
		var objects importing.ObjectStore = f.objects
		var barrier importing.ObjectBarrier = f.barrier
		var parser importing.Parser = f.parser
		var keys importing.Keys = f.keys
		var deleted importing.Tombstones = f.deleted
		now := func() time.Time { return f.now }
		id := func() (string, error) { return f.id, nil }
		method := statementdatav1.StatementsService_UploadStatement_FullMethodName
		switch missing {
		case 0:
			tx = nil
		case 1:
			repo = nil
		case 2:
			objects = nil
		case 3:
			barrier = nil
		case 4:
			parser = nil
		case 5:
			keys = nil
		case 6:
			deleted = nil
		case 7:
			now = nil
		case 8:
			id = nil
		case 9:
			method = ""
		}
		if _, err := importing.New(tx, repo, objects, barrier, parser, keys, deleted, now, id, method); err == nil {
			t.Fatal("missing dependency accepted", missing)
		}
	}
}
