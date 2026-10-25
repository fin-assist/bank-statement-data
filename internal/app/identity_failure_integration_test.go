package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/go-grpc/client"
	"github.com/fin-assist/go-grpc/server"

	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/testidentity"
)

func writeTestFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// foreignCA is a self-signed CA that did not issue the JWKS server certificate.
func foreignCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "foreign"}, IsCA: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// The prod branch of the service assembly against a TLS Kubernetes JWKS endpoint: the
// service passes the CA, the separate API token and the namespace from its configuration,
// a ServiceAccount token of the namespace passes, other namespaces and audiences do not,
// a server outside the configured CA is never trusted, and the outgoing token is the
// projected finassist-internal file.
func TestIntegrationProdKubernetesIdentity(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const apiToken = "api-server-audience-token"
	var fetches, unauthorized atomic.Int32
	jwks, err := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": "cluster", "x": base64.RawURLEncoding.EncodeToString(pub)}}})
	if err != nil {
		t.Fatal(err)
	}
	cluster := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+apiToken || r.URL.Path != "/openid/v1/jwks" {
			unauthorized.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fetches.Add(1)
		_, _ = w.Write(jwks)
	}))
	defer cluster.Close()
	caFile := writeTestFile(t, "ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cluster.Certificate().Raw}))
	prodConfig := func(ca string) config.Config {
		return config.Config{Service: "bank-statement-data", Env: config.EnvProd,
			Auth: config.AuthConfig{Mode: config.AuthVerify, ServiceTokenFile: writeTestFile(t, "internal-token", []byte("projected-internal-token\n")),
				Identity: config.IdentityConfig{UserIssuer: testidentity.UserIssuer, UserAudience: testidentity.UserAudience,
					Kubernetes: config.KubernetesIdentity{Issuer: "https://cluster.test", JWKSURL: cluster.URL + "/openid/v1/jwks",
						CAFile: ca, APITokenFile: writeTestFile(t, "api-token", []byte(apiToken+"\n")), Namespace: "finassist"}}},
			Peers: map[string]string{"user": "passthrough:///user.invalid:9090"}}
	}
	saToken := func(namespace, name, audience string) string {
		now := time.Now()
		return testidentity.SignJWT(t, key, "cluster", map[string]any{
			"iss": "https://cluster.test", "aud": audience, "sub": "system:serviceaccount:" + namespace + ":" + name,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			"kubernetes.io": map[string]any{"namespace": namespace, "serviceaccount": map[string]any{"name": name}},
		})
	}

	id, err := newIdentity(prodConfig(caFile), prometheus.NewRegistry(), nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer id.close()
	if caller, err := id.services.VerifyService(t.Context(), saToken("finassist", "bank-bff", "finassist-internal")); err != nil || caller != "bank-bff" {
		t.Fatalf("ServiceAccount of the namespace: %q %v", caller, err)
	}
	if fetches.Load() == 0 || unauthorized.Load() != 0 {
		t.Fatalf("JWKS fetched %d times, %d without the API token", fetches.Load(), unauthorized.Load())
	}
	for name, token := range map[string]string{
		"other namespace": saToken("other", "bank-bff", "finassist-internal"),
		"API audience":    saToken("finassist", "bank-bff", "https://kubernetes.default.svc"),
	} {
		if caller, err := id.services.VerifyService(t.Context(), token); err == nil {
			t.Fatalf("%s accepted as %q", name, caller)
		}
	}
	if token, err := id.tokens.Token(t.Context()); err != nil || token != "projected-internal-token" {
		t.Fatalf("outgoing token %q %v", token, err)
	}

	before := fetches.Load()
	untrusted, err := newIdentity(prodConfig(writeTestFile(t, "foreign.crt", foreignCA(t))), prometheus.NewRegistry(), nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.close()
	_, err = untrusted.services.VerifyService(t.Context(), saToken("finassist", "bank-bff", "finassist-internal"))
	if status.Code(err) != codes.Unavailable || fetches.Load() != before {
		t.Fatalf("JWKS server outside the configured CA: %v (fetches %d -> %d)", err, before, fetches.Load())
	}
}

// When user.GetSigningKeys cannot be reached and no key snapshot exists yet, a user call
// is refused before the handler with a bare UNAVAILABLE (safe to retry), never accepted
// and never INVALID_TOKEN that would log the user out; the failed fetch is visible in the
// client metrics of the service registry.
func TestIntegrationUserKeysUnavailableRefusesBeforeHandler(t *testing.T) {
	stand := testidentity.New(t)
	cfg := config.Config{Service: "bank-statement-data"}
	stand.Configure(&cfg, "bank-statement-data")
	cfg.Peers["user"] = "passthrough:///127.0.0.1:1" // nothing listens: connection refused
	reg := prometheus.NewRegistry()
	clients, err := client.NewMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newIdentity(cfg, reg, clients, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(id.close)
	call, seen := admissionChain(t, id, server.NoTombstones{}, reg)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- call(stand.ServiceToken("bank-bff"), stand.UserToken("3f0e9a52-7f7b-4c9b-9b8e-0d1f5d0c2a11"))
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		t.Fatal("refusal did not arrive within the deadline")
	}
	st := status.Convert(err)
	if st.Code() != codes.Unavailable || server.Reason(err) != "" || len(st.Details()) != 0 {
		t.Fatalf("got %v (reason %q, details %d), want a bare UNAVAILABLE", st.Code(), server.Reason(err), len(st.Details()))
	}
	for _, leak := range []string{"127.0.0.1", "dial", "connection", "GetSigningKeys"} {
		if strings.Contains(st.Message(), leak) {
			t.Fatalf("refusal leaks %q: %q", leak, st.Message())
		}
	}
	select {
	case got := <-seen:
		t.Fatalf("handler ran without verified keys (user %q)", got)
	default:
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counted := false
	for _, f := range families {
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			counted = counted || (f.GetName() == "grpc_client_handled_total" && labels["peer"] == "user" &&
				labels["method"] == "/finassist.user.v1.UserInternalService/GetSigningKeys" && labels["code"] == "Unavailable")
		}
	}
	if !counted {
		t.Fatal("the failed GetSigningKeys is not in grpc_client_handled_total{peer=\"user\"}")
	}
}
