package objectstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIntegrationDefaultEncryptionProofAcrossAuthenticatedTLS(t *testing.T) {
	for _, mode := range []string{"valid", "missing-default", "changed-key", "denied-config", "truncated-config", "missing-response-key", "wrong-response-algorithm", "duplicate-response-key"} {
		t.Run(mode, func(t *testing.T) {
			var writes, guards atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-YaCloud-SubjectToken") != "runtime-token" {
					t.Error("missing runtime identity")
					w.WriteHeader(403)
					return
				}
				if r.Header.Get(encryptionAlgorithm) != "" || r.Header.Get(encryptionKey) != "" {
					t.Error("explicit encryption sent")
				}
				if r.URL.Query().Has("encryption") {
					guards.Add(1)
					switch mode {
					case "missing-default":
						w.WriteHeader(404)
					case "changed-key":
						_, _ = io.WriteString(w, strings.ReplaceAll(encryptionXML, "kms-test", "changed"))
					case "denied-config":
						w.WriteHeader(403)
					case "truncated-config":
						w.Header().Set("Content-Length", "1024")
						_, _ = io.WriteString(w, "<partial>")
					default:
						_, _ = io.WriteString(w, encryptionXML)
					}
					return
				}
				if r.Method == http.MethodPut {
					writes.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
				}
				w.Header().Set(encryptionAlgorithm, "aws:kms")
				w.Header().Set(encryptionKey, "kms-test")
				switch mode {
				case "missing-response-key":
					w.Header().Del(encryptionKey)
				case "wrong-response-algorithm":
					w.Header().Set(encryptionAlgorithm, "AES256")
				case "duplicate-response-key":
					w.Header().Add(encryptionKey, "kms-test")
				}
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, "private-file")
				} else {
					w.WriteHeader(200)
				}
			}))
			defer server.Close()
			s, err := New(Config{Endpoint: server.URL, Bucket: "private-statements", KMSKeyID: "kms-test", Token: func(context.Context) (string, error) { return "runtime-token", nil }, Client: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			for _, write := range []func() error{
				func() error { return s.Put(t.Context(), "u/f", strings.NewReader("file"), 4) },
				func() error { return s.Delete(t.Context(), "u/f") },
			} {
				err = write()
				if mode == "valid" && err != nil || mode != "valid" && err == nil {
					t.Fatal("unverified write result", mode, err)
				}
			}
			configFailure := mode == "missing-default" || mode == "changed-key" || mode == "denied-config" || mode == "truncated-config"
			wantWrites := int32(2)
			if configFailure {
				wantWrites = 0
			}
			if guards.Load() != 2 || writes.Load() != wantWrites {
				t.Fatal("unsafe effects or cached default", guards.Load(), writes.Load())
			}
			if !configFailure {
				body, err := s.Open(t.Context(), "u/f")
				if mode != "valid" {
					if body != nil || !errors.Is(err, ErrUnavailable) {
						t.Fatal("unverified bytes exposed", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					bytes, readErr := io.ReadAll(body)
					_ = body.Close()
					if readErr != nil || string(bytes) != "private-file" {
						t.Fatal("verified body lost", readErr)
					}
				}
			}
		})
	}
}
