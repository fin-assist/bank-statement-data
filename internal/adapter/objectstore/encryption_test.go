package objectstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Unlike testConfig, this seam supplies no implicit encryption metadata.
func encryptionStore(t *testing.T, transport roundTrip) *Store {
	t.Helper()
	s, err := New(Config{Endpoint: "https://storage.example", Bucket: "private-statements", KMSKeyID: "kms-test", Token: func(context.Context) (string, error) { return "test-token", nil }, Client: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func encryptedReply(code int, body string) *http.Response {
	response := reply(code, body)
	response.Header.Set(encryptionAlgorithm, "aws:kms")
	response.Header.Set(encryptionKey, "kms-test")
	return response
}

func TestUnitDefaultEncryptionRejectsUnsafeOrUnverifiableSettingsBeforeWrites(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
		want       error
	}{
		{"correct", encryptionXML, 200, nil},
		{"namespace", strings.Replace(encryptionXML, "<ServerSideEncryptionConfiguration>", `<ServerSideEncryptionConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`, 1), 200, nil},
		{"missing", `<ServerSideEncryptionConfiguration/>`, 200, ErrConfig},
		{"missing-default", `<ServerSideEncryptionConfiguration><Rule/></ServerSideEncryptionConfiguration>`, 200, ErrConfig},
		{"duplicate-rule", strings.Replace(encryptionXML, "</Rule>", `</Rule><Rule/>`, 1), 200, ErrConfig},
		{"duplicate-default", strings.Replace(encryptionXML, "</Rule>", `<ApplyServerSideEncryptionByDefault/></Rule>`, 1), 200, ErrConfig},
		{"missing-algorithm", strings.ReplaceAll(encryptionXML, `<SSEAlgorithm>aws:kms</SSEAlgorithm>`, ""), 200, ErrConfig},
		{"wrong-algorithm", strings.ReplaceAll(encryptionXML, "aws:kms", "AES256"), 200, ErrConfig},
		{"duplicate-algorithm", strings.ReplaceAll(encryptionXML, `</SSEAlgorithm>`, `</SSEAlgorithm><SSEAlgorithm>aws:kms</SSEAlgorithm>`), 200, ErrConfig},
		{"missing-key", strings.ReplaceAll(encryptionXML, `<KMSMasterKeyID>kms-test</KMSMasterKeyID>`, ""), 200, ErrConfig},
		{"wrong-key", strings.ReplaceAll(encryptionXML, "kms-test", "other-key"), 200, ErrConfig},
		{"duplicate-key", strings.ReplaceAll(encryptionXML, `</KMSMasterKeyID>`, `</KMSMasterKeyID><KMSMasterKeyID>kms-test</KMSMasterKeyID>`), 200, ErrConfig},
		{"no-config", "", 404, ErrUnavailable},
		{"denied", "private-provider-diagnostics", 403, ErrUnavailable},
		{"unavailable", "private-provider-diagnostics", 503, ErrUnavailable},
		{"malformed", "<ServerSideEncryptionConfiguration>", 200, ErrUnavailable},
		{"wrong-root", "<Other/>", 200, ErrUnavailable},
		{"trailing-document", encryptionXML + "<Other/>", 200, ErrUnavailable},
		{"oversized", strings.Repeat("x", (64<<10)+1), 200, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			puts, guards := 0, 0
			s := encryptionStore(t, func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("X-YaCloud-SubjectToken") != "test-token" {
					t.Fatal("unauthenticated storage request")
				}
				if r.URL.Query().Has("encryption") {
					guards++
					return reply(tc.code, tc.body), nil
				}
				if r.Method == http.MethodPut {
					puts++
					if r.Header.Get(encryptionAlgorithm) != "" || r.Header.Get(encryptionKey) != "" {
						t.Fatal("requires configuration-write role")
					}
					return encryptedReply(200, ""), nil
				}
				if r.URL.Query().Has("versioning") {
					return reply(200, "<VersioningConfiguration/>"), nil
				}
				return reply(404, ""), nil
			})
			for _, err := range []error{s.CheckBucket(t.Context()), s.Put(t.Context(), "user/file", strings.NewReader("x"), 1), s.Delete(t.Context(), "user/file")} {
				if !errors.Is(err, tc.want) {
					t.Fatalf("got %v want %v", err, tc.want)
				}
			}
			wantPuts := 0
			if tc.want == nil {
				wantPuts = 2
			}
			if puts != wantPuts || guards != 3 {
				t.Fatalf("unsafe write/cached default puts=%d guards=%d", puts, guards)
			}
		})
	}
}

