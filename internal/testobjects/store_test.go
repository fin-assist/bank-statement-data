package testobjects

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/adapter/objectstore"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

// The same fixture underlies full-service API claims. Assert its refusal and CAS
// semantics independently, so a permissive fake cannot turn an unsafe service green.
func TestUnitFixturePreservesCloudConditionalWriteAndErasureSemantics(t *testing.T) {
	fixture, client := memoryFixture(t)
	verifyFixtureCAS(t, fixture, client)
}

func TestIntegrationFixturePreservesActualTLSCloudBoundary(t *testing.T) {
	fixture := New(t)
	verifyFixtureCAS(t, fixture, fixture.Server.Client())
}

func verifyFixtureCAS(t *testing.T, fixture *Store, client *http.Client) {
	t.Helper()
	var cfg config.Config
	fixture.Configure(&cfg)
	store, err := objectstore.New(objectstore.Config{Endpoint: cfg.ObjectStorage.Endpoint, Bucket: cfg.ObjectStorage.Bucket, KMSKeyID: cfg.ObjectStorage.KMSKeyID, Token: objectstore.FileToken(cfg.ObjectStorage.IAMTokenFile), Client: client})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CheckBucket(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := "73ef1f95-14d5-453c-80ec-d0e2ac8c854d/b7a17bfd-cf78-44a3-95f7-09941c59e996"
	original := []byte("private synthetic file")

	fixture.SetCreationFailure(true)
	if err = store.Put(t.Context(), key, bytes.NewReader(original), int64(len(original))); err == nil {
		t.Fatal("fixture hid provider create refusal")
	}
	if _, ok := fixture.Bytes(key); ok {
		t.Fatal("refused create retained data")
	}
	fixture.SetCreationFailure(false)
	if err = store.Put(t.Context(), key, bytes.NewReader(original), int64(len(original))); err != nil {
		t.Fatal(err)
	}
	if err = store.Put(t.Context(), key, strings.NewReader("replacement"), 11); err == nil {
		t.Fatal("fixture allowed overwrite of an admitted key")
	}
	copy, ok := fixture.Bytes(key)
	if !ok || !bytes.Equal(copy, original) {
		t.Fatal("fixture changed original")
	}
	copy[0] = 'x'
	stored, _ := fixture.Bytes(key)
	if !bytes.Equal(stored, original) {
		t.Fatal("fixture leaked mutable object storage")
	}
	opened, err := store.Open(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil || !bytes.Equal(body, original) {
		t.Fatal("GET corrupted bytes")
	}
	page, err := store.List(t.Context(), "73ef1f95-14d5-453c-80ec-d0e2ac8c854d/", "")
	if err != nil || len(page.Objects) != 1 || page.Objects[0].Key != key || page.Objects[0].ModifiedAt.IsZero() {
		t.Fatalf("fixture list: %+v %v", page, err)
	}
	fixture.SetErasureFailure(true)
	if err = store.Delete(t.Context(), key); err == nil {
		t.Fatal("fixture swallowed provider erasure refusal")
	}
	fixture.SetErasureFailure(false)

	entered, resume := make(chan struct{}, 1), make(chan struct{})
	fixture.PauseReads(entered, resume)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type readResult struct {
		body []byte
		err  error
	}
	result := make(chan readResult, 1)
	go func() {
		reader, err := store.Open(ctx, key)
		if err != nil {
			result <- readResult{err: err}
			return
		}
		body, err := io.ReadAll(reader)
		_ = reader.Close()
		result <- readResult{body, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fixture GET did not capture its snapshot")
	}
	if err = store.Delete(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	close(resume)
	fixture.PauseReads(nil, nil)
	select {
	case got := <-result:
		if got.err != nil || !bytes.Equal(got.body, original) {
			t.Fatalf("in-flight GET lost its old snapshot: %q %v", got.body, got.err)
		}
	case <-ctx.Done():
		t.Fatal("fixture stranded GET across erasure")
	}

	if err = store.Put(t.Context(), key, bytes.NewReader(original), int64(len(original))); err == nil {
		t.Fatal("fixture erased the permanent late-write fence")
	}
	if _, err = store.Open(t.Context(), key); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("zero fence visible as file: %v", err)
	}
	page, err = store.List(t.Context(), "", "")
	if err != nil || len(page.Objects) != 0 {
		t.Fatalf("zero fence in file list: %+v %v", page, err)
	}
	if _, err = store.Open(t.Context(), key+"/missing"); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("missing object: %v", err)
	}
	if _, ok = fixture.Bytes("missing"); ok {
		t.Fatal("invented object")
	}
}

func TestUnitFixtureDoesNotAcceptInvalidObjectRequests(t *testing.T) {
	fixture, _ := memoryFixture(t)
	for _, tc := range []struct {
		name, method, target, token, kms, body string
		length                                 int64
		code                                   int
	}{
		{"unauthenticated", http.MethodGet, "/private?versioning", "", "", "", 0, 403},
		{"unsupported bucket method", http.MethodPost, "/private", "fixture-iam-token", "", "", 0, 405},
		{"unknown query", http.MethodGet, "/private?other", "fixture-iam-token", "", "", 0, 400},
		{"foreign bucket", http.MethodGet, "/foreign/file", "fixture-iam-token", "", "", 0, 404},
		{"missing encryption", http.MethodPut, "/private/key", "fixture-iam-token", "", "a", 1, 400},
		{"bad KMS", http.MethodPut, "/private/key", "fixture-iam-token", "wrong", "a", 1, 400},
		{"size mismatch", http.MethodPut, "/private/key", "fixture-iam-token", "fixture-key", "abc", 2, 400},
		{"unsupported object method", http.MethodHead, "/private/key", "fixture-iam-token", "", "", 0, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, fixture.Server.URL+tc.target, strings.NewReader(tc.body)).WithContext(context.Background())
			req.ContentLength = tc.length
			req.Header.Set("X-YaCloud-SubjectToken", tc.token)
			if tc.kms != "" {
				req.Header.Set("X-Amz-Server-Side-Encryption", "aws:kms")
				req.Header.Set("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", tc.kms)
			}
			reply := httptest.NewRecorder()
			fixture.Server.Config.Handler.ServeHTTP(reply, req)
			if reply.Code != tc.code {
				t.Fatalf("fixture weakened rejection: %d", reply.Code)
			}
			if _, ok := fixture.Bytes("key"); ok {
				t.Fatal("refused request created object")
			}
		})
	}
}

// A declared in-memory HTTP transport exercises protocol behavior without a socket.
// Runtime/New and the TLS path above retain their real listener in Integration/API.
type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func memoryFixture(t *testing.T) (*Store, *http.Client) {
	t.Helper()
	s := &Store{data: map[string][]byte{}, modified: map[string]time.Time{}, TokenFile: filepath.Join(t.TempDir(), "iam")}
	if err := os.WriteFile(s.TokenFile, []byte("fixture-iam-token"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Server = &httptest.Server{Config: &http.Server{Handler: http.HandlerFunc(s.serve)}, URL: "https://objects.test"}
	client := &http.Client{Transport: fixtureTransport(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		s.serve(recorder, r)
		return recorder.Result(), nil
	})}
	return s, client
}
