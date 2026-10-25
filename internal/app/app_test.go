package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/testidentity"
)

func registryEmpty(t *testing.T, reg *prometheus.Registry) bool {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	return len(families) == 0
}

func gathered(t *testing.T, reg *prometheus.Registry, name string) bool {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return true
		}
	}
	return false
}

// Verify mode refuses to build identity from incomplete or unsafe settings: no weaker
// fallback, nothing registered, no connection to user.
func TestUnitIdentityFailsClosed(t *testing.T) {
	stand := testidentity.New(t)
	valid := func() config.Config {
		cfg := config.Config{Service: "bank-statement-data"}
		stand.Configure(&cfg, "bank-statement-data")
		return cfg
	}
	tests := []struct {
		name   string
		change func(*config.Config)
		want   string
	}{
		{"verify in dev", func(c *config.Config) { c.Env = config.EnvDev }, "env prod or stage"},
		{"no ENV", func(c *config.Config) { c.Env = "" }, "env prod or stage"},
		{"no user peer", func(c *config.Config) { delete(c.Peers, "user") }, "PEER_USER_ADDR"},
		{"no user issuer", func(c *config.Config) { c.Auth.Identity.UserIssuer = "" }, "user issuer"},
		{"missing stage key file", func(c *config.Config) { c.Auth.Identity.Stage.SigningKeyFile = t.TempDir() + "/absent.pem" }, "identity"},
		{"stage keys in prod", func(c *config.Config) { c.Env = config.EnvProd; c.Auth.ServiceTokenFile = "/token" }, "not allowed in prod"},
		{"prod without service token", func(c *config.Config) {
			c.Env = config.EnvProd
			c.Auth.Identity.Stage = config.StageIdentity{}
		}, "separate service and API token files"},
		{"prod without CA", func(c *config.Config) {
			c.Env = config.EnvProd
			c.Auth.Identity.Stage = config.StageIdentity{}
			c.Auth.ServiceTokenFile = "/token"
			c.Auth.Identity.Kubernetes.CAFile = t.TempDir() + "/absent.crt"
		}, "CA file"},
		{"unknown mode", func(c *config.Config) { c.Auth.Mode = "trust-me" }, "unsupported AUTH_MODE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.change(&cfg)
			reg := prometheus.NewRegistry()
			id, err := newIdentity(cfg, reg, nil, time.Now)
			if err == nil || !strings.Contains(err.Error(), tt.want) || id.services != nil || id.users != nil || id.tokens != nil {
				t.Fatalf("identity built: %v", err)
			}
			if !registryEmpty(t, reg) {
				t.Fatal("a refused identity left metrics registered")
			}
			// The whole assembly refuses with the same reason before opening any pool.
			opened := false
			a, err := assemble(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) {
				opened = true
				return nil, errors.New("must not open")
			})
			if a != nil || opened || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("assembly: app=%v opened=%v err=%v", a != nil, opened, err)
			}
		})
	}
	if stand.SigningKeysCalls() != 0 {
		t.Fatal("a refused identity called user")
	}
}

// A valid stand configuration builds the go-identity assembly without network: user keys
// are fetched lazily or by the identity-keys job, which the assembly returns.
func TestUnitStageIdentityBuildsAssemblyAndRefreshJob(t *testing.T) {
	stand := testidentity.New(t)
	cfg := config.Config{Service: "bank-statement-data"}
	stand.Configure(&cfg, "bank-statement-data")
	reg := prometheus.NewRegistry()
	id, err := newIdentity(cfg, reg, nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer id.close()
	if id.services == nil || id.users == nil || id.tokens == nil || len(id.jobs) != 1 || id.jobs[0].Name != "identity-keys" || id.jobs[0].Run == nil {
		t.Fatalf("incomplete identity: %+v", id)
	}
	if _, err := id.services.VerifyService(t.Context(), "not-a-jwt"); err == nil {
		t.Fatal("garbage service token accepted")
	}
	if !gathered(t, reg, "finassist_identity_rejected_total") {
		t.Fatal("identity metrics are not in the service registry")
	}
	token, err := id.tokens.Token(t.Context())
	if err != nil || strings.Count(token, ".") != 2 {
		t.Fatalf("outgoing stand token: %v", err)
	}
	if caller, err := id.services.VerifyService(t.Context(), stand.ServiceToken("bank-bff")); err != nil || caller != "bank-bff" {
		t.Fatalf("stand caller: %q %v", caller, err)
	}
	if _, err := id.services.VerifyService(t.Context(), stand.ForgedServiceToken("bank-bff")); err == nil {
		t.Fatal("forged stand token accepted")
	}
	if stand.SigningKeysCalls() != 0 {
		t.Fatal("assembly contacted user before a user token needed keys")
	}
}

func TestUnitInsecureDevIdentityOnlyInDev(t *testing.T) {
	id, err := newIdentity(config.Config{Service: "bank-statement-data", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev}}, prometheus.NewRegistry(), nil, time.Now)
	if err != nil || id.services == nil || id.users == nil || id.tokens == nil || len(id.jobs) != 0 {
		t.Fatalf("dev: %v", err)
	}
	id.close()
}

