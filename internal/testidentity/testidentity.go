// Package testidentity is a real stand identity for tests of AUTH_MODE=verify: Ed25519 key
// files of the stand services, a stand JWKS file and a user service that answers
// GetSigningKeys over a real TCP listener. Tokens it mints are genuine signed JWTs, so the
// go-identity assembly of the service verifies them exactly as on the stand. It is test
// infrastructure, never wired into the serve command.
package testidentity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	identity "github.com/fin-assist/go-identity"

	"github.com/fin-assist/bank-statement-data/internal/config"
)

// Stand settings shared by every token of a Stand.
const (
	Issuer       = "stage"
	UserIssuer   = "https://user.test"
	UserAudience = "finassist"
	userKeyID    = "user-key-1"
	audience     = "finassist-internal"
)

// Services are the stand services with keys in the public JWKS file.
var Services = []string{"bank-statement-data", "bank-bff", "user", "analytics", "assistant"}

// Stand is one test's stand: keys, files and the fake user service.
type Stand struct {
	t        testing.TB
	services map[string]ed25519.PrivateKey
	userKey  ed25519.PrivateKey
	keyFiles map[string]string
	public   string
	userJWKS string
	userAddr string
	mu       sync.Mutex
	callers  []string
}

// New writes the key files into a temporary directory, removed when the test ends. It
// opens no socket; ServeUser starts the user service.
func New(t testing.TB) *Stand {
	t.Helper()
	s := &Stand{t: t, services: map[string]ed25519.PrivateKey{}, keyFiles: map[string]string{}}
	dir := t.TempDir()
	type jwk struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		Kid string `json:"kid"`
		X   string `json:"x"`
		Use string `json:"use"`
	}
	var stand []jwk
	for _, name := range Services {
		pub, key := newKey(t)
		s.services[name] = key
		s.keyFiles[name] = writeKey(t, dir, name, key)
		stand = append(stand, jwk{Kty: "OKP", Crv: "Ed25519", Kid: name, X: base64.RawURLEncoding.EncodeToString(pub), Use: "sig"})
	}
	s.public = writeJSON(t, dir, "stand-keys.json", map[string]any{"keys": stand})
	userPub, userKey := newKey(t)
	s.userKey = userKey
	userJWKS, err := json.Marshal(map[string]any{"keys": []jwk{{Kty: "OKP", Crv: "Ed25519", Kid: userKeyID, X: base64.RawURLEncoding.EncodeToString(userPub), Use: "sig"}}})
	if err != nil {
		t.Fatal(err)
	}
	s.userJWKS = string(userJWKS)
	// Until ServeUser the peer is an address nobody dials: the connection is lazy.
	s.userAddr = "user.invalid:9090"
	return s
}

// ServeUser starts the user service on a real 127.0.0.1 listener (integration and API
// tests); Configure called afterwards points PEER_USER_ADDR at it.
func (s *Stand) ServeUser() *Stand {
	s.serveUser()
	return s
}

// Configure switches cfg to AUTH_MODE=verify on the stand as service: ENV=stage, its key
// file, the stand JWKS and the user peer of this Stand.
func (s *Stand) Configure(cfg *config.Config, service string) {
	cfg.Env = config.EnvStage
	cfg.Auth.Mode = config.AuthVerify
	cfg.Auth.Identity = config.IdentityConfig{
		UserIssuer: UserIssuer, UserAudience: UserAudience,
		Stage: config.StageIdentity{Issuer: Issuer, SigningKeyFile: s.keyFiles[service], PublicKeysFile: s.public},
	}
	if cfg.Peers == nil {
		cfg.Peers = map[string]string{}
	}
	cfg.Peers["user"] = "passthrough:///" + s.userAddr
}

// ServiceToken is a stand JWT of caller (kid = sub = caller, one minute).
func (s *Stand) ServiceToken(caller string) string {
	now := time.Now()
	return sign(s.t, s.services[caller], caller, map[string]any{
		"iss": Issuer, "sub": caller, "aud": audience, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
	})
}

// ForgedServiceToken claims caller but is signed by a key the stand does not know.
func (s *Stand) ForgedServiceToken(caller string) string {
	_, key := newKey(s.t)
	now := time.Now()
	return sign(s.t, key, caller, map[string]any{
		"iss": Issuer, "sub": caller, "aud": audience, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
	})
}

// UserToken is an access JWT of userID signed by the key user publishes.
func (s *Stand) UserToken(userID string) string {
	return s.userToken(s.userKey, userID, time.Now().Add(15*time.Minute))
}

