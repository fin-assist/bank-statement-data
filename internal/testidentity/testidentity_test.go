package testidentity

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	identity "github.com/fin-assist/go-identity"

	"github.com/fin-assist/bank-statement-data/internal/config"
)

type staticKeys string

func (k staticKeys) JWKS(context.Context) ([]byte, error) { return []byte(k), nil }

// The fixture is only useful if its tokens are judged by go-identity exactly as tokens of
// the stand: genuine ones pass, forged and expired ones do not.
func TestUnitStandTokensAreJudgedByGoIdentity(t *testing.T) {
	s := New(t)
	services := s.keysServer().services
	for _, caller := range Services {
		if got, err := services.VerifyService(t.Context(), s.ServiceToken(caller)); err != nil || got != caller {
			t.Fatalf("%s: %q %v", caller, got, err)
		}
	}
	if _, err := services.VerifyService(t.Context(), s.ForgedServiceToken("bank-bff")); err == nil {
		t.Fatal("forged service token accepted")
	}
	cache, err := identity.NewKeyCache(staticKeys(s.userJWKS), time.Now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	users, err := identity.NewUserVerifier(cache, identity.VerifyOptions{Issuer: UserIssuer, Audience: UserAudience, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	if sub, err := users.VerifyUser(t.Context(), s.UserToken("u-1")); err != nil || sub != "u-1" {
		t.Fatalf("user token: %q %v", sub, err)
	}
	for name, token := range map[string]string{"forged": s.ForgedUserToken("u-1"), "expired": s.ExpiredUserToken("u-1")} {
		if _, err := users.VerifyUser(t.Context(), token); err == nil {
			t.Fatalf("%s user token accepted", name)
		}
	}
}

func TestUnitStandFilesAndConfiguration(t *testing.T) {
	s := New(t)
	cfg := config.Config{}
	s.Configure(&cfg, "bank-statement-data")
	stage := cfg.Auth.Identity.Stage
	if cfg.Env != config.EnvStage || cfg.Auth.Mode != config.AuthVerify || stage.Issuer != Issuer || cfg.Peers["user"] == "" ||
		cfg.Auth.Identity.UserIssuer != UserIssuer || cfg.Auth.Identity.UserAudience != UserAudience {
		t.Fatalf("configuration %+v", cfg)
	}
	key, err := os.ReadFile(stage.SigningKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(key)
	if block == nil {
		t.Fatal("signing key is not PEM")
	}
	if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(stage.SigningKeyFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("signing key mode: %v", err)
	}
	if _, err := os.Stat(stage.PublicKeysFile); err != nil {
		t.Fatal(err)
	}
	env := s.Environment("bank-statement-data")
	if env["ENV"] != "stage" || env["AUTH_MODE"] != "verify" || env["AUTH_STAGE_SIGNING_KEY_FILE"] != stage.SigningKeyFile ||
		env["AUTH_STAGE_PUBLIC_KEYS_FILE"] != stage.PublicKeysFile || env["PEER_USER_ADDR"] != cfg.Peers["user"] {
		t.Fatalf("environment %v", env)
	}
	md, _ := metadata.FromOutgoingContext(Outgoing(t.Context(), "svc", "usr"))
	if md.Get("x-service-authorization")[0] != "Bearer svc" || md.Get("authorization")[0] != "Bearer usr" {
		t.Fatalf("metadata %v", md)
	}
	md, _ = metadata.FromOutgoingContext(Outgoing(t.Context(), "svc", ""))
	if len(md.Get("authorization")) != 0 {
		t.Fatalf("system call carries a user token: %v", md)
	}
}

// The fake user refuses a call without a valid stand token, like the real one, and records
// verified callers only.
func TestUnitSigningKeysVerifiesCaller(t *testing.T) {
	s := New(t)
	keys := s.keysServer()
	in := func(values ...string) context.Context {
		md := metadata.MD{}
		for _, v := range values {
			md.Append("x-service-authorization", v)
		}
		return metadata.NewIncomingContext(t.Context(), md)
	}
	for name, ctx := range map[string]context.Context{
		"none":    t.Context(),
		"forged":  in("Bearer " + s.ForgedServiceToken("bank-statement-data")),
		"scheme":  in(s.ServiceToken("bank-statement-data")),
		"two":     in("Bearer "+s.ServiceToken("bank-statement-data"), "Bearer "+s.ServiceToken("bank-statement-data")),
		"garbage": in("Bearer x"),
	} {
		if _, err := keys.GetSigningKeys(ctx, &userv1.GetSigningKeysRequest{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if s.SigningKeysCalls() != 0 {
		t.Fatal("refused calls were recorded")
	}
	resp, err := keys.GetSigningKeys(in("Bearer "+s.ServiceToken("bank-statement-data")), &userv1.GetSigningKeysRequest{})
	if err != nil || resp.GetJwksJson() != s.userJWKS {
		t.Fatalf("keys: %v", err)
	}
	if got := s.SigningKeysCallers(); len(got) != 1 || got[0] != "bank-statement-data" {
		t.Fatalf("callers %v", got)
	}
}
