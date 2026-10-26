package importing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type jobRepo struct {
	*lifeRepo
	get        func(context.Context, string, string) (Upload, error)
	candidates func(context.Context, int, time.Time) ([]Upload, error)
}

func (r *jobRepo) Upload(ctx context.Context, user, id string) (Upload, error) {
	if r.get != nil {
		return r.get(ctx, user, id)
	}
	return r.lifeRepo.Upload(ctx, user, id)
}
func (r *jobRepo) Cleanup(ctx context.Context, limit int, before time.Time) ([]Upload, error) {
	if r.candidates != nil {
		return r.candidates(ctx, limit, before)
	}
	return r.lifeRepo.Cleanup(ctx, limit, before)
}
func jobReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("job did not reach expected phase")
		var zero T
		return zero
	}
}

func TestUnitRunBoundsWorkersDeduplicatesActiveJobsAndJoinsOnCancel(t *testing.T) {
	f := newLife(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan string, 8)
	polled := make(chan struct{}, 8)
	var active atomic.Int32
	var mu sync.Mutex
	calls := map[string]int{}
	r := &jobRepo{lifeRepo: f.repo}
	r.get = func(ctx context.Context, _, id string) (Upload, error) {
		active.Add(1)
		defer active.Add(-1)
		mu.Lock()
		calls[id]++
		mu.Unlock()
		entered <- id
		<-ctx.Done()
		return Upload{}, ctx.Err()
	}
	f.repo.pending = func(ctx context.Context) ([]Upload, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Error("queue read lacks bounded context")
		}
		polled <- struct{}{}
		return []Upload{{UserID: lifeUser, ID: "a"}, {UserID: lifeUser, ID: "a"}, {UserID: lifeUser, ID: "b"}, {UserID: lifeUser, ID: "c"}, {UserID: lifeUser, ID: "d"}}, nil
	}
	f.s.repo = r
	done := make(chan error, 1)
	go func() { done <- f.s.Run(ctx) }()
	a, b := jobReceive(t, entered), jobReceive(t, entered)
	if a == b {
		t.Fatal("duplicate active job was scheduled")
	}
	jobReceive(t, polled)
	jobReceive(t, polled)
	if n := active.Load(); n != 2 {
		t.Fatalf("active workers=%d", n)
	}
	select {
	case <-entered:
		t.Fatal("more than two workers entered processing")
	default:
	}
	cancel()
	if err := jobReceive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if active.Load() != 0 {
		t.Fatal("Run returned before joining workers")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, n := range calls {
		if n != 1 {
			t.Fatal("active job repeated", calls)
		}
	}
}

