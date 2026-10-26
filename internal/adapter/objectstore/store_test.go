package objectstore

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Header: make(http.Header)}
}
func testConfig(fn roundTrip) Config {
	return Config{Endpoint: "https://storage.example", Bucket: "private-statements", KMSKeyID: "kms-test", Token: func(context.Context) (string, error) { return "test-token", nil }, Client: &http.Client{Transport: fn}}
}
func testStore(t *testing.T, fn roundTrip) *Store {
	t.Helper()
	s, e := New(testConfig(fn))
	if e != nil {
		t.Fatal(e)
	}
	return s
}

func TestUnitConfigAndBoundaries(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.Endpoint = "http://storage.example" }, func(c *Config) { c.Endpoint = "https://user:secret@storage.example" },
		func(c *Config) { c.Endpoint = "https://storage.example/path" }, func(c *Config) { c.Endpoint = "https://storage.example/?secret=true" },
		func(c *Config) { c.Endpoint = "https://storage.example/#fragment" }, func(c *Config) { c.Endpoint = "%" },
		func(c *Config) { c.Bucket = "x" }, func(c *Config) { c.Bucket = "-bad" }, func(c *Config) { c.Bucket = "bad..bucket" },
		func(c *Config) { c.Bucket = "UpperCase" }, func(c *Config) { c.Bucket = strings.Repeat("x", 64) },
		func(c *Config) { c.KMSKeyID = "" }, func(c *Config) { c.KMSKeyID = "key\r\ninjected" }, func(c *Config) { c.Token = nil },
		func(c *Config) { c.Client = nil }, func(c *Config) { c.Client = &http.Client{} },
	} {
		c := testConfig(func(*http.Request) (*http.Response, error) { t.Fatal("network on config"); return nil, nil })
		change(&c)
		if _, err := New(c); !errors.Is(err, ErrConfig) {
			t.Fatalf("config accepted: %v", err)
		}
	}
	for _, timeout := range []time.Duration{0, time.Minute, time.Second} {
		c := testConfig(func(*http.Request) (*http.Response, error) { return reply(200, ""), nil })
		c.Client.Timeout = timeout
		s, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		want := timeout
		if want <= 0 || want > 10*time.Second {
			want = 10 * time.Second
		}
		if s.client.Timeout != want {
			t.Fatal(s.client.Timeout)
		}
		if c.Client.CheckRedirect != nil || c.Client.Timeout != timeout {
			t.Fatal("mutated caller client")
		}
		if !errors.Is(s.client.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
			t.Fatal("redirect accepted")
		}
	}
	for _, key := range []string{"", "/root", "a//b", "a/../b", "a/./b", "a\\b", "a\x00b", "bad key", strings.Repeat("x", 1025), string([]byte{255})} {
		if validKey(key) {
			t.Fatalf("accepted key %q", key)
		}
	}
	if !validKey("user/upload") || !validBucket("bucket-123") {
		t.Fatal("valid input rejected")
	}
}