// ForgedUserToken names the published key ID but is signed by another key.
func (s *Stand) ForgedUserToken(userID string) string {
	_, key := newKey(s.t)
	return s.userToken(key, userID, time.Now().Add(15*time.Minute))
}

// ExpiredUserToken is a genuine access JWT of userID that expired a minute ago.
func (s *Stand) ExpiredUserToken(userID string) string {
	return s.userToken(s.userKey, userID, time.Now().Add(-time.Minute))
}

// SigningKeysCalls counts the GetSigningKeys calls the user service answered.
func (s *Stand) SigningKeysCalls() int { return len(s.SigningKeysCallers()) }

// Outgoing returns ctx carrying the service token of caller and, when user is not empty,
// the user access token, as bank-bff sends them.
func Outgoing(ctx context.Context, serviceToken, userToken string) context.Context {
	kv := []string{"x-service-authorization", "Bearer " + serviceToken}
	if userToken != "" {
		kv = append(kv, "authorization", "Bearer "+userToken)
	}
	return metadata.AppendToOutgoingContext(ctx, kv...)
}

func (s *Stand) userToken(key ed25519.PrivateKey, sub string, exp time.Time) string {
	return sign(s.t, key, userKeyID, map[string]any{
		"iss": UserIssuer, "sub": sub, "aud": UserAudience, "iat": exp.Add(-15 * time.Minute).Unix(), "exp": exp.Unix(),
	})
}

// signingKeys is the user service: like the real one it verifies the stand service token
// of the caller, then serves the published user JWKS.
type signingKeys struct {
	userv1.UnimplementedUserInternalServiceServer
	jwks     string
	services *identity.StageVerifier
	stand    *Stand
}

func (k signingKeys) GetSigningKeys(ctx context.Context, _ *userv1.GetSigningKeysRequest) (*userv1.GetSigningKeysResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("x-service-authorization")
	token, ok := strings.CutPrefix(strings.Join(values, ""), "Bearer ")
	caller, err := k.services.VerifyService(ctx, token)
	if len(values) != 1 || !ok || err != nil {
		return nil, status.Error(codes.Unauthenticated, "") //nolint:forbidigo // a fake peer's bare refusal
	}
	k.stand.record(caller)
	return &userv1.GetSigningKeysResponse{JwksJson: k.jwks}, nil
}

func (s *Stand) record(caller string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callers = append(s.callers, caller)
}

// SigningKeysCallers lists the verified callers of every answered GetSigningKeys.
func (s *Stand) SigningKeysCallers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.callers...)
}

// Environment is the serve environment of service on this stand (ENV=stage,
// AUTH_MODE=verify and the AUTH_* variables), for process-level tests.
func (s *Stand) Environment(service string) map[string]string {
	cfg := config.Config{}
	s.Configure(&cfg, service)
	return map[string]string{
		"ENV": "stage", "AUTH_MODE": "verify", "SERVICE_NAME": service,
		"AUTH_USER_ISSUER": UserIssuer, "AUTH_USER_AUDIENCE": UserAudience,
		"AUTH_STAGE_ISSUER": Issuer, "AUTH_STAGE_SIGNING_KEY_FILE": s.keyFiles[service],
		"AUTH_STAGE_PUBLIC_KEYS_FILE": s.public, "PEER_USER_ADDR": cfg.Peers["user"],
	}
}

func (s *Stand) keysServer() signingKeys {
	public := map[string]ed25519.PublicKey{}
	for name, key := range s.services {
		public[name] = key.Public().(ed25519.PublicKey)
	}
	verifier, err := identity.NewStageVerifier("stage", identity.VerifyOptions{Issuer: Issuer, Audience: audience, Clock: time.Now}, public)
	if err != nil {
		s.t.Fatal(err)
	}
	return signingKeys{jwks: s.userJWKS, services: verifier, stand: s}
}

func (s *Stand) serveUser() {
	lis, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	srv := grpc.NewServer()
	userv1.RegisterUserInternalServiceServer(srv, s.keysServer())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(lis) }()
	s.userAddr = lis.Addr().String()
	s.t.Cleanup(func() {
		srv.Stop()
		if err := <-done; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			s.t.Error(err)
		}
	})
}

func newKey(t testing.TB) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, key
}

func writeKey(t testing.TB, dir, name string, key ed25519.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeJSON(t testing.TB, dir, name string, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sign builds an EdDSA JWT with the standard library: the payload is fixed by the test,
// the signature is real.
func sign(t testing.TB, key ed25519.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": kid})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
}