func TestUnitDefaultEncryptionIsRecheckedAndTokensReloadBetweenGuardAndPut(t *testing.T) {
	tokens, guards, puts := 0, 0, 0
	s := encryptionStore(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Has("encryption") {
			guards++
			if guards > 1 {
				return reply(200, strings.ReplaceAll(encryptionXML, "kms-test", "changed-key")), nil
			}
			if r.Header.Get("X-YaCloud-SubjectToken") != "first" {
				t.Fatal("guard token not loaded")
			}
			return reply(200, encryptionXML), nil
		}
		puts++
		if r.Header.Get("X-YaCloud-SubjectToken") != "rotated" {
			t.Fatal("PUT reused guard token")
		}
		return encryptedReply(200, ""), nil
	})
	s.token = func(context.Context) (string, error) {
		tokens++
		if tokens == 1 {
			return "first", nil
		}
		return "rotated", nil
	}
	if err := s.Put(t.Context(), "u/f", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context(), "u/f"); !errors.Is(err, ErrConfig) {
		t.Fatal("changed key accepted", err)
	}
	if guards != 2 || puts != 1 || tokens != 3 {
		t.Fatal("cached configuration/token or unsafe fence", guards, puts, tokens)
	}
}

func TestUnitEncryptionResponseProofIsRequiredForFilesAndEmptyFences(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(http.Header)
	}{
		{"valid", func(http.Header) {}},
		{"missing-algorithm", func(h http.Header) { h.Del(encryptionAlgorithm) }},
		{"missing-key", func(h http.Header) { h.Del(encryptionKey) }},
		{"wrong-algorithm", func(h http.Header) { h.Set(encryptionAlgorithm, "AES256") }},
		{"wrong-key", func(h http.Header) { h.Set(encryptionKey, "other") }},
		{"duplicate-algorithm", func(h http.Header) { h.Add(encryptionAlgorithm, "aws:kms") }},
		{"duplicate-key", func(h http.Header) { h.Add(encryptionKey, "kms-test") }},
		{"comma-joined-key", func(h http.Header) { h.Set(encryptionKey, "kms-test,other") }},
	} {
		for _, operation := range []string{"put", "fence", "open", "empty-open"} {
			t.Run(change.name+"/"+operation, func(t *testing.T) {
				var raw *budgetBody
				s := encryptionStore(t, func(r *http.Request) (*http.Response, error) {
					if r.URL.Query().Has("encryption") {
						return reply(200, encryptionXML), nil
					}
					response := encryptedReply(200, "private-bytes")
					if operation == "empty-open" {
						response = encryptedReply(200, "")
					}
					change.mutate(response.Header)
					raw = &budgetBody{read: response.Body.Read}
					response.Body = raw
					return response, nil
				})
				var err error
				switch operation {
				case "put":
					err = s.Put(t.Context(), "u/f", strings.NewReader("x"), 1)
				case "fence":
					err = s.Delete(t.Context(), "u/f")
				default:
					var body io.ReadCloser
					body, err = s.Open(t.Context(), "u/f")
					if change.name != "valid" && body != nil {
						t.Fatal("unverified bytes exposed")
					}
					if body != nil {
						_ = body.Close()
					}
				}
				want := error(nil)
				if change.name != "valid" {
					want = ErrUnavailable
				} else if operation == "empty-open" {
					want = ErrNotFound
				}
				if !errors.Is(err, want) || raw == nil || !raw.closed {
					t.Fatal("encryption proof/body ownership lost", err, want)
				}
			})
		}
	}
}

func TestUnitEncryptionPreflightAndPutShareOneDeadline(t *testing.T) {
	var guardDeadline time.Time
	puts := 0
	s := encryptionStore(t, func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("unbounded storage operation")
		}
		if r.URL.Query().Has("encryption") {
			guardDeadline = deadline
			return reply(200, encryptionXML), nil
		}
		puts++
		if deadline != guardDeadline {
			t.Fatal("PUT received a fresh timeout budget")
		}
		<-r.Context().Done()
		return encryptedReply(200, ""), nil
	})
	s.client.Timeout = 50 * time.Millisecond
	if err := s.Put(t.Context(), "u/f", strings.NewReader("x"), 1); !errors.Is(err, context.DeadlineExceeded) || puts != 1 {
		t.Fatal("late success accepted", err, puts)
	}
}
