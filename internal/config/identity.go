package config

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"
)

// IdentityConfig pins the trusted issuers, audiences and key sources of AUTH_MODE=verify.
// It is passed field by field to go-identity NewAssembly: ENV=prod uses the Kubernetes
// fields, ENV=stage the Stage fields; user tokens are checked in both by the keys of
// user.GetSigningKeys (PEER_USER_ADDR).
type IdentityConfig struct {
	UserIssuer   string
	UserAudience string
	Kubernetes   KubernetesIdentity
	Stage        StageIdentity
}

// KubernetesIdentity configures ServiceAccount tokens in prod. The API token belongs to the
// Kubernetes API audience; AuthConfig.ServiceTokenFile is the separate finassist-internal
// projected token.
type KubernetesIdentity struct {
	Issuer       string
	JWKSURL      string
	CAFile       string
	APITokenFile string
	Namespace    string
}

// StageIdentity configures the short stand JWTs signed with file keys.
type StageIdentity struct {
	Issuer         string
	SigningKeyFile string // this service's PKCS#8 PEM Ed25519 key
	PublicKeysFile string // JWKS of the stand services, kid = service name
}

func (s StageIdentity) empty() bool {
	return s.Issuer == "" && s.SigningKeyFile == "" && s.PublicKeysFile == ""
}

func (r *reader) identity() IdentityConfig {
	return IdentityConfig{
		UserIssuer:   r.str("AUTH_USER_ISSUER"),
		UserAudience: r.str("AUTH_USER_AUDIENCE"),
		Kubernetes: KubernetesIdentity{
			Issuer:       r.str("AUTH_KUBERNETES_ISSUER"),
			JWKSURL:      r.str("AUTH_KUBERNETES_JWKS_URL"),
			CAFile:       r.str("AUTH_KUBERNETES_CA_FILE"),
			APITokenFile: r.str("AUTH_KUBERNETES_API_TOKEN_FILE"),
			Namespace:    r.str("AUTH_KUBERNETES_NAMESPACE"),
		},
		Stage: StageIdentity{
			Issuer:         r.str("AUTH_STAGE_ISSUER"),
			SigningKeyFile: r.str("AUTH_STAGE_SIGNING_KEY_FILE"),
			PublicKeysFile: r.str("AUTH_STAGE_PUBLIC_KEYS_FILE"),
		},
	}
}

// validateIdentity reports every missing or unsafe setting of AUTH_MODE=verify at load, so
// a misconfigured pod fails before it opens pools; go-identity checks the same options
// again and reads the files.
func (c Config) validateIdentity() []error {
	if c.Auth.Mode != AuthVerify {
		return nil
	}
	id := c.Auth.Identity
	var errs []error
	required := func(name, value string) {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, errors.New(name+" is required with AUTH_MODE=verify"))
		}
	}
	required("AUTH_USER_ISSUER", id.UserIssuer)
	required("AUTH_USER_AUDIENCE", id.UserAudience)
	required("PEER_USER_ADDR", c.Peers["user"])
	switch c.Env {
	case EnvProd:
		required("SERVICE_TOKEN_FILE", c.Auth.ServiceTokenFile)
		required("AUTH_KUBERNETES_ISSUER", id.Kubernetes.Issuer)
		required("AUTH_KUBERNETES_JWKS_URL", id.Kubernetes.JWKSURL)
		required("AUTH_KUBERNETES_CA_FILE", id.Kubernetes.CAFile)
		required("AUTH_KUBERNETES_API_TOKEN_FILE", id.Kubernetes.APITokenFile)
		required("AUTH_KUBERNETES_NAMESPACE", id.Kubernetes.Namespace)
		if v := id.Kubernetes.JWKSURL; v != "" {
			u, err := url.Parse(v)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				errs = append(errs, errors.New("AUTH_KUBERNETES_JWKS_URL must be HTTPS without credentials, query or fragment"))
			}
		}
		if c.Auth.ServiceTokenFile != "" && filepath.Clean(c.Auth.ServiceTokenFile) == filepath.Clean(id.Kubernetes.APITokenFile) {
			errs = append(errs, errors.New("SERVICE_TOKEN_FILE and AUTH_KUBERNETES_API_TOKEN_FILE must use separate audiences and files"))
		}
		if !id.Stage.empty() {
			errs = append(errs, errors.New("AUTH_STAGE_* stand keys are not allowed with ENV=prod"))
		}
	case EnvStage:
		required("AUTH_STAGE_ISSUER", id.Stage.Issuer)
		required("AUTH_STAGE_SIGNING_KEY_FILE", id.Stage.SigningKeyFile)
		required("AUTH_STAGE_PUBLIC_KEYS_FILE", id.Stage.PublicKeysFile)
	default:
		// go-identity serves only prod and stage; dev uses AUTH_MODE=insecure-dev.
		errs = append(errs, errors.New("AUTH_MODE=verify needs ENV=prod or ENV=stage; use AUTH_MODE=insecure-dev with ENV=dev"))
	}
	return errs
}
