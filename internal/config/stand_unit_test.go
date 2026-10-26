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
		// systemd ignores spaces around the key's "="; read the key the same way so no
		// directive escapes the checks below.
		key, value, _ := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "Environment":
			// Only the plain NAME=value form: systemd also accepts quoted and space-separated
			// assignments, which this parser would misread and which could hide a secret.
			if strings.ContainsAny(value, "\"' \t\\") {
				t.Fatalf("unit line %q: use one plain NAME=value per Environment=, without quotes, spaces or backslashes", line)
			}
			name, setting, found := strings.Cut(value, "=")
			if !found {
				t.Fatalf("malformed unit line %q", line)
			}
			unit.env[name] = strings.ReplaceAll(setting, "%d", standCredentials)
		case "LoadCredential":
			name, source, _ := strings.Cut(value, ":")
			unit.credentials[name] = source
		case "EnvironmentFile":
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
	// Every credential is read by some variable: an unread copy of the public JWKS (the old
	// per-service path) or a leftover one only makes the start depend on a file nobody reads.
	for name, source := range unit.credentials {
		used := false
		for _, value := range unit.env {
			if value == standCredentials+"/"+name {
				used = true
			}
		}
		if !used {
			t.Fatalf("unit loads credential %s:%s that no variable reads; the public JWKS is read directly from %s", name, source, standPublicKeys)
		}
	}
	for name, value := range unit.env {
		if credential, ok := strings.CutPrefix(value, standCredentials+"/"); ok {
			if _, loaded := unit.credentials[credential]; !loaded {
				t.Fatalf("%s names credential %q that the unit does not load", name, credential)
			}
		}
	}
	// A secret value in Environment= would show up in "systemctl show"; only *_FILE is allowed.
	for _, v := range Vars {
		if _, set := unit.env[v.Name]; v.Secret && set {
			t.Fatalf("unit sets secret %s by value; pass it as %s_FILE from a credential", v.Name, v.Name)
		}
	}
	// Credentials come from this service's own directory of root-owned files, one file per
	// credential named after it: <name> or <name>.<ext>, directly in that directory.
	for name, source := range unit.credentials {
		clean := filepath.Clean(source)
		file := filepath.Base(clean)
		if !filepath.IsAbs(source) || filepath.Dir(clean) != "/etc/finassist/"+standServiceName || strings.TrimSuffix(file, filepath.Ext(file)) != name {
			t.Fatalf("credential %s comes from %q, want /etc/finassist/%s/%s[.ext]", name, source, standServiceName, name)
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
	// Files exist only where systemd puts the declared credentials; each one names its
	// credential, so the DSN below proves which credential DB_PRIMARY_DSN_FILE points at.
	readFile := func(path string) ([]byte, error) {
		name, ok := strings.CutPrefix(path, standCredentials+"/")
		if _, loaded := unit.credentials[name]; !ok || !loaded {
			return noFiles(path)
		}
		return []byte("postgres://" + name + "/" + standServiceName), nil
	}
	get, list := env(vars)
	c, err := load(get, readFile, list)
	if err != nil {
		t.Fatalf("stand unit configuration rejected: %v", err)
	}
	if c.Env != EnvStage || c.Auth.Mode != AuthVerify || c.Service != standServiceName {
		t.Fatalf("stand runs ENV=%s AUTH_MODE=%s SERVICE_NAME=%s, want stage, verify and %s", c.Env, c.Auth.Mode, c.Service, standServiceName)
	}
	if want := "postgres://db-primary-dsn/" + standServiceName; c.DB.PrimaryDSN != want {
		t.Fatalf("primary DSN %q, want %q from the db-primary-dsn credential", c.DB.PrimaryDSN, want)
	}
	stage := c.Auth.Identity.Stage
	if stage.SigningKeyFile != standCredentials+"/stage-signing-key" || stage.PublicKeysFile != standPublicKeys {
		t.Fatalf("stage key files: %+v", stage)
	}
}