func TestUnitRunPropagatesReadAndWorkerFailuresAndContainsPanic(t *testing.T) {
	failure := errors.New("queue unavailable")
	t.Run("pending", func(t *testing.T) {
		f := newLife(t)
		f.repo.pending = func(context.Context) ([]Upload, error) { return nil, failure }
		if err := f.s.Run(t.Context()); !errors.Is(err, failure) {
			t.Fatal(err)
		}
		if f.trace.count("open") != 0 {
			t.Fatal("failed queue processed source")
		}
	})
	for _, kind := range []string{"read", "panic"} {
		t.Run(kind, func(t *testing.T) {
			f := newLife(t)
			f.repo.pending = func(context.Context) ([]Upload, error) { return []Upload{f.upload}, nil }
			if kind == "panic" {
				f.repo.upload = func() (Upload, error) { panic("private panic value") }
			} else {
				f.repo.errs["upload"] = failure
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := f.s.Run(ctx)
			if kind == "read" {
				if !errors.Is(err, failure) {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != "statement worker stopped unexpectedly" {
				t.Fatal("panic not sanitized", err)
			}
			if f.trace.count("open") != 0 {
				t.Fatal("source opened after worker read failed")
			}
		})
	}
	for _, ignored := range []error{ErrNotFound, ErrDeleted} {
		t.Run(ignored.Error(), func(t *testing.T) {
			f := newLife(t)
			var polls atomic.Int32
			f.repo.pending = func(context.Context) ([]Upload, error) {
				if polls.Add(1) > 1 {
					return nil, failure
				}
				return []Upload{f.upload}, nil
			}
			f.repo.errs["upload"] = ignored
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := f.s.Run(ctx); !errors.Is(err, failure) {
				t.Fatal("terminal job stopped queue", err)
			}
		})
	}
}

func TestUnitRunCleanupUsesAgedBoundedCandidatesAndConditionalRetirement(t *testing.T) {
	for _, status := range []string{"processing", "done", "staging", "failed", "deleted", "already-erased"} {
		t.Run(status, func(t *testing.T) {
			f := newLife(t)
			f.upload.Status = status
			if status == "already-erased" {
				f.upload.Status = "failed"
				f.upload.FileDeletedAt = &f.now
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// Cancel at the first completed pass without advancing the production ticker.
			r := &jobRepo{lifeRepo: f.repo}
			r.candidates = func(read context.Context, limit int, before time.Time) ([]Upload, error) {
				if limit != 16 || !before.Equal(f.now.Add(-time.Minute)) {
					t.Error("cleanup admission changed")
				}
				if deadline, ok := read.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
					t.Error("unbounded candidate read")
				}
				return []Upload{f.upload}, nil
			}
			r.get = func(ctx context.Context, user, id string) (Upload, error) {
				u, err := f.repo.Upload(ctx, user, id)
				if status == "processing" || status == "done" || status == "already-erased" {
					cancel()
				}
				return u, err
			}
			f.repo.onFileDeleted = cancel
			f.s.repo = r
			if err := f.s.RunCleanup(ctx); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			want := 0
			if status == "staging" || status == "failed" || status == "deleted" {
				want = 1
			}
			if f.trace.count("fence") != want || f.trace.count("file-deleted") != want {
				t.Fatal("wrong candidate erased", f.trace.events)
			}
			if (f.trace.count("abandon") == 1) != (status == "staging") {
				t.Fatal("nonstaging intent abandoned", f.trace.events)
			}
			if f.trace.count("unlock") != 1 {
				t.Fatal("cleanup leaked barrier")
			}
		})
	}
}

func TestUnitRunCleanupStopsOnErrorsAndIgnoresRetiredCandidates(t *testing.T) {
	failure := errors.New("cleanup unavailable")
	for _, phase := range []string{"candidates", "barrier", "guard", "upload", "abandon", "fence", "file-deleted", "unlock"} {
		t.Run(phase, func(t *testing.T) {
			f := newLife(t)
			f.upload.Status = "staging"
			f.repo.cleanup = func(context.Context) ([]Upload, error) { return []Upload{f.upload}, nil }
			switch phase {
			case "candidates":
				f.repo.cleanup = func(context.Context) ([]Upload, error) { return nil, failure }
			case "barrier":
				f.barrier.err = failure
			case "guard":
				f.deleted.err = failure
			case "fence":
				f.objects.deleteErr = failure
			case "unlock":
				f.barrier.unlockErr = failure
			default:
				f.repo.errs[phase] = failure
			}
			if err := f.s.RunCleanup(t.Context()); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if phase == "candidates" || phase == "barrier" || phase == "guard" || phase == "upload" || phase == "abandon" {
				if f.trace.count("fence") != 0 {
					t.Fatal("unsafe candidate erased")
				}
			}
		})
	}
	for _, ignored := range []error{ErrDeleted, ErrNotFound} {
		t.Run(ignored.Error(), func(t *testing.T) {
			f := newLife(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.repo.cleanup = func(context.Context) ([]Upload, error) { return []Upload{f.upload}, nil }
			f.repo.upload = func() (Upload, error) { cancel(); return Upload{}, ignored }
			if err := f.s.RunCleanup(ctx); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if f.trace.count("fence") != 0 {
				t.Fatal("retired candidate erased")
			}
		})
	}
	t.Run("pre-cancelled", func(t *testing.T) {
		f := newLife(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := f.s.RunCleanup(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestUnitSweepOrphansHonorsNamespaceGracePrimaryReferenceAndEmptyPages(t *testing.T) {
	f := newLife(t)
	valid := lifeUser + "/" + lifeUpload
	objects := []Object{
		{Key: valid, ModifiedAt: f.now.Add(-time.Hour)},
		{Key: valid, ModifiedAt: f.now.Add(-time.Hour + time.Nanosecond)},
		{Key: valid},
		{Key: "other-prefix/" + lifeUpload, ModifiedAt: f.now.Add(-2 * time.Hour)},
		{Key: "00000000-0000-0000-0000-000000000000/" + lifeUpload, ModifiedAt: f.now.Add(-2 * time.Hour)},
		{Key: lifeUser + "/bad-id", ModifiedAt: f.now.Add(-2 * time.Hour)},
		{Key: valid + "/extra", ModifiedAt: f.now.Add(-2 * time.Hour)},
		{Key: strings.ReplaceAll(lifeUser, "-", "") + "/" + lifeUpload, ModifiedAt: f.now.Add(-2 * time.Hour)},
		{Key: lifeUser + "/00000000-0000-0000-0000-000000000000", ModifiedAt: f.now.Add(-2 * time.Hour)},
	}
	var cursors []string
	f.objects.list = func(prefix, cursor string) (ObjectPage, error) {
		if prefix != "" {
			t.Fatal("orphan scan should use entire private namespace")
		}
		cursors = append(cursors, cursor)
		if cursor == "" {
			return ObjectPage{Next: "page2"}, nil
		}
		return ObjectPage{Objects: objects}, nil
	}
	if err := f.s.SweepOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(cursors) != "[ page2]" || f.s.sweepCursor != "" {
		t.Fatal("empty provider page skipped continuation", cursors)
	}
	if f.trace.count("exclusive") != 1 || f.trace.count("reference") != 1 || f.trace.count("fence") != 1 || f.trace.count("unlock") != 1 {
		t.Fatal("namespace/grace boundary not respected", f.trace.events)
	}
	f = newLife(t)
	f.repo.referenced = true
	f.objects.list = func(string, string) (ObjectPage, error) { return ObjectPage{Objects: objects[:1]}, nil }
	if err := f.s.SweepOrphans(t.Context()); err != nil || f.trace.count("fence") != 0 || f.trace.count("reference-batch") != 1 || f.trace.count("reference") != 0 || f.trace.count("unlock") != 0 {
		t.Fatal("live primary reference erased", err)
	}
}

func TestUnitSweepOrphansFailsClosedAndReleasesLocks(t *testing.T) {
	failure := errors.New("storage unavailable")
	for _, phase := range []string{"list", "reference-batch", "barrier", "transaction", "guard", "lock", "reference", "fence", "unlock"} {
		t.Run(phase, func(t *testing.T) {
			f := newLife(t)
			f.objects.list = func(string, string) (ObjectPage, error) {
				return ObjectPage{Objects: []Object{{Key: f.upload.ObjectKey, ModifiedAt: f.now.Add(-time.Hour)}}}, nil
			}
			switch phase {
			case "list":
				f.objects.list = func(string, string) (ObjectPage, error) { return ObjectPage{}, failure }
			case "barrier":
				f.barrier.err = failure
			case "transaction":
				f.tx.before = failure
			case "guard":
				f.deleted.err = failure
			case "fence":
				f.objects.deleteErr = failure
			case "unlock":
				f.barrier.unlockErr = failure
			default:
				f.repo.errs[phase] = failure
			}
			if err := f.s.SweepOrphans(t.Context()); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if phase != "fence" && phase != "unlock" && f.trace.count("fence") != 0 {
				t.Fatal("uncertain primary state erased")
			}
			want := 1
			if phase == "list" || phase == "reference-batch" || phase == "barrier" {
				want = 0
			}
			if f.trace.count("unlock") != want {
				t.Fatal("barrier leak", f.trace.events)
			}
		})
	}
	t.Run("retired-user", func(t *testing.T) {
		f := newLife(t)
		f.deleted.alive = false
		f.objects.list = func(string, string) (ObjectPage, error) {
			return ObjectPage{Objects: []Object{{Key: f.upload.ObjectKey, ModifiedAt: f.now.Add(-time.Hour)}}}, nil
		}
		if err := f.s.SweepOrphans(t.Context()); err != nil || f.trace.count("fence") != 1 || f.trace.count("reference") != 0 {
			t.Fatal(err)
		}
	})
	t.Run("missing-user-row", func(t *testing.T) {
		f := newLife(t)
		f.repo.errs["lock"] = ErrNotFound
		f.objects.list = func(string, string) (ObjectPage, error) {
			return ObjectPage{Objects: []Object{{Key: f.upload.ObjectKey, ModifiedAt: f.now.Add(-time.Hour)}}}, nil
		}
		if err := f.s.SweepOrphans(t.Context()); err != nil || f.trace.count("fence") != 1 || f.trace.count("reference") != 1 {
			t.Fatal(err)
		}
	})
}

func TestUnitSweepOrphansResumesOpaqueCursorAndRejectsProviderCycles(t *testing.T) {
	failure := errors.New("provider unavailable")
	f := newLife(t)
	var cursors []string
	f.objects.list = func(_, cursor string) (ObjectPage, error) {
		cursors = append(cursors, cursor)
		if cursor == "" {
			return ObjectPage{Next: "resume-token"}, nil
		}
		return ObjectPage{}, failure
	}
	if err := f.s.SweepOrphans(t.Context()); !errors.Is(err, failure) || f.s.sweepCursor != "resume-token" {
		t.Fatal(err)
	}
	f.objects.list = func(_, cursor string) (ObjectPage, error) {
		cursors = append(cursors, cursor)
		return ObjectPage{}, nil
	}
	if err := f.s.SweepOrphans(t.Context()); err != nil || f.s.sweepCursor != "" || fmt.Sprint(cursors) != "[ resume-token resume-token]" {
		t.Fatal(err, cursors)
	}
	for _, cycle := range []string{"same", "two-page"} {
		t.Run(cycle, func(t *testing.T) {
			f := newLife(t)
			f.objects.list = func(_, cursor string) (ObjectPage, error) {
				if cycle == "same" || cursor == "" {
					return ObjectPage{Next: "a"}, nil
				}
				if cursor == "a" {
					return ObjectPage{Next: "b"}, nil
				}
				return ObjectPage{Next: "a"}, nil
			}
			if err := f.s.SweepOrphans(t.Context()); !errors.Is(err, errAccounting) {
				t.Fatal("provider cycle accepted", err)
			}
			if f.trace.count("fence") != 0 {
				t.Fatal("empty pages erased an object")
			}
		})
	}
}
