// Package objectstore implements the private Yandex Object Storage boundary.
package objectstore

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- S3 Content-MD5 is transfer integrity, not authentication.
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

const maxObjectBytes int64 = 10 << 20
const maxListBytes int64 = 2 << 20

// Errors deliberately exclude provider diagnostics, keys and credentials.
var (
	ErrConfig      = errors.New("invalid private object storage configuration")
	ErrInput       = errors.New("invalid private object storage input")
	ErrUnavailable = errors.New("private object storage unavailable")
	ErrNotFound    = errors.New("private statement object not found")
)

// TokenProvider reloads the current short-lived IAM token for each request.
type TokenProvider func(context.Context) (string, error)

// Config contains explicit infrastructure supplied by application assembly.
type Config struct {
	Endpoint string
	Bucket   string
	KMSKeyID string
	Token    TokenProvider
	Client   *http.Client
}

// Store uses path-style S3 requests authenticated with a Yandex IAM token.
type Store struct {
	endpoint url.URL
	bucket   string
	kmsKeyID string
	token    TokenProvider
	client   http.Client
}

var _ importing.ObjectStore = (*Store)(nil)

// New requires TLS, encryption and an explicitly supplied HTTP transport.
// Redirects are disabled so a provider redirect cannot disclose the IAM token.
func New(c Config) (*Store, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		!validBucket(c.Bucket) || !headerValue(c.KMSKeyID) || c.Token == nil || c.Client == nil || c.Client.Transport == nil {
		return nil, ErrConfig
	}
	client := *c.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// The service reservation lasts 15s; leave time for its final database commit.
	if client.Timeout <= 0 || client.Timeout > 10*time.Second {
		client.Timeout = 10 * time.Second
	}
	return &Store{endpoint: *u, bucket: c.Bucket, kmsKeyID: c.KMSKeyID, token: c.Token, client: client}, nil
}

// Put sends a single bounded object with an exact length and transfer checksum.
// An uncertain PUT is never automatically retried; the usecase owns reconciliation.
func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	if size == 0 {
		return ErrInput
	}
	return s.put(ctx, key, body, size, true)
}

func (s *Store) put(ctx context.Context, key string, body io.Reader, size int64, create bool) error {
	if !validKey(key) || body == nil || size < 0 || size > maxObjectBytes {
		return ErrInput
	}
	data, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil || int64(len(data)) != size {
		return ErrInput
	}
	req, err := s.request(ctx, http.MethodPut, key, nil, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = size
	if create {
		req.Header.Set("If-None-Match", "*")
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	sum := md5.Sum(data) // #nosec G401 -- mandated S3 Content-MD5 transfer checksum.
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	req.Header.Set("X-Amz-Server-Side-Encryption", "aws:kms")
	req.Header.Set("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", s.kmsKeyID)
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	return nil
}

// Open returns a bounded body which the caller must close and verify against its hash.
// Caller cancellation remains authoritative through EOF; already read bytes are retained.
func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if !validKey(key) {
		return nil, ErrInput
	}
	req, err := s.request(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxObjectBytes || resp.ContentLength == 0 {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusOK && resp.ContentLength == 0 {
			return nil, ErrNotFound
		}
		return nil, ErrUnavailable
	}
	return &limitedBody{ctx: ctx, body: resp.Body, left: maxObjectBytes}, nil
}

// Delete erases file contents with a permanent encrypted zero-byte fence. A late
// conditional file PUT is rejected even if its client already timed out. Removing
// this technical marker would make the unknown write eligible again.
func (s *Store) Delete(ctx context.Context, key string) error {
	return s.put(ctx, key, strings.NewReader(""), 0, false)
}

type listResult struct {
	XMLName   xml.Name `xml:"ListBucketResult"`
	Truncated bool     `xml:"IsTruncated"`
	Next      string   `xml:"NextContinuationToken"`
	Objects   []struct {
		Key      string    `xml:"Key"`
		Modified time.Time `xml:"LastModified"`
		Size     *int64    `xml:"Size"`
	} `xml:"Contents"`
}

// List returns one bounded page. The usecase owns pagination and orphan grace.
func (s *Store) List(ctx context.Context, prefix, cursor string) (importing.ObjectPage, error) {
	if (prefix != "" && (!strings.HasSuffix(prefix, "/") || !validKey(strings.TrimSuffix(prefix, "/")))) || len(cursor) > 2048 {
		return importing.ObjectPage{}, ErrInput
	}
	q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}, "prefix": {prefix}}
	if cursor != "" {
		q.Set("continuation-token", cursor)
	}
	req, err := s.request(ctx, http.MethodGet, "", q, nil)
	if err != nil {
		return importing.ObjectPage{}, err
	}
	resp, err := s.do(req)
	if err != nil {
		return importing.ObjectPage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return importing.ObjectPage{}, ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxListBytes+1))
	if err != nil || int64(len(data)) > maxListBytes {
		return importing.ObjectPage{}, ErrUnavailable
	}
	var result listResult
	decoder := xml.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&result) != nil || len(result.Objects) > 1000 || len(result.Next) > 2048 ||
		(result.Truncated && (result.Next == "" || result.Next == cursor)) || (!result.Truncated && result.Next != "") {
		return importing.ObjectPage{}, ErrUnavailable
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return importing.ObjectPage{}, ErrUnavailable
	}
	page := importing.ObjectPage{Next: result.Next}
	for _, object := range result.Objects {
		if !validKey(object.Key) || !strings.HasPrefix(object.Key, prefix) || object.Modified.IsZero() || object.Size == nil || *object.Size < 0 {
			return importing.ObjectPage{}, ErrUnavailable
		}
		if *object.Size == 0 {
			continue
		}
		page.Objects = append(page.Objects, importing.Object{Key: object.Key, ModifiedAt: object.Modified})
	}
	return page, nil
}

func (s *Store) request(ctx context.Context, method, key string, query url.Values, body io.Reader) (*http.Request, error) {
	token, err := s.token(ctx)
	if err != nil || !headerValue(token) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrUnavailable
	}
	u := s.endpoint
	u.Path = "/" + s.bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawPath = ""
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, ErrInput
	}
	req.Header.Set("X-YaCloud-SubjectToken", token)
	return req, nil
}

func (s *Store) do(req *http.Request) (*http.Response, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, ErrUnavailable
	}
	return resp, nil
}

func validBucket(s string) bool {
	if len(s) < 3 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' || strings.Contains(s, "..") {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func headerValue(s string) bool {
	if s == "" || len(s) > 16384 {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func validKey(s string) bool {
	if s == "" || len(s) > 1024 || !utf8.ValidString(s) {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	for _, c := range s {
		if c < 33 || c > 126 || c == '\\' {
			return false
		}
	}
	return true
}

type limitedBody struct {
	ctx  context.Context
	body io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if b.left == 0 {
		var extra [1]byte
		n, err := b.body.Read(extra[:])
		if callerErr := b.ctx.Err(); callerErr != nil {
			return 0, callerErr
		}
		if n > 0 {
			return 0, ErrUnavailable
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.body.Read(p)
	b.left -= int64(n)
	// Transport EOF can race with cancellation; never report successful EOF
	// after the caller budget expired, while retaining already returned bytes.
	if callerErr := b.ctx.Err(); callerErr != nil {
		return n, callerErr
	}
	return n, err
}
func (b *limitedBody) Close() error { return b.body.Close() }
