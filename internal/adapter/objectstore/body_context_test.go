package objectstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

type budgetBody struct {
	read   func([]byte) (int, error)
	reads  int
	closed bool
}

func (b *budgetBody) Read(p []byte) (int, error) { b.reads++; return b.read(p) }
func (b *budgetBody) Close() error               { b.closed = true; return nil }

func TestUnitOpenBodyPreservesCallerCancellationBeforeAndAfterRead(t *testing.T) {
	for _, phase := range []string{"before", "after-eof", "after-bytes", "limit-probe"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			raw := &budgetBody{read: func(p []byte) (int, error) {
				cancel()
				if phase == "after-bytes" {
					return copy(p, "fixture"), io.EOF
				}
				return 0, io.EOF
			}}
			s := testStore(t, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: raw, Header: make(http.Header)}, nil
			})
			opened, err := s.Open(ctx, "user/file")
			if err != nil {
				t.Fatal(err)
			}
			body := opened.(*limitedBody)
			if phase == "before" {
				cancel()
			}
			if phase == "limit-probe" {
				body.left = 0
			}
			if n, err := body.Read(nil); n != 0 || err != nil || raw.reads != 0 {
				t.Fatal("zero-length read touched transport or context", n, err, raw.reads)
			}
			data := make([]byte, 16)
			n, err := body.Read(data)
			want := 0
			if phase == "after-bytes" {
				want = 7
			}
			if n != want || !errors.Is(err, context.Canceled) {
				t.Fatal("caller budget lost", n, err)
			}
			if phase == "before" && raw.reads != 0 {
				t.Fatal("canceled caller still read transport")
			}
			if n > 0 && string(data[:n]) != "fixture" {
				t.Fatal("partial bytes changed")
			}
			if err := body.Close(); err != nil || !raw.closed {
				t.Fatal("transport ownership lost", err)
			}
		})
	}
}

func TestUnitOpenBodyCopyKeepsPartialBytesAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	raw := &budgetBody{read: func(p []byte) (int, error) { cancel(); return copy(p, "fixture"), io.EOF }}
	s := testStore(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: raw, Header: make(http.Header)}, nil
	})
	body, err := s.Open(ctx, "user/file")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	n, err := io.Copy(io.Discard, body)
	if n != 7 || !errors.Is(err, context.Canceled) {
		t.Fatal("copy converted canceled EOF to success", n, err)
	}
}
