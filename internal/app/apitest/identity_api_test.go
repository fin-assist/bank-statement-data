package apitest

import (
	"context"
	"errors"
	"strings"
	"testing"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/jackc/pgx/v5"

	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
	"github.com/fin-assist/bank-statement-data/internal/testidentity"
)

// AUTH_MODE=verify on the stand: the whole assembly starts with go-identity (stand key
// files, the user peer) and go-tombstone after real migrations. Both import and
// operations RPCs use verified identity, caller policy and primary deletion admission.
func TestAPIStageVerifyAssemblyServes(t *testing.T) {
	stand := testidentity.New(t).ServeUser()
	env := Start(t, func(cfg *config.Config) {
		cfg.Service = "bank-statement-data"
		stand.Configure(cfg, "bank-statement-data")
	})
	if env.App.Cfg.Env != config.EnvStage || env.App.Cfg.Auth.Mode != config.AuthVerify {
		t.Fatalf("assembly configuration %s/%s", env.App.Cfg.Env, env.App.Cfg.Auth.Mode)
	}
	jobs := map[string]bool{}
	for _, j := range env.App.Jobs {
		jobs[j.Name] = true
	}
	if !jobs["identity-keys"] || len(env.App.Jobs) != 4 {
		t.Fatalf("jobs %v: the user key refresh must run with the service", jobs)
	}
	token, err := env.App.Tokens.Token(t.Context())
	if err != nil || strings.HasPrefix(token, "dev:") {
		t.Fatalf("outgoing token is not a stand JWT: %v", err)
	}
	resp, err := healthpb.NewHealthClient(env.Conn).Check(t.Context(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health: %v %v", resp, err)
	}
	ctx := testidentity.Outgoing(t.Context(), stand.ServiceToken("bank-bff"), stand.UserToken("3f0e9a52-7f7b-4c9b-9b8e-0d1f5d0c2a11"))
	operations := pb.NewOperationsServiceClient(env.Conn)
	if feed, err := operations.ListFeed(ctx, &pb.ListFeedRequest{}); err != nil || feed.TimeZone != "Europe/Moscow" || len(feed.Items) != 0 {
		t.Fatalf("verified real operations: %v %v", feed, err)
	}
	client := pb.NewStatementsServiceClient(env.Conn)
	if c, err := client.GetImportConfig(ctx, &pb.GetImportConfigRequest{}); err != nil || c.TimeZone != "Europe/Moscow" {
		t.Fatalf("verified real import: %v %v", c, err)
	}
	for _, tc := range []struct{ name, service, user, reason string }{
		{"foreign caller", stand.ServiceToken("assistant"), stand.UserToken("3f0e9a52-7f7b-4c9b-9b8e-0d1f5d0c2a11"), "PERMISSION_DENIED"},
		{"forged user", stand.ServiceToken("bank-bff"), stand.ForgedUserToken("3f0e9a52-7f7b-4c9b-9b8e-0d1f5d0c2a11"), "INVALID_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.GetImportConfig(testidentity.Outgoing(t.Context(), tc.service, tc.user), &pb.GetImportConfigRequest{})
			_, operationErr := operations.ListFeed(testidentity.Outgoing(t.Context(), tc.service, tc.user), &pb.ListFeedRequest{})
			if tc.reason == "PERMISSION_DENIED" {
				if status.Code(operationErr) != codes.PermissionDenied {
					t.Fatalf("operations foreign caller: %v", operationErr)
				}
			} else if Reason(operationErr) != tc.reason {
				t.Fatalf("operations bad identity: %v", operationErr)
			}
			if tc.reason == "PERMISSION_DENIED" {
				if status.Code(err) != codes.PermissionDenied {
					t.Fatalf("foreign caller: %v", err)
				}
			} else if Reason(err) != tc.reason {
				t.Fatalf("bad identity: %v", err)
			}
		})
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	if _, err = db.Exec(t.Context(), `INSERT INTO tombstone_users(user_id,deleted_at)VALUES($1,now())`, "3f0e9a52-7f7b-4c9b-9b8e-0d1f5d0c2a11"); err != nil {
		t.Fatal(err)
	}
	if _, err = operations.ListFeed(ctx, &pb.ListFeedRequest{}); Reason(err) != "TOKEN_REVOKED" {
		t.Fatalf("deleted verified operations: %v", err)
	}
	if _, err = client.GetImportConfig(ctx, &pb.GetImportConfigRequest{}); Reason(err) != "TOKEN_REVOKED" {
		t.Fatalf("deleted verified import: %v", err)
	}
}

// Verify mode never falls back to unverified identities: without its settings, outside
// prod/stage or with stand keys in prod the whole assembly refuses with the identity
// reason (that no pool is opened first is asserted by TestUnitIdentityFailsClosed).
func TestAPIVerifyAssemblyRefusesIncompleteIdentity(t *testing.T) {
	stand := testidentity.New(t)
	dsn := testdb.DSN(t)
	for _, tt := range []struct {
		name, want string
		mutate     func(*config.Config)
	}{
		{"prod without files", "separate service and API token files", func(c *config.Config) {
			c.Env, c.Auth.Mode = config.EnvProd, config.AuthVerify
			c.Auth.Identity = config.IdentityConfig{UserIssuer: "u", UserAudience: "a"}
			c.Peers["user"] = "dns:///user.invalid:9090"
		}},
		{"verify in dev", "env prod or stage", func(c *config.Config) {
			stand.Configure(c, "bank-statement-data")
			c.Env = config.EnvDev
		}},
		{"stand keys in prod", "not allowed in prod", func(c *config.Config) {
			stand.Configure(c, "bank-statement-data")
			c.Env, c.Auth.ServiceTokenFile = config.EnvProd, "/var/run/secrets/finassist/token"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env, closeEnv, err := start(t, context.Background(), dsn, realDependencies(), tt.mutate)
			if err == nil {
				closeEnv()
				t.Fatalf("started: %v", env != nil)
			}
			if !strings.HasPrefix(err.Error(), "app: identity: ") || !strings.Contains(err.Error(), tt.want) || errors.Is(err, context.Canceled) {
				t.Fatalf("refusal %q, want the identity reason %q", err, tt.want)
			}
		})
	}
	if stand.SigningKeysCalls() != 0 {
		t.Fatal("a refused assembly called user")
	}
}
