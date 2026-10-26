package objectstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestUnitCommonS3BodiesRejectLateValidXML(t *testing.T) {
	for _, operation := range []string{"list", "bucket-last-response"} {
		for _, budget := range []string{"caller", "client"} {
			t.Run(operation+"/"+budget, func(t *testing.T) {
				caller, cancel := context.WithCancel(t.Context())
				defer cancel()
				var last *budgetBody
				calls := 0
				cfg := testConfig(func(r *http.Request) (*http.Response, error) {
					calls++
					if operation == "bucket-last-response" {
						if r.URL.Query().Has("versioning") {
							return reply(http.StatusOK, "<VersioningConfiguration/>"), nil
						}
						if r.URL.Query().Has("lifecycle") {
							return reply(http.StatusNotFound, ""), nil
						}
					}
					payload := listXML
					if operation == "bucket-last-response" {
						payload = "<ObjectLockConfiguration/>"
					}
					last = &budgetBody{read: func(p []byte) (int, error) {
						if budget == "caller" {
							cancel()
						} else {
							<-r.Context().Done()
						}
						return copy(p, payload), io.EOF
					}}
					return &http.Response{StatusCode: http.StatusOK, ContentLength: -1, Body: last, Header: make(http.Header)}, nil
				})
				cfg.Client.Timeout = 200 * time.Millisecond
				s, err := New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if operation == "list" {
					page, err := s.List(caller, "u/", "")
					if !errors.Is(err, ErrUnavailable) || len(page.Objects) != 0 || page.Next != "" {
						t.Fatal("late XML became successful list", page, err)
					}
				} else {
					if err := s.CheckBucket(caller); !errors.Is(err, ErrUnavailable) || calls != 3 {
						t.Fatal("late final XML became safe bucket", calls, err)
					}
				}
				if last == nil || !last.closed {
					t.Fatal("response body ownership lost")
				}
				if budget == "client" && caller.Err() != nil {
					t.Fatal("adapter canceled caller")
				}
			})
		}
	}
}

func TestUnitObjectLimitProbeKeepsTransferErrorAndDiscardsExtraByte(t *testing.T) {
	for _, outcome := range []error{context.Canceled, context.DeadlineExceeded, io.EOF} {
		t.Run(outcome.Error(), func(t *testing.T) {
			raw := &budgetBody{read: func(p []byte) (int, error) {
				p[0] = 'x'
				return 1, outcome
			}}
			body := &limitedBody{body: raw}
			p := []byte{'s'}
			n, err := body.Read(p)
			want := outcome
			if errors.Is(outcome, io.EOF) {
				want = ErrUnavailable
			}
			if n != 0 || !errors.Is(err, want) || p[0] != 's' || raw.reads != 1 {
				t.Fatal("probe changed transfer error or exposed extra byte", n, err, p, raw.reads)
			}
		})
	}
}