func TestUnitPutIntegrityAndRejections(t *testing.T) {
	data := "private statement"
	s := testStore(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != "PUT" || r.URL.String() != "https://storage.example/private-statements/user/upload" {
			t.Fatalf("request %s %s", r.Method, r.URL)
		}
		if r.Header.Get("X-YaCloud-SubjectToken") != "test-token" || r.Header.Get("Authorization") != "" {
			t.Fatal("wrong auth")
		}
		if r.Header.Get("X-Amz-Server-Side-Encryption") != "aws:kms" || r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != "kms-test" {
			t.Fatal("encryption missing")
		}
		sum := md5.Sum([]byte(data))
		if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(sum[:]) || r.ContentLength != int64(len(data)) {
			t.Fatal("integrity missing")
		}
		body, e := io.ReadAll(r.Body)
		if e != nil || string(body) != data {
			t.Fatal("wrong body")
		}
		return reply(200, ""), nil
	})
	if e := s.Put(t.Context(), "user/upload", strings.NewReader(data), int64(len(data))); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		key  string
		body io.Reader
		size int64
	}{
		{"../key", strings.NewReader(data), 1}, {"user/upload", nil, 0}, {"user/upload", strings.NewReader(data), -1},
		{"user/upload", strings.NewReader(data), maxObjectBytes + 1}, {"user/upload", strings.NewReader(data), 1},
		{"user/upload", strings.NewReader(data), 100}, {"user/upload", errReader{}, 1},
	} {
		if e := s.Put(t.Context(), tc.key, tc.body, tc.size); !errors.Is(e, ErrInput) {
			t.Fatalf("put rejection %v", e)
		}
	}
	for _, code := range []int{301, 403, 500} {
		s := testStore(t, func(*http.Request) (*http.Response, error) { return reply(code, "private provider credential"), nil })
		if e := s.Put(t.Context(), "u/f", strings.NewReader("x"), 1); !errors.Is(e, ErrUnavailable) {
			t.Fatalf("unsafe failure: %v", e)
		}
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("private I/O diagnostics") }

