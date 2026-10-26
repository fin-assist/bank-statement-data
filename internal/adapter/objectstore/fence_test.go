package objectstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestUnitFileErasureFencesLateCreates(t *testing.T) {
	objects := make(map[string]string)
	s := testStore(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			data, ok := objects[r.URL.Path]
			if !ok {
				return reply(404, ""), nil
			}
			return reply(200, data), nil
		}
		if r.Method != http.MethodPut {
			t.Fatal("physical DELETE would remove the fence")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if r.Header.Get("X-Amz-Server-Side-Encryption") != "aws:kms" || r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != "kms-test" {
			t.Fatal("unencrypted fence or file")
		}
		if len(data) > 0 {
			if r.Header.Get("If-None-Match") != "*" {
				t.Fatal("file write is not conditional")
			}
			if _, exists := objects[r.URL.Path]; exists {
				return reply(412, "private provider diagnostics"), nil
			}
		} else if r.Header.Get("If-None-Match") != "" || r.ContentLength != 0 {
			t.Fatal("fence cannot overwrite existing contents")
		}
		objects[r.URL.Path] = string(data)
		return reply(200, ""), nil
	})
	if err := s.Put(t.Context(), "u/f", strings.NewReader("private bytes"), 13); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Delete(t.Context(), "u/f"); err != nil {
			t.Fatal(err)
		}
	}
	if data, exists := objects["/private-statements/u/f"]; !exists || data != "" {
		t.Fatal("erasure removed fence or retained private contents")
	}
	if _, err := s.Open(t.Context(), "u/f"); !errors.Is(err, ErrNotFound) {
		t.Fatal("erased file is readable", err)
	}
	if err := s.Put(t.Context(), "u/f", strings.NewReader("late private bytes"), 18); !errors.Is(err, ErrUnavailable) {
		t.Fatal("late create passed the erasure fence", err)
	}
	if err := s.Delete(t.Context(), "u/missing"); err != nil {
		t.Fatal("missing-file erasure failed", err)
	}
	if err := s.Put(t.Context(), "u/empty", strings.NewReader(""), 0); !errors.Is(err, ErrInput) {
		t.Fatal("empty file aliases erasure marker", err)
	}
}

func TestIntegrationErasureSurvivesCloudCommitAfterClientCancellation(t *testing.T) {
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan int, 1)
	var releaseOnce sync.Once
	releasePut := func() { releaseOnce.Do(func() { close(release) }) }
	var mu sync.Mutex
	objects := make(map[string][]byte)
	revision := make(map[string]int)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-YaCloud-SubjectToken") != "test-token" {
			w.WriteHeader(403)
			return
		}
		if r.Method == http.MethodGet {
			if r.URL.Query().Has("versioning") {
				_, _ = io.WriteString(w, `<VersioningConfiguration/>`)
				return
			}
			if r.URL.Query().Has("lifecycle") || r.URL.Query().Has("object-lock") {
				w.WriteHeader(404)
				return
			}
			mu.Lock()
			data, ok := objects[r.URL.Path]
			data = append([]byte(nil), data...)
			mu.Unlock()
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = w.Write(data)
			return
		}
		if r.Method != http.MethodPut {
			w.WriteHeader(405)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		if len(data) == 0 {
			if r.Header.Get("If-None-Match") != "" {
				t.Error("erasure must overwrite")
			}
			mu.Lock()
			objects[r.URL.Path] = nil
			revision[r.URL.Path]++
			mu.Unlock()
			w.WriteHeader(200)
			return
		}
		if r.Header.Get("If-None-Match") != "*" {
			t.Error("unconditional original file PUT")
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		before := revision[r.URL.Path]
		_, exists := objects[r.URL.Path]
		mu.Unlock()
		if exists {
			w.WriteHeader(412)
			return
		}
		close(started)
		// The cloud has received the body and deliberately continues processing
		// after its caller is gone. Client cancellation cannot prove its outcome.
		<-release
		mu.Lock()
		code := 200
		if revision[r.URL.Path] != before {
			code = 409
		} else if _, exists := objects[r.URL.Path]; exists {
			code = 412
		} else {
			objects[r.URL.Path] = data
			revision[r.URL.Path]++
		}
		mu.Unlock()
		finished <- code
		w.WriteHeader(code)
	}))
	defer server.Close()
	defer releasePut()
	c := testConfig(nil)
	c.Endpoint = server.URL
	c.Client = server.Client()
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CheckBucket(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- s.Put(ctx, "u/f", strings.NewReader("private statement"), 17) }()
	select {
	case <-started:
	case <-t.Context().Done():
		t.Fatal("cloud never received upload")
	}
	cancel()
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatal("PUT cancellation lost", err)
	}
	if err = s.Delete(t.Context(), "u/f"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Open(t.Context(), "u/f"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	releasePut()
	if code := <-finished; code != 409 {
		t.Fatal("cloud did not enforce concurrent conditional-write conflict", code)
	}
	mu.Lock()
	data, exists := objects["/private-statements/u/f"]
	mu.Unlock()
	if !exists || len(data) != 0 {
		t.Fatal("late cloud write resurrected statement")
	}
	if _, err = s.Open(t.Context(), "u/f"); !errors.Is(err, ErrNotFound) {
		t.Fatal("erased contents reappeared", err)
	}
}

func TestUnitListHidesFencesButRetainsPagination(t *testing.T) {
	page1 := `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken><Contents><Key>u/f</Key><Size>0</Size><LastModified>2026-10-01T00:00:00Z</LastModified></Contents></ListBucketResult>`
	page2 := `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>u/live</Key><Size>20</Size><LastModified>2026-10-01T00:00:00Z</LastModified></Contents></ListBucketResult>`
	s := testStore(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("continuation-token") == "next" {
			return reply(200, page2), nil
		}
		return reply(200, page1), nil
	})
	first, err := s.List(t.Context(), "u/", "")
	if err != nil || len(first.Objects) != 0 || first.Next != "next" {
		t.Fatalf("empty marker page lost cursor %+v %v", first, err)
	}
	second, err := s.List(t.Context(), "u/", first.Next)
	if err != nil || len(second.Objects) != 1 || second.Objects[0].Key != "u/live" || second.Next != "" {
		t.Fatal(second, err)
	}
	for _, size := range []string{"", "<Size>-1</Size>"} {
		s := testStore(t, func(*http.Request) (*http.Response, error) {
			return reply(200, strings.Replace(page2, "<Size>20</Size>", size, 1)), nil
		})
		if _, err := s.List(t.Context(), "u/", ""); !errors.Is(err, ErrUnavailable) {
			t.Fatal("untrusted size accepted", err)
		}
	}
}

