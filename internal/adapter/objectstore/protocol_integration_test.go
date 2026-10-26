package objectstore

import (
	"context"
	"crypto/md5" // #nosec G501 -- checks the S3 transfer-integrity header, not authentication.
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func protocolTLS(t *testing.T, handler http.HandlerFunc) (*Store, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	store, err := New(Config{Endpoint: server.URL, Bucket: "private-statements", KMSKeyID: "kms-test", Token: func(context.Context) (string, error) { return "protocol-token", nil }, Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return store, server
}

func TestIntegrationBucketSafetyUsesAuthenticatedTLSAndRejectsUnsafeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, kind, body string
		status           int
		want             error
	}{
		{"disabled", "versioning", `<VersioningConfiguration/>`, 200, nil},
		{"enabled", "versioning", `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`, 200, ErrConfig},
		{"suspended", "versioning", `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`, 200, ErrConfig},
		{"duplicate-status", "versioning", `<VersioningConfiguration><Status/><Status/></VersioningConfiguration>`, 200, ErrConfig},
		{"future-versioning", "versioning", `<VersioningConfiguration><Status>Future</Status></VersioningConfiguration>`, 200, ErrConfig},
		{"enabled-expiration", "lifecycle", `<LifecycleConfiguration><Rule><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`, 200, ErrConfig},
		{"disabled-expiration", "lifecycle", `<LifecycleConfiguration><Rule><Status>Disabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`, 200, nil},
		{"nonexpiring-enabled-rule", "lifecycle", `<LifecycleConfiguration><Rule><Status>Enabled</Status></Rule></LifecycleConfiguration>`, 200, nil},
		{"future-lifecycle", "lifecycle", `<LifecycleConfiguration><Rule><Status>Future</Status></Rule></LifecycleConfiguration>`, 200, ErrConfig},
		{"missing-lifecycle-status", "lifecycle", `<LifecycleConfiguration><Rule><Expiration/></Rule></LifecycleConfiguration>`, 200, ErrConfig},
		{"retention", "object-lock", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`, 200, ErrConfig},
		{"empty-lock", "object-lock", `<ObjectLockConfiguration/>`, 200, nil},
		{"version-denied", "versioning", "private-provider-diagnostics", 403, ErrUnavailable},
		{"lifecycle-denied", "lifecycle", "private-provider-diagnostics", 403, ErrUnavailable},
		{"lock-denied", "object-lock", "private-provider-diagnostics", 403, ErrUnavailable},
		{"mandatory-version-missing", "versioning", "", 404, ErrUnavailable},
		{"invalid-xml", "versioning", "not XML", 200, ErrUnavailable},
		{"wrong-root", "versioning", `<Other/>`, 200, ErrUnavailable},
		{"trailing-document", "versioning", `<VersioningConfiguration/><Other/>`, 200, ErrUnavailable},
		{"bounded-config", "versioning", strings.Repeat("x", (64<<10)+1), 200, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s, _ := protocolTLS(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/private-statements" || r.Header.Get("X-YaCloud-SubjectToken") != "protocol-token" || r.Header.Get("Authorization") != "" {
					t.Error("invalid authenticated bucket probe")
				}
				if r.URL.Query().Has(tc.kind) {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				if r.URL.Query().Has("versioning") {
					_, _ = io.WriteString(w, `<VersioningConfiguration/>`)
					return
				}
				w.WriteHeader(404)
			})
			err := s.CheckBucket(t.Context())
			if !errors.Is(err, tc.want) {
				t.Fatalf("bucket refusal=%v want=%v", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "private-provider") {
				t.Fatal("provider diagnostics escaped")
			}
			want := int32(3)
			if tc.want != nil {
				switch tc.kind {
				case "versioning":
					want = 1
				case "lifecycle":
					want = 2
				}
			}
			if calls.Load() != want {
				t.Fatal("unsafe bucket did not stop probing", calls.Load(), want)
			}
		})
	}
}

func TestIntegrationProviderFailuresNeverAuthorizeRetriesOrConfirmErasure(t *testing.T) {
	for _, operation := range []string{"create", "erase", "open", "list"} {
		for _, status := range []int{301, 403, 409, 412, 500} {
			t.Run(fmt.Sprintf("%s-%d", operation, status), func(t *testing.T) {
				var calls atomic.Int32
				s, _ := protocolTLS(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Location", "https://storage.invalid/private")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "private-provider-diagnostics")
				})
				var err error
				switch operation {
				case "create":
					err = s.Put(t.Context(), "user/file", strings.NewReader("fixture"), 7)
				case "erase":
					err = s.Delete(t.Context(), "user/file")
				case "open":
					_, err = s.Open(t.Context(), "user/file")
				case "list":
					_, err = s.List(t.Context(), "user/", "")
				}
				if !errors.Is(err, ErrUnavailable) || err.Error() != ErrUnavailable.Error() {
					t.Fatal("provider refusal was lost or leaked", err)
				}
				if calls.Load() != 1 {
					t.Fatal("uncertain operation automatically retried", calls.Load())
				}
			})
		}
	}
}

func TestIntegrationConditionalCreateAndPermanentFenceUseExactWireHeaders(t *testing.T) {
	var calls atomic.Int32
	var present bool
	var mu sync.Mutex
	var stored string
	s, _ := protocolTLS(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPut || r.URL.Path != "/private-statements/user/file" {
			t.Error("physical deletion or wrong key")
			w.WriteHeader(400)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error("request body failed")
			w.WriteHeader(400)
			return
		}
		checksum := md5.Sum(data) // #nosec G401 -- S3 checksum.
		if r.ContentLength != int64(len(data)) || r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(checksum[:]) || r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("X-Amz-Server-Side-Encryption") != "aws:kms" || r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != "kms-test" || r.Header.Get("X-YaCloud-SubjectToken") != "protocol-token" {
			t.Error("incomplete S3 integrity/encryption headers")
		}
		if len(data) > 0 {
			if r.Header.Get("If-None-Match") != "*" {
				t.Error("original PUT lacks create-only condition")
			}
			if present {
				w.WriteHeader(412)
				return
			}
		} else if r.Header.Get("If-None-Match") != "" {
			t.Error("erasure must overwrite the original")
		}
		stored = string(data)
		present = true
		w.WriteHeader(200)
	})
	if err := s.Put(t.Context(), "user/file", strings.NewReader("fixture"), 7); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(t.Context(), "user/file", strings.NewReader("different"), 9); !errors.Is(err, ErrUnavailable) {
		t.Fatal("duplicate file overwritten", err)
	}
	if err := s.Delete(t.Context(), "user/file"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(t.Context(), "user/file", strings.NewReader("late"), 4); !errors.Is(err, ErrUnavailable) {
		t.Fatal("late file passed permanent fence", err)
	}
	if err := s.Delete(t.Context(), "user/file"); err != nil {
		t.Fatal("erasure not repeatable", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls.Load() != 5 || !present || stored != "" {
		t.Fatal("fence removed or private content retained")
	}
}

func TestIntegrationOpenBoundsUnknownLengthAndRejectsTruncatedTransfer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		length   int64
		bodySize int
		chunked  bool
		want     error
	}{
		{"empty-marker", 0, 0, false, ErrNotFound},
		{"declared-oversize", maxObjectBytes + 1, 0, false, ErrUnavailable},
		{"exact-boundary", maxObjectBytes, int(maxObjectBytes), true, nil},
		{"chunked-overflow", -1, int(maxObjectBytes + 1), true, ErrUnavailable},
		{"short-transfer", 64, 7, false, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := protocolTLS(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.chunked {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				} else {
					w.Header().Set("Content-Length", strconv.FormatInt(tc.length, 10))
				}
				_, _ = io.Copy(w, io.LimitReader(strings.NewReader(strings.Repeat("x", tc.bodySize)), int64(tc.bodySize)))
			})
			body, err := s.Open(t.Context(), "user/file")
			if tc.name == "empty-marker" || tc.name == "declared-oversize" {
				if !errors.Is(err, tc.want) || body != nil {
					t.Fatal("unsafe body exposed", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if n, readErr := body.Read(nil); n != 0 || readErr != nil {
				t.Fatal("empty read consumed source")
			}
			n, readErr := io.Copy(io.Discard, body)
			if closeErr := body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if !errors.Is(readErr, tc.want) {
				t.Fatal("transfer integrity not enforced", readErr)
			}
			expected := int64(tc.bodySize)
			if expected > maxObjectBytes {
				expected = maxObjectBytes
			}
			if n != expected {
				t.Fatal("incorrect read bound", n, expected)
			}
		})
	}
}

func TestIntegrationListRejectsUnsafePagesAndPreservesMarkerContinuation(t *testing.T) {
	base := `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next+opaque=</NextContinuationToken><Contents><Key>user/file</Key><LastModified>2026-10-01T00:00:00Z</LastModified><Size>7</Size></Contents></ListBucketResult>`
	for _, tc := range []struct{ name, body string }{
		{"broken", "<ListBucketResult>"}, {"extra", base + "<Other/>"},
		{"foreign-key", strings.ReplaceAll(base, "user/file", "foreign/file")},
		{"traversal", strings.ReplaceAll(base, "user/file", "user/../file")},
		{"missing-size", strings.ReplaceAll(base, "<Size>7</Size>", "")},
		{"negative-size", strings.ReplaceAll(base, "<Size>7</Size>", "<Size>-1</Size>")},
		{"bad-date", strings.ReplaceAll(base, "2026-10-01T00:00:00Z", "not-a-date")},
		{"zero-date", strings.ReplaceAll(base, "2026-10-01T00:00:00Z", "0001-01-01T00:00:00Z")},
		{"missing-next", strings.ReplaceAll(base, "next+opaque=", "")},
		{"repeated-next", strings.ReplaceAll(base, "next+opaque=", "previous")},
		{"untruncated-next", strings.ReplaceAll(base, "<IsTruncated>true", "<IsTruncated>false")},
		{"oversized-next", strings.ReplaceAll(base, "next+opaque=", strings.Repeat("x", 2049))},
		{"more-than-page", `<ListBucketResult>` + strings.Repeat(`<Contents><Key>user/file</Key><LastModified>2026-10-01T00:00:00Z</LastModified><Size>7</Size></Contents>`, 1001) + `</ListBucketResult>`},
		{"oversized-body", strings.Repeat("x", int(maxListBytes)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := protocolTLS(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, tc.body) })
			page, err := s.List(t.Context(), "user/", "previous")
			if !errors.Is(err, ErrUnavailable) || len(page.Objects) != 0 || page.Next != "" {
				t.Fatal("unsafe provider page exposed", err)
			}
		})
	}
	t.Run("marker-page", func(t *testing.T) {
		var requests atomic.Int32
		s, _ := protocolTLS(t, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			q := r.URL.Query()
			if q.Get("list-type") != "2" || q.Get("max-keys") != "1000" || q.Get("prefix") != "user/" {
				t.Error("invalid provider query")
			}
			if q.Get("continuation-token") == "previous+opaque=" {
				_, _ = io.WriteString(w, strings.ReplaceAll(base, "<Size>7</Size>", "<Size>0</Size>"))
				return
			}
			if q.Get("continuation-token") != "next+opaque=" {
				t.Error("opaque token changed")
			}
			_, _ = io.WriteString(w, strings.ReplaceAll(strings.ReplaceAll(base, "<IsTruncated>true", "<IsTruncated>false"), "<NextContinuationToken>next+opaque=</NextContinuationToken>", ""))
		})
		first, err := s.List(t.Context(), "user/", "previous+opaque=")
		if err != nil || len(first.Objects) != 0 || first.Next != "next+opaque=" {
			t.Fatal("marker lost continuation", err)
		}
		second, err := s.List(t.Context(), "user/", first.Next)
		if err != nil || len(second.Objects) != 1 || second.Objects[0].Key != "user/file" || second.Next != "" || requests.Load() != 2 {
			t.Fatal("continuation failed", err)
		}
	})
}

func TestIntegrationStorageCancellationAndTruncatedXMLAreBounded(t *testing.T) {
	for _, operation := range []string{"create", "erase", "open", "list", "bucket"} {
		t.Run(operation, func(t *testing.T) {
			entered := make(chan struct{})
			finished := make(chan struct{})
			release := make(chan struct{})
			s, _ := protocolTLS(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(entered)
				select {
				case <-r.Context().Done():
				case <-release:
				}
				close(finished)
			})
			t.Cleanup(func() { close(release) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "create":
					err = s.Put(ctx, "user/file", strings.NewReader("fixture"), 7)
				case "erase":
					err = s.Delete(ctx, "user/file")
				case "open":
					_, err = s.Open(ctx, "user/file")
				case "list":
					_, err = s.List(ctx, "user/", "")
				case "bucket":
					err = s.CheckBucket(ctx)
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("request did not reach provider")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled adapter did not return")
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("provider request not cancelled")
			}
		})
	}
	for _, operation := range []string{"list", "bucket"} {
		t.Run(operation+"-truncated", func(t *testing.T) {
			s, _ := protocolTLS(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "1024")
				_, _ = io.WriteString(w, "<partial>")
			})
			var err error
			if operation == "bucket" {
				err = s.CheckBucket(t.Context())
			} else {
				_, err = s.List(t.Context(), "user/", "")
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatal("short XML transfer accepted", err)
			}
		})
	}
}

func TestIntegrationStorageReloadsIAMFileAndRejectsInvalidHeadersBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	var last atomic.Value
	s, _ := protocolTLS(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		last.Store(r.Header.Get("X-YaCloud-SubjectToken"))
		w.WriteHeader(200)
	})
	path := filepath.Join(t.TempDir(), "iam")
	s.token = FileToken(path)
	for _, token := range []string{"first-token\n", "rotated-token\n"} {
		if err := os.WriteFile(path, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(t.Context(), "user/file"); err != nil {
			t.Fatal(err)
		}
		if last.Load() != strings.TrimSpace(token) {
			t.Fatal("rotated IAM token not reloaded")
		}
	}
	for _, token := range []string{"", "bad\r\nInjected: yes", strings.Repeat("x", 16385)} {
		if err := os.WriteFile(path, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(t.Context(), "user/file"); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), "user/file"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing IAM file accepted", err)
	}
	if requests.Load() != 2 {
		t.Fatal("invalid credential crossed network boundary", requests.Load())
	}
}

func TestIntegrationInvalidUploadsAndListSelectorsHaveNoProviderEffects(t *testing.T) {
	var calls atomic.Int32
	s, _ := protocolTLS(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) })
	for _, tc := range []struct {
		name, key string
		body      io.Reader
		size      int64
	}{
		{"nil", "user/file", nil, 7},
		{"negative", "user/file", strings.NewReader("fixture"), -1},
		{"oversize", "user/file", strings.NewReader("fixture"), maxObjectBytes + 1},
		{"empty", "user/file", strings.NewReader(""), 0},
		{"short", "user/file", strings.NewReader("fixture"), 8},
		{"long", "user/file", strings.NewReader("fixture"), 6},
		{"traversal", "user/../file", strings.NewReader("fixture"), 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Put(t.Context(), tc.key, tc.body, tc.size); !errors.Is(err, ErrInput) {
				t.Fatal("invalid original reached provider", err)
			}
		})
	}
	for _, key := range []string{"../file", "user//file"} {
		if _, err := s.Open(t.Context(), key); !errors.Is(err, ErrInput) {
			t.Fatal(err)
		}
		if err := s.Delete(t.Context(), key); !errors.Is(err, ErrInput) {
			t.Fatal(err)
		}
	}
	for _, prefix := range []string{"user", "../", "user//"} {
		if _, err := s.List(t.Context(), prefix, ""); !errors.Is(err, ErrInput) {
			t.Fatal(err)
		}
	}
	if _, err := s.List(t.Context(), "user/", strings.Repeat("x", 2049)); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid local input caused remote effects", calls.Load())
	}
}

func TestIntegrationSlowProviderRespectsDeadlinesBeforeAndAfterHeaders(t *testing.T) {
	for _, phase := range []string{"bucket", "open", "erase", "stream-body"} {
		t.Run(phase, func(t *testing.T) {
			finished := make(chan struct{})
			release := make(chan struct{})
			s, _ := protocolTLS(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if phase == "stream-body" {
					w.WriteHeader(200)
					_, _ = io.WriteString(w, "fixture")
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
				close(finished)
			})
			t.Cleanup(func() { close(release) })
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			var err error
			switch phase {
			case "bucket":
				err = s.CheckBucket(ctx)
			case "erase":
				err = s.Delete(ctx, "user/file")
			default:
				var body io.ReadCloser
				body, err = s.Open(ctx, "user/file")
				if phase == "stream-body" {
					if err != nil {
						t.Fatal("provider never opened response", err)
					}
					n, readErr := io.Copy(io.Discard, body)
					err = readErr
					_ = body.Close()
					if n != 7 {
						t.Fatal("partial body lost", n)
					}
				}
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("caller budget lost", err)
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("provider request remained active after deadline")
			}
		})
	}
}
