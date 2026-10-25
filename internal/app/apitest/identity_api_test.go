package apitest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
	"github.com/fin-assist/bank-statement-data/internal/testidentity"
)

// AUTH_MODE=verify on the stand: the whole assembly starts with go-identity (stand key
// files, the user peer) and go-tombstone after real migrations, serves health and still
// refuses what it does not serve. The service has no RPC yet, so the token and tombstone
// refusals of a user call are covered by TestIntegrationVerifiedIdentityAndTombstoneAdmission
// on the same assembly parts; a user RPC brings them here.
func TestAPIStageVerifyAssemblyServes(t *testing.T) {
	stand := testidentity.New(t).ServeUser()
	env := Start(t, func(cfg *config.Config) { stand.Configure(cfg, "bank-statement-data") })
	if env.App.Cfg.Env != config.EnvStage || env.App.Cfg.Auth.Mode != config.AuthVerify {
		t.Fatalf("assembly configuration %s/%s", env.App.Cfg.Env, env.App.Cfg.Auth.Mode)
	}
	jobs := map[string]bool{}
	for _, j := range env.App.Jobs {
		jobs[j.Name] = true
	}
	if !jobs["identity-keys"] || len(env.App.Jobs) != 1 {
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
	if err := env.Conn.Invoke(ctx, "/finassist.statementdata.v1.OperationsService/GetOperation", &healthpb.HealthCheckRequest{}, &healthpb.HealthCheckResponse{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("unserved method: %v", err)
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
			env, closeEnv, err := start(context.Background(), dsn, realDependencies(), tt.mutate)
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
