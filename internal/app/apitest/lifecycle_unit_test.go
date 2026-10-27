package apitest

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

type requestContextKey struct{}

type memoryListener struct {
	closed chan struct{}
	once   sync.Once
}

func (l *memoryListener) Accept() (net.Conn, error) { return nil, errors.New("not a transport") }
func (l *memoryListener) Addr() net.Addr            { return nil }
func (l *memoryListener) DialContext(context.Context) (net.Conn, error) {
	return nil, errors.New("not a transport")
}
func (l *memoryListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }

type memoryService struct {
	checkErr      error
	exited        chan struct{}
	events        *[]string
	shutdownValid bool
}

func (s *memoryService) Check() error { *s.events = append(*s.events, "check"); return s.checkErr }
func (s *memoryService) Serve(l net.Listener) error {
	<-l.(*memoryListener).closed
	close(s.exited)
	return nil
}
func (s *memoryService) SetServing(v bool) {
	if v {
		*s.events = append(*s.events, "serving")
	}
}
func (s *memoryService) Shutdown(ctx context.Context, delay time.Duration) {
	deadline, ok := ctx.Deadline()
	s.shutdownValid = ok && time.Until(deadline) > 0 && time.Until(deadline) <= 5*time.Second && ctx.Err() == nil && delay == 0
	*s.events = append(*s.events, "shutdown")
}

func TestUnitStartStagesOwnResources(t *testing.T) {
	failure := errors.New("stage failed")
	for _, stage := range []string{"migrate", "app", "server", "client", "success"} {
		t.Run(stage, func(t *testing.T) {
			events := []string{}
			lis := &memoryListener{closed: make(chan struct{})}
			srv := &memoryService{exited: make(chan struct{}), events: &events}
			if stage == "server" {
				srv.checkErr = failure
			}
			assembled := &app.App{}
			closedAfterServe := false
			key := requestContextKey{}
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), key, "request"))
			defer cancel()
			deps := dependencies{
				migrate: func(got context.Context, dsn string) error {
					events = append(events, "migrate")
					if got.Value(key) != "request" || dsn != "test-owned" {
						t.Fatal("lost context/DSN")
					}
					if stage == "migrate" {
						return failure
					}
					return nil
				},
				assemble: func(got context.Context, cfg config.Config) (*app.App, service, func(), error) {
					events = append(events, "app")
					if got != ctx || cfg.Service != "custom" || cfg.DB.PrimaryDSN != "test-owned" || cfg.DB.MaxConns != 7 || cfg.Peers["peer"] != "second" {
						t.Fatalf("assembly config: %+v", cfg)
					}
					if stage == "app" {
						return nil, nil, nil, failure
					}
					return assembled, srv, func() {
						events = append(events, "close-app")
						select {
						case <-srv.exited:
							closedAfterServe = true
						default:
						}
					}, nil
				},
				listen: func() listener { events = append(events, "listen"); return lis },
				connect: func(got listener) (*grpc.ClientConn, func() error, error) {
					events = append(events, "client")
					if got != lis {
						t.Fatal("wrong listener")
					}
					if stage == "client" {
						cancel()
						return nil, nil, failure
					}
					return nil, func() error { events = append(events, "close-client"); return failure }, nil
				},
			}
			env, cleanup, err := start(t, ctx, "test-owned", deps, func(cfg *config.Config) {
				if cfg.Service != "apitest" || cfg.Env != config.EnvDev || cfg.Auth.Mode != config.AuthInsecureDev || cfg.DB.MaxConns != 4 || cfg.GRPC.MaxRecvMsgSize != 4<<20 || len(cfg.Peers) != 0 {
					t.Fatalf("defaults: %+v", cfg)
				}
				cfg.Service = "custom"
				cfg.DB.MaxConns = 7
				cfg.Peers["peer"] = "first"
			}, func(cfg *config.Config) { cfg.Peers["peer"] = "second" })
			expected := map[string][]string{
				"migrate": {"migrate"}, "app": {"migrate", "app"}, "server": {"migrate", "app", "check", "close-app"},
				"client":  {"migrate", "app", "check", "listen", "serving", "client", "shutdown", "close-app"},
				"success": {"migrate", "app", "check", "listen", "serving", "client", "close-client", "shutdown", "close-app"},
			}[stage]
			if stage == "success" {
				if err != nil || env.App != assembled || env.DSN != "test-owned" || cleanup == nil {
					t.Fatalf("result: %v %v", env, err)
				}
				cancel()
				cleanup()
			} else if !errors.Is(err, failure) || !strings.HasPrefix(err.Error(), stage+":") || env != nil || cleanup != nil {
				t.Fatalf("stage result: %v %v", env, err)
			}
			if !reflect.DeepEqual(events, expected) {
				t.Fatalf("resource order: %v want %v", events, expected)
			}
			if stage == "client" || stage == "success" {
				if !srv.shutdownValid || !closedAfterServe {
					t.Fatal("shutdown must have independent bounded context and join Serve before closing app")
				}
			}
		})
	}
}
