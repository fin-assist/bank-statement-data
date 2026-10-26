package app_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/objectstore"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func recoveryWait(t *testing.T, check func() bool) {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("durable recovery did not converge")
		case <-ticker.C:
		}
	}
}
func recoveryStop(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Error("worker exit", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("workers did not join after cancellation")
	}
}

func TestIntegrationDurableQueueSurvivesWorkerStopAndRestart(t *testing.T) {
	a, objects := importAssembly(t)
	var ids []string
	for _, name := range []string{"first.ofx", "second.ofx", "third.ofx"} {
		accepted, err := a.Imports.Upload(t.Context(), apiUser, importing.Header{FileName: name}, bytes.NewBufferString(apiOFX), []string{uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, accepted.UploadID)
	}
	before := importSnapshot(t, a, apiUser)
	if len(before.Uploads) != 3 || len(before.Operations) != 0 {
		t.Fatal("admission was not durable before worker startup")
	}
	entered := make(chan struct{}, 4)
	resume := make(chan struct{})
	objects.PauseReads(entered, resume)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Imports.Run(ctx) }()
	// Only this user's oldest durable head is processed until it completes.
	for range 1 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			cancel()
			close(resume)
			t.Fatal("oldest head did not reach real source read")
		}
	}
	recoveryStop(t, cancel, done)
	close(resume)
	objects.PauseReads(nil, nil)
	stopped := importSnapshot(t, a, apiUser)
	if len(stopped.Operations) != 0 || len(stopped.Uploads) != 3 {
		t.Fatal("cancelled worker published partial source facts")
	}
	for _, u := range stopped.Uploads {
		if u.Status != "processing" || u.FileDeletedAt != nil {
			t.Fatal("pod stop retired recoverable job", u.Status)
		}
	}
	ctx, cancel = context.WithCancel(t.Context())
	done = make(chan error, 1)
	go func() { done <- a.Imports.Run(ctx) }()
	recoveryWait(t, func() bool {
		state := importSnapshot(t, a, apiUser)
		if len(state.Uploads) != 3 {
			return false
		}
		for _, u := range state.Uploads {
			if u.Status != "done" {
				return false
			}
		}
		return true
	})
	recoveryStop(t, cancel, done)
	state := importSnapshot(t, a, apiUser)
	if len(state.Operations) != 1 || len(state.Sources) != 3 {
		t.Fatalf("restart duplicated/lost operations or source links: %d/%d", len(state.Operations), len(state.Sources))
	}
	var newCount int64
	for _, u := range state.Uploads {
		if u.Result == nil {
			t.Fatal("missing durable result")
		}
		newCount += u.Result.NewCount
	}
	if newCount != 1 {
		t.Fatal("one source published more than once", newCount)
	}
	// A new worker episode sees no pending jobs and cannot republish finished rows.
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if err := a.Imports.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	again := importSnapshot(t, a, apiUser)
	if len(again.Operations) != len(state.Operations) || len(again.Sources) != len(state.Sources) {
		t.Fatal("finished jobs replayed effects")
	}
	for _, id := range ids {
		if body, ok := objects.Bytes(apiUser + "/" + id); !ok || len(body) == 0 {
			t.Fatal("graceful stop erased admitted source")
		}
	}
}

func TestIntegrationOrphanSweepUsesActualPrimaryReferencesAndNamespace(t *testing.T) {
	var current atomic.Int64
	current.Store(time.Now().UnixNano())
	a, objects := importAssembly(t, func() time.Time { return time.Unix(0, current.Load()).UTC() })
	accepted, err := a.Imports.Upload(t.Context(), apiUser, importing.Header{FileName: "active.ofx"}, bytes.NewBufferString(apiOFX), []string{uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := a.Cfg.ObjectStorage
	store, err := objectstore.New(objectstore.Config{Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, KMSKeyID: cfg.KMSKeyID, Token: objectstore.FileToken(cfg.IAMTokenFile), Client: objects.Server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	orphan := uuid.NewString() + "/" + uuid.NewString()
	unknown := "non-uuid/opaque"
	future := uuid.NewString() + "/" + uuid.NewString()
	for _, key := range []string{orphan, unknown} {
		if err := store.Put(t.Context(), key, strings.NewReader("source"), 6); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Imports.SweepOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if body, _ := objects.Bytes(orphan); len(body) != 6 {
		t.Fatal("fresh orphan erased before admission grace")
	}
	current.Add(int64(2 * time.Hour))
	if err := a.Imports.SweepOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if body, ok := objects.Bytes(orphan); !ok || len(body) != 0 {
		t.Fatal("aged unreferenced object not permanently fenced")
	}
	if body, _ := objects.Bytes(unknown); len(body) != 6 {
		t.Fatal("foreign namespace erased")
	}
	if body, _ := objects.Bytes(apiUser + "/" + accepted.UploadID); len(body) == 0 {
		t.Fatal("active primary reference erased despite object age")
	}
	// Erasure failure leaves an orphan eligible for a later real sweep.
	if err := store.Put(t.Context(), future, strings.NewReader("source"), 6); err != nil {
		t.Fatal(err)
	}
	objects.SetErasureFailure(true)
	if err := a.Imports.SweepOrphans(t.Context()); err == nil {
		t.Fatal("cloud refusal acknowledged as deletion")
	}
	if body, _ := objects.Bytes(future); len(body) != 6 {
		t.Fatal("failed erasure lost source unexpectedly")
	}
	objects.SetErasureFailure(false)
	if err := a.Imports.SweepOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if body, ok := objects.Bytes(future); !ok || len(body) != 0 {
		t.Fatal("orphan recovery did not install fence")
	}
}
