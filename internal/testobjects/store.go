// Package testobjects provides a real TLS HTTP boundary for service tests. It is
// never selected by production configuration; the production S3 adapter is unchanged.
package testobjects

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/config"
)

// Store serves a dedicated private fixture bucket, including conditional file writes
// and permanent zero-byte erasure fences.
type Store struct {
	Server         *httptest.Server
	mu             sync.Mutex
	data           map[string][]byte
	modified       map[string]time.Time
	TokenFile      string
	erasureFailure atomic.Bool
	readEntered    chan<- struct{}
	readResume     <-chan struct{}
}

// PauseReads delays the response after its snapshot was captured, modelling a
// provider GET already in flight while a file is concurrently cancelled/erased.
func (s *Store) PauseReads(entered chan<- struct{}, resume <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readEntered, s.readResume = entered, resume
}

// SetErasureFailure models a provider outage without changing the runtime adapter.
func (s *Store) SetErasureFailure(fail bool) { s.erasureFailure.Store(fail) }

// New creates a real TLS S3 protocol fixture and an external token file.
func New(t testing.TB) *Store {
	t.Helper()
	s := &Store{data: map[string][]byte{}, modified: map[string]time.Time{}, TokenFile: filepath.Join(t.TempDir(), "iam")}
	if err := os.WriteFile(s.TokenFile, []byte("fixture-iam-token"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Server.Close)
	return s
}

// Configure selects this fixture endpoint without weakening HTTPS verification.
func (s *Store) Configure(c *config.Config) {
	c.ObjectStorage = config.ObjectStorageConfig{Endpoint: s.Server.URL, Bucket: "private", KMSKeyID: "fixture-key", IAMTokenFile: s.TokenFile}
}

// Bytes returns a copy of an object; a present empty slice is an erasure fence.
func (s *Store) Bytes(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return append([]byte{}, v...), ok
}

func (s *Store) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-YaCloud-SubjectToken") != "fixture-iam-token" {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/private" || r.URL.Path == "/private/" {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		q := r.URL.Query()
		switch {
		case q.Has("versioning"):
			_, _ = io.WriteString(w, "<VersioningConfiguration/>")
		case q.Has("lifecycle"), q.Has("object-lock"):
			w.WriteHeader(404)
		case q.Get("list-type") == "2":
			type item struct {
				Key          string
				LastModified string
				Size         int
			}
			type page struct {
				XMLName               xml.Name `xml:"ListBucketResult"`
				Contents              []item
				IsTruncated           bool
				NextContinuationToken string `xml:",omitempty"`
			}
			s.mu.Lock()
			keys := make([]string, 0, len(s.data))
			for k := range s.data {
				if strings.HasPrefix(k, q.Get("prefix")) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			p := page{}
			for _, k := range keys {
				p.Contents = append(p.Contents, item{k, s.modified[k].UTC().Format(time.RFC3339Nano), len(s.data[k])})
			}
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			_ = xml.NewEncoder(w).Encode(p)
		default:
			w.WriteHeader(400)
		}
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/private/")
	if !ok || key == "" {
		w.WriteHeader(404)
		return
	}
	switch r.Method {
	case http.MethodPut:
		if r.Header.Get("X-Amz-Server-Side-Encryption") != "aws:kms" || r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != "fixture-key" {
			w.WriteHeader(400)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, (10<<20)+1))
		if err != nil || len(body) > 10<<20 || r.ContentLength != int64(len(body)) {
			w.WriteHeader(400)
			return
		}
		if len(body) == 0 && s.erasureFailure.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, exists := s.data[key]; exists && r.Header.Get("If-None-Match") == "*" {
			w.WriteHeader(412)
			return
		}
		s.data[key] = append([]byte{}, body...)
		s.modified[key] = time.Now()
		w.WriteHeader(200)
	case http.MethodGet:
		s.mu.Lock()
		body, ok := s.data[key]
		body = append([]byte{}, body...)
		entered, resume := s.readEntered, s.readResume
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		if resume != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-resume:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	default:
		w.WriteHeader(405)
	}
}
