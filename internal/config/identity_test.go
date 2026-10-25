package config

import (
	"strings"
	"testing"
)

func prodIdentityConfig() Config {
	return Config{Env: EnvProd, Auth: AuthConfig{Mode: AuthVerify, ServiceTokenFile: "/internal-token", Identity: IdentityConfig{
		UserIssuer: "https://user.example", UserAudience: "finassist",
		Kubernetes: KubernetesIdentity{Issuer: "https://cluster.example", JWKSURL: "https://cluster.example/openid/v1/jwks", CAFile: "/ca.pem", APITokenFile: "/api-token", Namespace: "finassist"},
	}}, Peers: map[string]string{"user": "dns:///user:9090"}}
}

func stageIdentityConfig() Config {
	c := prodIdentityConfig()
	c.Env = EnvStage
	c.Auth.ServiceTokenFile = ""
	c.Auth.Identity.Stage = StageIdentity{Issuer: "stage", SigningKeyFile: "/stage/key.pem", PublicKeysFile: "/stage/keys.json"}
	return c
}

func TestUnitValidateIdentityProd(t *testing.T) {
	if errs := prodIdentityConfig().validateIdentity(); len(errs) != 0 {
		t.Fatal(errs)
	}
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"AUTH_USER_ISSUER", func(c *Config) { c.Auth.Identity.UserIssuer = "" }},
		{"AUTH_USER_AUDIENCE", func(c *Config) { c.Auth.Identity.UserAudience = " " }},
		{"PEER_USER_ADDR", func(c *Config) { delete(c.Peers, "user") }},
		{"SERVICE_TOKEN_FILE", func(c *Config) { c.Auth.ServiceTokenFile = "" }},
		{"AUTH_KUBERNETES_ISSUER", func(c *Config) { c.Auth.Identity.Kubernetes.Issuer = "" }},
		{"AUTH_KUBERNETES_JWKS_URL", func(c *Config) { c.Auth.Identity.Kubernetes.JWKSURL = "" }},
		{"AUTH_KUBERNETES_CA_FILE", func(c *Config) { c.Auth.Identity.Kubernetes.CAFile = "" }},
		{"AUTH_KUBERNETES_API_TOKEN_FILE", func(c *Config) { c.Auth.Identity.Kubernetes.APITokenFile = "" }},
		{"AUTH_KUBERNETES_NAMESPACE", func(c *Config) { c.Auth.Identity.Kubernetes.Namespace = "" }},
		{"AUTH_STAGE_", func(c *Config) { c.Auth.Identity.Stage.SigningKeyFile = "/stage/key.pem" }},
		{"separate audiences", func(c *Config) { c.Auth.Identity.Kubernetes.APITokenFile = "/x/../internal-token" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := prodIdentityConfig()
			tt.change(&c)
			errs := c.validateIdentity()
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tt.name) {
				t.Fatalf("got %v", errs)
			}
		})
	}
}

func TestUnitValidateIdentityStage(t *testing.T) {
	if errs := stageIdentityConfig().validateIdentity(); len(errs) != 0 {
		t.Fatal(errs)
	}
	for name, change := range map[string]func(*Config){
		"AUTH_STAGE_ISSUER":           func(c *Config) { c.Auth.Identity.Stage.Issuer = "" },
		"AUTH_STAGE_SIGNING_KEY_FILE": func(c *Config) { c.Auth.Identity.Stage.SigningKeyFile = "" },
		"AUTH_STAGE_PUBLIC_KEYS_FILE": func(c *Config) { c.Auth.Identity.Stage.PublicKeysFile = "" },
		"PEER_USER_ADDR":              func(c *Config) { c.Peers = map[string]string{} },
	} {
		t.Run(name, func(t *testing.T) {
			c := stageIdentityConfig()
			change(&c)
			errs := c.validateIdentity()
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), name) {
				t.Fatalf("got %v", errs)
			}
		})
	}
	// Kubernetes settings are not used in stage: their absence is not an error.
	c := stageIdentityConfig()
	c.Auth.Identity.Kubernetes = KubernetesIdentity{}
	if errs := c.validateIdentity(); len(errs) != 0 {
		t.Fatal(errs)
	}
}

func TestUnitValidateIdentityRefusesVerifyOutsideStageAndProd(t *testing.T) {
	for _, env := range []Env{EnvDev, "", "qa"} {
		c := prodIdentityConfig()
		c.Env = env
		errs := c.validateIdentity()
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "ENV=prod or ENV=stage") {
			t.Fatalf("%q: %v", env, errs)
		}
	}
	c := Config{Env: EnvDev, Auth: AuthConfig{Mode: AuthInsecureDev}}
	if errs := c.validateIdentity(); len(errs) != 0 {
		t.Fatal(errs)
	}
}

func TestUnitValidateIdentityJWKSURL(t *testing.T) {
	for _, address := range []string{"http://cluster/jwks", "https://name:secret@cluster/jwks", "https://cluster/jwks?token=secret", "https://cluster/jwks#key", "https:///jwks", ":invalid"} {
		t.Run(address, func(t *testing.T) {
			c := prodIdentityConfig()
			c.Auth.Identity.Kubernetes.JWKSURL = address
			errs := c.validateIdentity()
			if len(errs) != 1 || strings.Contains(errs[0].Error(), "secret") {
				t.Fatalf("unsafe URL accepted or leaked: %v", errs)
			}
		})
	}
}

func TestUnitLoadReadsIdentityAndDefaults(t *testing.T) {
	get, list := env(base())
	c, err := load(get, noFiles, list)
	if err != nil {
		t.Fatal(err)
	}
	k := c.Auth.Identity.Kubernetes
	if k.Issuer != "https://kubernetes.default.svc.cluster.local" || k.JWKSURL != "https://kubernetes.default.svc/openid/v1/jwks" ||
		k.CAFile != "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt" || k.APITokenFile != "/var/run/secrets/kubernetes.io/serviceaccount/token" ||
		k.Namespace != "finassist" || !c.Auth.Identity.Stage.empty() || c.Auth.Identity.UserIssuer != "https://user.example" || c.Auth.Identity.UserAudience != "finassist" {
		t.Fatalf("identity %+v", c.Auth.Identity)
	}
	vars := base()
	delete(vars, "SERVICE_TOKEN_FILE")
	vars["ENV"] = "stage"
	vars["AUTH_STAGE_ISSUER"], vars["AUTH_STAGE_SIGNING_KEY_FILE"], vars["AUTH_STAGE_PUBLIC_KEYS_FILE"] = "stage", "/k.pem", "/keys.json"
	get, list = env(vars)
	if c, err = load(get, noFiles, list); err != nil || c.Auth.Identity.Stage != (StageIdentity{Issuer: "stage", SigningKeyFile: "/k.pem", PublicKeysFile: "/keys.json"}) {
		t.Fatalf("stage %+v %v", c.Auth.Identity.Stage, err)
	}
	// The defaults alone (ENV=prod, AUTH_MODE=verify) never start without identity settings.
	get, list = env(map[string]string{"SERVICE_NAME": "s", "DB_PRIMARY_DSN": "postgres://p/s"})
	if _, err := load(get, noFiles, list); err == nil || !strings.Contains(err.Error(), "AUTH_USER_ISSUER") || !strings.Contains(err.Error(), "SERVICE_TOKEN_FILE") {
		t.Fatalf("bare defaults accepted: %v", err)
	}
}
