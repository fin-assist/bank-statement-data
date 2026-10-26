package importing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type perfReferenceRepo struct {
	*lifeRepo
	batch func(context.Context, []string) (map[string]bool, error)
}

func (r *perfReferenceRepo) ReferencedBatch(ctx context.Context, keys []string) (map[string]bool, error) {
	return r.batch(ctx, keys)
}

type perfObjectBarrier struct {
	ObjectBarrier
	before func()
}

func (b perfObjectBarrier) Exclusive(ctx context.Context, user string) (func() error, error) {
	b.before()
	return b.ObjectBarrier.Exclusive(ctx, user)
}

func TestUnitOrphanBatchSkipsHealthyLocksAndFreshlyRechecksCandidate(t *testing.T) {
	f := newLife(t)
	const healthy = 999
	var objects []Object
	known := map[string]bool{}
	for range healthy {
		key := lifeUser + "/" + uuid.NewString()
		objects = append(objects, Object{Key: key, ModifiedAt: f.now.Add(-time.Hour)})
		known[key] = true
	}
	candidate := lifeUser + "/" + uuid.NewString()
	objects = append(objects, Object{Key: candidate, ModifiedAt: f.now.Add(-time.Hour)})
	f.objects.list = func(string, string) (ObjectPage, error) { return ObjectPage{Objects: objects}, nil }
	batches := 0
	f.s.repo = &perfReferenceRepo{lifeRepo: f.repo, batch: func(ctx context.Context, keys []string) (map[string]bool, error) {
		batches++
		if ctx.Value(lifeTxnKey{}) == true || len(keys) != healthy+1 {
			t.Fatalf("batch escaped prefilter phase: %d", len(keys))
		}
		return known, nil
	}}
	// Admission committed after the prefilter: the old fresh check must save this source.
	f.s.barrier = perfObjectBarrier{ObjectBarrier: f.barrier, before: func() { f.repo.referenced = true }}
	if err := f.s.SweepOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if batches != 1 || f.trace.count("exclusive") != 1 || f.trace.count("reference") != 1 || f.trace.count("unlock") != 1 || f.trace.count("fence") != 0 {
		t.Fatalf("healthy rows locked or fresh reference ignored: batches=%d trace=%v", batches, f.trace.events)
	}
}
func TestUnitOrphanBatchFailureAndInvalidPageNeverAcquireBarrier(t *testing.T) {
	for _, empty := range []bool{false, true} {
		f := newLife(t)
		failure := errors.New("primary unavailable")
		called := false
		f.objects.list = func(string, string) (ObjectPage, error) {
			if empty {
				return ObjectPage{Objects: []Object{{Key: "foreign/path", ModifiedAt: f.now.Add(-time.Hour)}}}, nil
			}
			return ObjectPage{Objects: []Object{{Key: lifeUser + "/" + lifeUpload, ModifiedAt: f.now.Add(-time.Hour)}}}, nil
		}
		f.s.repo = &perfReferenceRepo{lifeRepo: f.repo, batch: func(context.Context, []string) (map[string]bool, error) { called = true; return nil, failure }}
		err := f.s.SweepOrphans(t.Context())
		if empty {
			if err != nil || called {
				t.Fatalf("invalid namespace reached primary: %v", err)
			}
		} else if !errors.Is(err, failure) || !called {
			t.Fatal(err)
		}
		if f.trace.count("exclusive") != 0 || f.trace.count("fence") != 0 || f.s.sweepCursor != "" {
			t.Fatal("failed prefilter mutated cleanup state", f.trace.events)
		}
	}
}