func TestUnitBucketErasurePreconditions(t *testing.T) {
	for _, tc := range []struct {
		version, lifecycle, lock string
		want                     error
	}{
		{version: `<VersioningConfiguration/>`},
		{version: `<VersioningConfiguration/>`, lifecycle: `<LifecycleConfiguration><Rule><Status>Disabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`},
		{version: `<VersioningConfiguration/>`, lifecycle: `<LifecycleConfiguration><Rule><Status>Enabled</Status></Rule></LifecycleConfiguration>`, lock: `<ObjectLockConfiguration/>`},
		{version: `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, want: ErrConfig},
		{version: `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`, want: ErrConfig},
		{version: `<VersioningConfiguration><Status>new-unknown</Status></VersioningConfiguration>`, want: ErrConfig},
		{version: `<VersioningConfiguration><Status>Enabled</Status><Status/></VersioningConfiguration>`, want: ErrConfig},
		{version: `<VersioningConfiguration/>`, lifecycle: `<LifecycleConfiguration><Rule><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`, want: ErrConfig},
		{version: `<VersioningConfiguration/>`, lifecycle: `<LifecycleConfiguration><Rule><Status>unknown</Status></Rule></LifecycleConfiguration>`, want: ErrConfig},
		{version: `<VersioningConfiguration/>`, lock: `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`, want: ErrConfig},
		{version: "bad XML", want: ErrUnavailable},
		{version: `<VersioningConfiguration/><extra/>`, want: ErrUnavailable},
		{version: strings.Repeat("x", (64<<10)+1), want: ErrUnavailable},
		{want: ErrUnavailable},
	} {
		s := testStore(t, func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.URL.Path != "/private-statements" || r.Header.Get("X-YaCloud-SubjectToken") != "test-token" {
				t.Fatal("invalid bucket probe")
			}
			body := ""
			switch {
			case r.URL.Query().Has("versioning"):
				body = tc.version
			case r.URL.Query().Has("lifecycle"):
				body = tc.lifecycle
			case r.URL.Query().Has("object-lock"):
				body = tc.lock
			default:
				t.Fatal("unknown subresource")
			}
			if body == "" {
				return reply(404, ""), nil
			}
			return reply(200, body), nil
		})
		if err := s.CheckBucket(t.Context()); !errors.Is(err, tc.want) {
			t.Fatalf("bucket check expected %v got %v", tc.want, err)
		}
	}
	for _, kind := range []string{"versioning", "lifecycle", "object-lock"} {
		s := testStore(t, func(r *http.Request) (*http.Response, error) {
			if r.URL.Query().Has(kind) {
				return reply(403, "private denied diagnostics"), nil
			}
			if r.URL.Query().Has("versioning") {
				return reply(200, `<VersioningConfiguration/>`), nil
			}
			return reply(404, ""), nil
		})
		if err := s.CheckBucket(t.Context()); !errors.Is(err, ErrUnavailable) {
			t.Fatal("probe denial ignored", err)
		}
	}
	s := testStore(t, func(*http.Request) (*http.Response, error) {
		r := reply(200, "")
		r.Body = io.NopCloser(errReader{})
		return r, nil
	})
	if err := s.CheckBucket(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	s = testStore(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("private transport diagnostics") })
	if err := s.CheckBucket(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	c := testConfig(func(*http.Request) (*http.Response, error) {
		t.Fatal("sent request after cancellation")
		return nil, nil
	})
	c.Token = func(ctx context.Context) (string, error) { return "", ctx.Err() }
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = s.CheckBucket(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