func TestUnitOpenDeleteAndBodyLimit(t *testing.T) {
	for _, code := range []int{200, 404, 500} {
		s := testStore(t, func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodGet && strings.HasPrefix(r.Header.Get("X-Amz-Server-Side-Encryption"), "aws") {
				t.Fatal("SSE request headers on read/delete")
			}
			return reply(code, "file"), nil
		})
		body, err := s.Open(t.Context(), "u/f")
		if code == 200 {
			if err != nil {
				t.Fatal(err)
			}
			data, e := io.ReadAll(body)
			if e != nil || string(data) != "file" {
				t.Fatal(e)
			}
			if e = body.Close(); e != nil {
				t.Fatal(e)
			}
		} else {
			want := ErrUnavailable
			if code == 404 {
				want = ErrNotFound
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
		}
		want := error(nil)
		if code != 200 {
			want = ErrUnavailable
		}
		if e := s.Delete(t.Context(), "u/f"); !errors.Is(e, want) {
			t.Fatal(e)
		}
	}
	s := testStore(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: maxObjectBytes + 1, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if _, e := s.Open(t.Context(), "u/f"); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	if _, e := s.Open(t.Context(), "../f"); !errors.Is(e, ErrInput) {
		t.Fatal(e)
	}
	if e := s.Delete(t.Context(), "../f"); !errors.Is(e, ErrInput) {
		t.Fatal(e)
	}
	for _, code := range []int{204, 301, 403} {
		s := testStore(t, func(*http.Request) (*http.Response, error) { return reply(code, ""), nil })
		e := s.Delete(t.Context(), "u/f")
		if !errors.Is(e, ErrUnavailable) {
			t.Fatal(e)
		}
	}
	for _, size := range []int{0, 1, 4, 5} {
		b := &limitedBody{ctx: t.Context(), expires: time.Now().Add(time.Second), body: io.NopCloser(strings.NewReader(strings.Repeat("x", size))), left: 4}
		if n, e := b.Read(nil); n != 0 || e != nil {
			t.Fatal("zero read")
		}
		got, err := io.ReadAll(b)
		if size > 4 {
			if len(got) != 4 || !errors.Is(err, ErrUnavailable) {
				t.Fatalf("overflow %d %v", len(got), err)
			}
		} else if err != nil || len(got) != size {
			t.Fatal(err)
		}
		if e := b.Close(); e != nil {
			t.Fatal(e)
		}
	}
}

const listXML = `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>true</IsTruncated><NextContinuationToken>next+opaque=</NextContinuationToken><Contents><Size>16</Size><Key>u/f</Key><LastModified>2026-10-01T10:00:00.000Z</LastModified></Contents></ListBucketResult>`

func TestUnitListPaginationAndUnsafeReplies(t *testing.T) {
	s := testStore(t, func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if q.Get("prefix") != "u/" || q.Get("list-type") != "2" || q.Get("max-keys") != "1000" || q.Get("continuation-token") != "prev+opaque=" {
			t.Fatal(q)
		}
		return reply(200, listXML), nil
	})
	p, err := s.List(t.Context(), "u/", "prev+opaque=")
	if err != nil || len(p.Objects) != 1 || p.Objects[0].Key != "u/f" || p.Objects[0].ModifiedAt.IsZero() || p.Next != "next+opaque=" {
		t.Fatalf("%+v %v", p, err)
	}
	for _, prefix := range []string{"u", "../", "/u/", "u//"} {
		if _, e := s.List(t.Context(), prefix, ""); !errors.Is(e, ErrInput) {
			t.Fatal(e)
		}
	}
	if _, e := s.List(t.Context(), "u/", strings.Repeat("x", 2049)); !errors.Is(e, ErrInput) {
		t.Fatal(e)
	}
	for _, xml := range []string{
		"broken", listXML + "<extra/>", strings.ReplaceAll(listXML, "u/f", "other/f"), strings.ReplaceAll(listXML, "u/f", "u/../f"),
		strings.ReplaceAll(listXML, "2026-10-01T10:00:00.000Z", "not-a-date"), strings.ReplaceAll(listXML, "2026-10-01T10:00:00.000Z", "0001-01-01T00:00:00Z"),
		strings.ReplaceAll(listXML, "next+opaque=", ""), strings.ReplaceAll(listXML, "<IsTruncated>true", "<IsTruncated>false"),
		strings.ReplaceAll(listXML, "next+opaque=", strings.Repeat("x", 2049)),
		"<ListBucketResult>" + strings.Repeat("<Contents><Size>16</Size><Key>u/f</Key><LastModified>2026-10-01T00:00:00Z</LastModified></Contents>", 1001) + "</ListBucketResult>",
		strings.Repeat("x", int(maxListBytes)+1),
	} {
		s := testStore(t, func(*http.Request) (*http.Response, error) { return reply(200, xml), nil })
		if _, e := s.List(t.Context(), "u/", ""); !errors.Is(e, ErrUnavailable) {
			t.Fatalf("unsafe list accepted: %v", e)
		}
	}
	s = testStore(t, func(*http.Request) (*http.Response, error) { return reply(200, listXML), nil })
	if _, e := s.List(t.Context(), "u/", "next+opaque="); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	s = testStore(t, func(*http.Request) (*http.Response, error) {
		return reply(200, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>"), nil
	})
	p, err = s.List(t.Context(), "", "")
	if err != nil || len(p.Objects) != 0 {
		t.Fatal(err)
	}
	s = testStore(t, func(*http.Request) (*http.Response, error) { return reply(403, "secret"), nil })
	if _, e := s.List(t.Context(), "u/", ""); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	s = testStore(t, func(*http.Request) (*http.Response, error) {
		r := reply(200, "")
		r.Body = io.NopCloser(errReader{})
		return r, nil
	})
	if _, e := s.List(t.Context(), "u/", ""); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
}

func TestUnitFailuresPreserveCancellationAndHideDiagnostics(t *testing.T) {
	for _, token := range []TokenProvider{func(context.Context) (string, error) { return "", errors.New("secret path") }, func(context.Context) (string, error) { return "\r\nInjected: yes", nil }} {
		c := testConfig(func(*http.Request) (*http.Response, error) { t.Fatal("sent invalid credential"); return nil, nil })
		c.Token = token
		s, e := New(c)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Delete(t.Context(), "u/f"); !errors.Is(e, ErrUnavailable) {
			t.Fatal(e)
		}
		if _, e = s.Open(t.Context(), "u/f"); !errors.Is(e, ErrUnavailable) {
			t.Fatal(e)
		}
		if e = s.Put(t.Context(), "u/f", strings.NewReader("x"), 1); !errors.Is(e, ErrUnavailable) {
			t.Fatal(e)
		}
		if _, e = s.List(t.Context(), "u/", ""); !errors.Is(e, ErrUnavailable) {
			t.Fatal(e)
		}
	}
	s := testStore(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("https://credential/private-key") })
	if e := s.Delete(t.Context(), "u/f"); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	if _, e := s.Open(t.Context(), "u/f"); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	if e := s.Put(t.Context(), "u/f", strings.NewReader("x"), 1); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	if _, e := s.List(t.Context(), "u/", ""); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if e := s.Delete(ctx, "u/f"); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	c := testConfig(func(*http.Request) (*http.Response, error) { t.Fatal("network after token cancel"); return nil, nil })
	c.Token = func(ctx context.Context) (string, error) { return "", ctx.Err() }
	s, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Delete(ctx, "u/f"); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestUnitFileTokenReloadAndBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iam")
	provider := FileToken(path)
	if _, e := provider(t.Context()); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	for _, value := range []string{"token-v1\n", "token-v2\n", "", "\r\nheader injected", strings.Repeat("x", 16385)} {
		if e := os.WriteFile(path, []byte(value), 0600); e != nil {
			t.Fatal(e)
		}
		got, e := provider(t.Context())
		if strings.HasPrefix(value, "token-") {
			if e != nil || got != strings.TrimSpace(value) {
				t.Fatal(e)
			}
		} else if !errors.Is(e, ErrUnavailable) {
			t.Fatalf("accepted invalid token: %v", e)
		}
	}
	if _, e := FileToken(filepath.Dir(path))(t.Context()); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := provider(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestIntegrationPrivateStorageTLSProtocol(t *testing.T) {
	data := []byte("<?OFX test private bytes>")
	objects := make(map[string][]byte)
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("X-YaCloud-SubjectToken") != "rotated-token" {
			t.Error("missing token")
			w.WriteHeader(403)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/private-statements/")
		switch r.Method {
		case "PUT":
			if r.Header.Get("X-Amz-Server-Side-Encryption") != "aws:kms" || r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != "kms-test" {
				t.Error("unencrypted request")
			}
			body, e := io.ReadAll(r.Body)
			if e != nil {
				t.Error(e)
			}
			objects[key] = body
			w.WriteHeader(200)
		case "GET":
			if r.URL.Query().Get("list-type") == "2" {
				_, _ = io.WriteString(w, `<ListBucketResult><Contents><Size>16</Size><Key>u/f</Key><LastModified>2026-10-01T00:00:00Z</LastModified></Contents></ListBucketResult>`)
				return
			}
			if b, ok := objects[key]; ok {
				_, _ = w.Write(b)
			} else {
				w.WriteHeader(404)
			}
		case "DELETE":
			delete(objects, key)
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	c := testConfig(nil)
	c.Endpoint = server.URL
	c.Client = server.Client()
	c.Token = func(context.Context) (string, error) { return "rotated-token", nil }
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if e := s.Put(t.Context(), "u/f", bytes.NewReader(data), int64(len(data))); e != nil {
		t.Fatal(e)
	}
	b, e := s.Open(t.Context(), "u/f")
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(b)
	if e != nil || !bytes.Equal(got, data) {
		t.Fatal(e)
	}
	if e = b.Close(); e != nil {
		t.Fatal(e)
	}
	if p, e := s.List(t.Context(), "u/", ""); e != nil || len(p.Objects) != 1 {
		t.Fatalf("%+v %v", p, e)
	}
	if e = s.Delete(t.Context(), "u/f"); e != nil {
		t.Fatal(e)
	}
	if e = s.Delete(t.Context(), "u/f"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Open(t.Context(), "u/f"); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	if requests != 6 {
		t.Fatal(requests)
	}
}

func TestIntegrationRedirectDoesNotExposeToken(t *testing.T) {
	calls := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c := testConfig(nil)
	c.Endpoint = source.URL
	c.Client = source.Client()
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if e := s.Delete(t.Context(), "u/f"); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	if calls != 0 {
		t.Fatal("token disclosed to redirect target")
	}
}

func TestUnitEmptyConfigurationRefused(t *testing.T) {
	_, err := New(Config{})
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("empty config: %v", err)
	}
}
