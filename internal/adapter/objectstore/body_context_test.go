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

type budgetBody struct {
	read   func([]byte) (int, error)
	reads  int
	closed bool
	close  func() error
}

func (b *budgetBody) Read(p []byte) (int, error) { b.reads++; return b.read(p) }
func (b *budgetBody) Close() error {
	b.closed = true
	if b.close != nil {
		return b.close()
	}
	return nil
}

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

func TestUnitOpenBodyKeepsExistingClientBudgetThroughEOF(t *testing.T) {
	for _, callerAlsoCanceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "client-budget", true: "caller-priority"}[callerAlsoCanceled], func(t *testing.T) {
			caller, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := testConfig(func(r *http.Request) (*http.Response, error) {
				raw := &budgetBody{read: func(p []byte) (int, error) {
					<-r.Context().Done()
					if callerAlsoCanceled {
						cancel()
					}
					return copy(p, "fixture"), io.EOF
				}}
				return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: raw, Header: make(http.Header)}, nil
			})
			cfg.Client.Timeout = 200 * time.Millisecond
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			body, err := s.Open(caller, "user/file")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = body.Close() }()
			n, err := io.Copy(io.Discard, body)
			want := context.DeadlineExceeded
			if callerAlsoCanceled {
				want = context.Canceled
			}
			if n != 7 || !errors.Is(err, want) {
				t.Fatal("existing client cap converted EOF to success", n, err)
			}
			if !callerAlsoCanceled && caller.Err() != nil {
				t.Fatal("adapter canceled the caller")
			}
		})
	}
}

func TestUnitOpenBodyCompletedTransferKeepsEOFAfterCapAndCloseOwnership(t *testing.T) {
	failure := errors.New("close failure")
	for _, closeErr := range []error{nil, failure} {
		t.Run(map[bool]string{true: "close-error", false: "success"}[closeErr != nil], func(t *testing.T) {
			caller, cancel := context.WithCancel(t.Context())
			defer cancel()
			raw := &budgetBody{read: strings.NewReader("fixture").Read, close: func() error {
				if caller.Err() != nil {
					t.Error("adapter canceled caller")
				}
				return closeErr
			}}
			cfg := testConfig(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: raw, Header: make(http.Header)}, nil
			})
			cfg.Client.Timeout = 200 * time.Millisecond
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			body, err := s.Open(caller, "user/file")
			if err != nil {
				t.Fatal(err)
			}
			if data, err := io.ReadAll(body); err != nil || string(data) != "fixture" {
				t.Fatal(data, err)
			}
			// Model CPU parsing after the completed transfer, beyond its former cap.
			timer := time.NewTimer(2 * cfg.Client.Timeout)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-caller.Done():
				t.Fatal("caller canceled during parsing")
			}
			if n, err := body.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatal("successful EOF acquired a new timeout", n, err)
			}
			if err := body.Close(); !errors.Is(err, closeErr) || !raw.closed || caller.Err() != nil {
				t.Fatal("Close ownership/error changed", err)
			}
			cancel()
			if n, err := body.Read(make([]byte, 1)); n != 0 || !errors.Is(err, context.Canceled) {
				t.Fatal("completed transfer hid later caller cancellation", n, err)
			}
		})
	}
}

func TestUnitOpenRejectsHeadersAfterClientCapAndClosesBody(t *testing.T) {
	caller := t.Context()
	raw := &budgetBody{read: strings.NewReader("fixture").Read}
	cfg := testConfig(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: raw, Header: make(http.Header)}, nil
	})
	cfg.Client.Timeout = 200 * time.Millisecond
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	body, err := s.Open(caller, "user/file")
	if body != nil || !errors.Is(err, context.DeadlineExceeded) || !raw.closed || caller.Err() != nil {
		t.Fatal("late headers lost cap/body/caller ownership", body, err, raw.closed)
	}
}
