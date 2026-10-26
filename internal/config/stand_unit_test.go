package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// standUnit is the settings the example systemd unit of the stand gives the process:
// active Environment= lines with %d resolved to the credentials directory, the
// LoadCredential= names and sources that directory holds, and the EnvironmentFile= paths.
type standUnit struct {
	env         map[string]string
	credentials map[string]string // credential name -> source path
	envFiles    []string
}

const (
	standServiceName = "bank-statement-data"
	standUnitFile    = standServiceName + ".service"
	standCredentials = "/run/credentials/" + standUnitFile
	standEnvFile     = "/etc/finassist/" + standServiceName + "/env"
	// standPublicKeys is the JWKS of the stand shared by all services (FIN-349): one path,
	// mode 0644, read directly by DynamicUser without LoadCredential.
	standPublicKeys = "/etc/finassist/stage-public.jwks"
)

func readStandUnit(t *testing.T) standUnit {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "deploy", "systemd", standUnitFile))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	unit := standUnit{env: map[string]string{}, credentials: map[string]string{}}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if value, ok := strings.CutPrefix(line, "Environment="); ok {
			name, setting, found := strings.Cut(value, "=")
			if !found {
				t.Fatalf("malformed unit line %q", line)
			}
			unit.env[name] = strings.ReplaceAll(setting, "%d", standCredentials)
		}
		if value, ok := strings.CutPrefix(line, "LoadCredential="); ok {
			name, source, _ := strings.Cut(value, ":")
			unit.credentials[name] = source
		}
		if value, ok := strings.CutPrefix(line, "EnvironmentFile="); ok {
			unit.envFiles = append(unit.envFiles, value)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return unit
}

// secretFileVars is every *_FILE variable that names a secret: the Secret variables of
// Vars and the stand signing key. The unit may pass them only as systemd credentials.
func secretFileVars() []string {
	names := []string{"AUTH_STAGE_SIGNING_KEY_FILE"}
	for _, v := range Vars {
		if v.Secret {
			names = append(names, v.Name+"_FILE")
		}
	}
	return names
}

// FIN-349: the example unit sets ENV=stage and keeps the default AUTH_MODE=verify, so it
// must itself pass the stand key files, the shared JWKS at its common path included; with
// only the non-secret settings its comment names in its env file, the configuration loads.
func TestUnitStandUnitSatisfiesStageIdentity(t *testing.T) {
	unit := readStandUnit(t)
	if unit.env["ENV"] != "stage" {
		t.Fatalf("unit ENV=%q, want stage", unit.env["ENV"])
	}
	// SERVICE_NAME is the kid/sub of the stand signing key, fixed by the unit itself.
	if unit.env["SERVICE_NAME"] != standServiceName {
		t.Fatalf("unit SERVICE_NAME=%q, want %s", unit.env["SERVICE_NAME"], standServiceName)
	}
	for _, name := range []string{"AUTH_STAGE_SIGNING_KEY_FILE", "AUTH_STAGE_PUBLIC_KEYS_FILE", "DB_PRIMARY_DSN_FILE"} {
		if unit.env[name] == "" {
			t.Fatalf("unit does not set %s", name)
		}
	}
	if got := unit.env["AUTH_STAGE_PUBLIC_KEYS_FILE"]; got != standPublicKeys {
		t.Fatalf("unit AUTH_STAGE_PUBLIC_KEYS_FILE=%q, want the shared %s", got, standPublicKeys)
	}
	// The public JWKS is read directly; a credential copy would be the old per-service path.
	for name, source := range unit.credentials {
		if strings.Contains(name, "public") || strings.Contains(source, "stage-public") {
			t.Fatalf("unit loads the public JWKS as credential %s:%s; it must read %s directly", name, source, standPublicKeys)
		}
	}
	for name, value := range unit.env {
		if credential, ok := strings.CutPrefix(value, standCredentials+"/"); ok {
			if _, loaded := unit.credentials[credential]; !loaded {
				t.Fatalf("%s names credential %q that the unit does not load", name, credential)
			}
		}
	}
	for _, name := range secretFileVars() {
		value, set := unit.env[name]
		if !set {
			continue
		}
		credential, ok := strings.CutPrefix(value, standCredentials+"/")
		if !ok || strings.Contains(credential, "/") {
			t.Fatalf("secret %s=%q is not a systemd credential under %s", name, value, standCredentials)
		}
	}
	if len(unit.envFiles) != 1 || unit.envFiles[0] != standEnvFile {
		t.Fatalf("unit EnvironmentFile=%q, want exactly %s", unit.envFiles, standEnvFile)
	}
	// The contents of standEnvFile on the stand: only non-secret settings named in the unit comment.
	envFiles := map[string]map[string]string{standEnvFile: {
		"AUTH_STAGE_ISSUER": "stage-services", "AUTH_USER_ISSUER": "https://user.example",
		"AUTH_USER_AUDIENCE": "finassist", "PEER_USER_ADDR": "dns:///user:9090",
	}}
	// Merge in systemd order: Environment= of the unit first, EnvironmentFile= over it.
	// An env file that repeated a unit setting would silently replace it, so refuse that.
	vars := map[string]string{}
	for name, value := range unit.env {
		vars[name] = value
	}
	for name, value := range envFiles[unit.envFiles[0]] {
		if _, ok := unit.env[name]; ok {
			t.Fatalf("the env file would override the unit setting %s", name)
		}
		vars[name] = value
	}
	// The DSN exists only where systemd puts the declared db-primary-dsn credential.
	dsnPath := ""
	if _, ok := unit.credentials["db-primary-dsn"]; ok {
		dsnPath = standCredentials + "/db-primary-dsn"
	}
	readFile := func(path string) ([]byte, error) {
		if dsnPath != "" && path == dsnPath {
			return []byte("postgres://primary/" + standServiceName), nil
		}
		return noFiles(path)
	}
	get, list := env(vars)
	c, err := load(get, readFile, list)
	if err != nil {
		t.Fatalf("stand unit configuration rejected: %v", err)
	}
	if c.Env != EnvStage || c.Auth.Mode != AuthVerify || c.Service != standServiceName {
		t.Fatalf("stand runs ENV=%s AUTH_MODE=%s SERVICE_NAME=%s, want stage, verify and %s", c.Env, c.Auth.Mode, c.Service, standServiceName)
	}
	stage := c.Auth.Identity.Stage
	if stage.SigningKeyFile != standCredentials+"/stage-signing-key" || stage.PublicKeysFile != standPublicKeys {
		t.Fatalf("stage key files: %+v", stage)
	}
}