// The dev-only gate holds for callers of New that bypass config.Load (apitest, a
// hand-built Config): unverified identities are refused before any pool is opened.
func TestUnitInsecureDevRefusedOutsideDevWithoutConfigLoad(t *testing.T) {
	for _, env := range []config.Env{config.EnvStage, config.EnvProd, ""} {
		t.Run(string(env), func(t *testing.T) {
			cfg := config.Config{Service: "analytics", Env: env, Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
				DB: config.DBConfig{PrimaryDSN: "primary", MaxConns: 1}}
			id, err := newIdentity(cfg, prometheus.NewRegistry(), nil, time.Now)
			if err == nil || id.services != nil || id.users != nil || id.tokens != nil || !strings.Contains(err.Error(), "only with ENV=dev") {
				t.Fatalf("insecure identities accepted in %q: %v", env, err)
			}
			opened := false
			a, err := assemble(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) {
				opened = true
				return nil, errors.New("must not open")
			})
			if a != nil || err == nil || opened {
				t.Fatalf("assembly in %q: app=%v opened=%v err=%v", env, a != nil, opened, err)
			}
		})
	}
}

type fakeDeleted struct {
	deleted bool
	err     error
	asked   []string
}

func (f *fakeDeleted) Deleted(_ context.Context, id string) (bool, error) {
	f.asked = append(f.asked, id)
	return f.deleted, f.err
}

// Admission asks the store for every call, counts only refusals and passes failures on
// (go-grpc turns them into a bare UNAVAILABLE).
func TestUnitTombstoneAdmissionCountsRefusalsOnly(t *testing.T) {
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "revoked", Help: "test"})
	value := func() float64 {
		var m dto.Metric
		if err := counter.Write(&m); err != nil {
			t.Fatal(err)
		}
		return m.GetCounter().GetValue()
	}
	boom := errors.New("storage")
	for _, tt := range []struct {
		name    string
		store   *fakeDeleted
		deleted bool
		err     error
		count   float64
	}{
		{"alive", &fakeDeleted{}, false, nil, 0},
		{"deleted", &fakeDeleted{deleted: true}, true, nil, 1},
		{"failure", &fakeDeleted{deleted: true, err: boom}, true, boom, 1},
		{"deleted again", &fakeDeleted{deleted: true}, true, nil, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tombstoneAdmission{store: tt.store, revoked: counter}.IsDeleted(t.Context(), "u-1")
			if got != tt.deleted || !errors.Is(err, tt.err) || len(tt.store.asked) != 1 || tt.store.asked[0] != "u-1" || value() != tt.count {
				t.Fatalf("got %v %v, asked %v, counted %v", got, err, tt.store.asked, value())
			}
		})
	}
}

func TestIntegrationNewFailsOnUnreachableDatabase(t *testing.T) {
	cfg := config.Config{Service: "user", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
		DB: config.DBConfig{PrimaryDSN: "postgres://nobody@127.0.0.1:1/db?connect_timeout=1", MaxConns: 1}}
	if _, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry()); err == nil {
		t.Fatal("want an error")
	}
}
