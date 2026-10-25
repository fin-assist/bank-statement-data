package app

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/fin-assist/go-grpc/reqctx"
	"github.com/fin-assist/go-grpc/server"
	"github.com/fin-assist/go-migrate"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
	"github.com/fin-assist/bank-statement-data/internal/testidentity"
)

// probe is a user-model method used only to drive the admission chain; the service has
// no user RPC yet. It records the verified user it saw.
type probe struct {
	testgrpc.UnimplementedTestServiceServer
	seen chan string
}

func (p probe) EmptyCall(ctx context.Context, _ *testgrpc.Empty) (*testgrpc.Empty, error) {
	id, _ := reqctx.UserID(ctx)
	p.seen <- id
	return &testgrpc.Empty{}, nil
}

// The identity and tombstone admission of the assembly, on their real dependencies: stand
// key files, user.GetSigningKeys over TCP, tombstone_users on PostgreSQL after every
// migration source, in the go-grpc server chain.
func TestIntegrationVerifiedIdentityAndTombstoneAdmission(t *testing.T) {
	dsn := testdb.DSN(t)
	mc := migrate.DefaultConfig(dsn)
	mc.Logger = slog.New(slog.DiscardHandler)
	if err := migrate.Up(t.Context(), mc, MigrationSources()); err != nil {
		t.Fatal(err)
	}
	db, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: dsn, MaxConns: 2, AppName: "identity-integration"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stand := testidentity.New(t).ServeUser()
	cfg := config.Config{Service: "bank-statement-data"}
	stand.Configure(&cfg, "bank-statement-data")
	reg := prometheus.NewRegistry()
	id, err := newIdentity(cfg, reg, nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(id.close) // after the chain below has stopped
	admission, err := newTombstoneAdmission(db, reg)
	if err != nil {
		t.Fatal(err)
	}
	call, seen := admissionChain(t, id, admission, reg)
	handler := probe{seen: seen}

	alive, deleted := "3f0e9a52-7f7b-4c9b-9b8e-0d1f5d0c2a11", "8c2b6c0e-1a3d-4f5e-9a7b-2c4d6e8f0a12"
	bff := stand.ServiceToken("bank-bff")
	if err := call(bff, stand.UserToken(alive)); err != nil {
		t.Fatal(err)
	}
	if got := <-handler.seen; got != alive {
		t.Fatalf("handler saw user %q", got)
	}
	if callers := stand.SigningKeysCallers(); len(callers) == 0 || callers[0] != "bank-statement-data" {
		t.Fatalf("user keys were not fetched with this service's stand token: %v", callers)
	}

	conn2, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn2.Close(context.Background()) }()
	if _, err := conn2.Exec(t.Context(), `INSERT INTO tombstone_users (user_id, deleted_at) VALUES ($1, now())`, deleted); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name          string
		service, user string
		code          codes.Code
		reason        string
	}{
		{"deleted user", bff, stand.UserToken(deleted), codes.Unauthenticated, "TOKEN_REVOKED"},
		{"deleted user, upper-case UUID", bff, stand.UserToken("8C2B6C0E-1A3D-4F5E-9A7B-2C4D6E8F0A12"), codes.Unauthenticated, "TOKEN_REVOKED"},
		{"forged user token", bff, stand.ForgedUserToken(alive), codes.Unauthenticated, "INVALID_TOKEN"},
		{"expired user token", bff, stand.ExpiredUserToken(alive), codes.Unauthenticated, "INVALID_TOKEN"},
		{"no user token", bff, "", codes.Unauthenticated, "INVALID_TOKEN"},
		{"subject is not a UUID", bff, stand.UserToken("not-a-uuid"), codes.Unavailable, ""},
		{"forged service token", stand.ForgedServiceToken("bank-bff"), stand.UserToken(alive), codes.Unauthenticated, ""},
		{"foreign caller", stand.ServiceToken("analytics"), stand.UserToken(alive), codes.PermissionDenied, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := call(tt.service, tt.user)
			if status.Code(err) != tt.code || server.Reason(err) != tt.reason {
				t.Fatalf("got %v (reason %q), want %v %q", status.Code(err), server.Reason(err), tt.code, tt.reason)
			}
			select {
			case got := <-handler.seen:
				t.Fatalf("handler ran for refused call (user %q)", got)
			default:
			}
		})
	}
	var revoked dto.Metric
	if err := admission.revoked.Write(&revoked); err != nil || revoked.GetCounter().GetValue() != 2 {
		t.Fatalf("finassist_tombstone_revoked_total = %v (%v), want the two deleted-user refusals", revoked.GetCounter().GetValue(), err)
	}
}

// admissionChain serves the probe in a go-grpc server built from the assembly's identity
// and tombstone admission and returns a caller of its user-model method.
func admissionChain(t *testing.T, id identityParts, admission server.Tombstones, reg *prometheus.Registry) (func(serviceToken, userToken string) error, chan string) {
	t.Helper()
	srv, err := server.New(server.Options{MaxRecvMsgSize: 1 << 20}, server.Deps{
		Logger: slog.New(slog.DiscardHandler), Metrics: reg,
		Policy:   server.Policy{"/grpc.testing.TestService/*": {Callers: []string{"bank-bff"}, Model: server.ModelUser}},
		Services: id.services, Users: id.users, Tombstones: admission,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := probe{seen: make(chan string, 1)}
	testgrpc.RegisterTestServiceServer(srv.GRPC(), handler)
	lis := bufconn.Listen(1 << 20)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(lis); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() { srv.Shutdown(context.Background(), 0); <-done })
	conn, err := grpc.NewClient("passthrough:///bank-statement-data",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	call := func(serviceToken, userToken string) error {
		_, err := testgrpc.NewTestServiceClient(conn).EmptyCall(testidentity.Outgoing(t.Context(), serviceToken, userToken), &testgrpc.Empty{})
		return err
	}
	return call, handler.seen
}
