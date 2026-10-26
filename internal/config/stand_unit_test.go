package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	standServiceName = "bank-statement-data"
	standUnitFile    = standServiceName + ".service"
	standCredentials = "/run/credentials/" + standUnitFile
	standServiceDir  = "/etc/finassist/" + standServiceName
	standEnvFile     = standServiceDir + "/env"
	// standPublicKeys is the JWKS of the stand shared by all services (FIN-349): one path,
	// mode 0644, read directly by DynamicUser without LoadCredential.
	standPublicKeys = "/etc/finassist/stage-public.jwks"
)

// standEnvFileContents is the env file of the stand: only the non-secret settings the
// unit comment names.
var standEnvFileContents = map[string]string{
	"AUTH_STAGE_ISSUER": "stage-services", "AUTH_USER_ISSUER": "https://user.example",
	"AUTH_USER_AUDIENCE": "finassist", "PEER_USER_ADDR": "dns:///user:9090",
}

// standUnit is the settings the example systemd unit of the stand gives the process:
// active Environment= lines with %d resolved to the credentials directory, the
// LoadCredential= names and sources that directory holds, and the EnvironmentFile= paths.
type standUnit struct {
	env         map[string]string
	credentials map[string]string // credential name -> source path
	envFiles    []string
}

// parseStandUnit reads the directives that shape the process environment. It accepts them
// only in [Service] and only in the forms it can read without misreading a secret.
func parseStandUnit(text string) (standUnit, error) {
	unit := standUnit{env: map[string]string{}, credentials: map[string]string{}}
	section := ""
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		// systemd ignores spaces around the key's "="; read the key the same way so no
		// directive escapes the checks below.
		key, value, _ := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "Environment", "EnvironmentFile", "LoadCredential", "SetCredential", "SetCredentialEncrypted", "LoadCredentialEncrypted", "ImportCredential":
			if section != "[Service]" {
				return standUnit{}, fmt.Errorf("%s= in %s, want [Service]: %q", key, section, line)
			}
		}
		switch key {
		case "Environment":
			// Only the plain NAME=value form: systemd also accepts quoted and space-separated
			// assignments, which this parser would misread and which could hide a secret.
			if strings.ContainsAny(value, "\"' \t\\") {
				return standUnit{}, fmt.Errorf("unit line %q: use one plain NAME=value per Environment=, without quotes, spaces or backslashes", line)
			}
			name, setting, found := strings.Cut(value, "=")
			if !found {
				return standUnit{}, fmt.Errorf("malformed unit line %q", line)
			}
			unit.env[name] = strings.ReplaceAll(setting, "%d", standCredentials)
		case "LoadCredential":
			name, source, _ := strings.Cut(value, ":")
			// Reject repeats instead of depending on which source a systemd version keeps.
			if _, duplicate := unit.credentials[name]; duplicate {
				return standUnit{}, fmt.Errorf("unit loads credential %s twice", name)
			}
			unit.credentials[name] = source
		case "EnvironmentFile":
			unit.envFiles = append(unit.envFiles, value)
		case "SetCredential", "SetCredentialEncrypted", "LoadCredentialEncrypted", "ImportCredential":
			// These directives embed secrets or load them from sources this check does not cover.
			return standUnit{}, fmt.Errorf("unit line %q: secrets come only from LoadCredential= files", line)
		}
	}
	if err := scanner.Err(); err != nil {
		return standUnit{}, err
	}
	return unit, nil
}

// declaredUnitVar reports whether the unit may set name: a declared non-secret setting, a
// secret only as its *_FILE, or a peer address.
func declaredUnitVar(name string) bool {
	if peerVar.MatchString(name) {
		return true
	}
	for _, v := range Vars {
		if (name == v.Name && !v.Secret) || (name == v.Name+"_FILE" && v.Secret) {
			return true
		}
	}
	return false
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

// checkStandUnit verifies the stand unit text against the configuration (FIN-349): the
// unit sets ENV=stage and keeps the default AUTH_MODE=verify, so it must itself pass the
// stand key files, the shared JWKS at its common path included; with only the non-secret
// settings its comment names in its env file, the configuration loads.
func checkStandUnit(text string) error {
	unit, err := parseStandUnit(text)
	if err != nil {
		return err
	}
	if unit.env["ENV"] != "stage" {
		return fmt.Errorf("unit ENV=%q, want stage", unit.env["ENV"])
	}
	// SERVICE_NAME is the kid/sub of the stand signing key, fixed by the unit itself.
	if unit.env["SERVICE_NAME"] != standServiceName {
		return fmt.Errorf("unit SERVICE_NAME=%q, want %s", unit.env["SERVICE_NAME"], standServiceName)
	}
	for _, name := range []string{"AUTH_STAGE_SIGNING_KEY_FILE", "AUTH_STAGE_PUBLIC_KEYS_FILE", "DB_PRIMARY_DSN_FILE"} {
		if unit.env[name] == "" {
			return fmt.Errorf("unit does not set %s", name)
		}
	}
	if got := unit.env["AUTH_STAGE_PUBLIC_KEYS_FILE"]; got != standPublicKeys {
		return fmt.Errorf("unit AUTH_STAGE_PUBLIC_KEYS_FILE=%q, want the shared %s", got, standPublicKeys)
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
			return fmt.Errorf("unit loads credential %s:%s that no variable reads; the public JWKS is read directly from %s", name, source, standPublicKeys)
		}
	}
	for name, value := range unit.env {
		if credential, ok := strings.CutPrefix(value, standCredentials+"/"); ok {
			if _, loaded := unit.credentials[credential]; !loaded {
				return fmt.Errorf("%s names credential %q that the unit does not load", name, credential)
			}
		}
	}
	// Only declared settings, and a secret only as its *_FILE: a value in Environment= shows
	// up in "systemctl show", and a name outside Vars (PGPASSWORD, which pgx reads) would
	// carry a secret past a check by Vars alone.
	for name := range unit.env {
		if !declaredUnitVar(name) {
			return fmt.Errorf("unit sets %s, which is neither a declared non-secret setting nor a secret's _FILE", name)
		}
	}
	// Credentials come from this service's own directory of root-owned files, one file per
	// credential named after it: <name> or <name>.<ext>, directly in that directory.
	for name, source := range unit.credentials {
		clean := filepath.Clean(source)
		base := filepath.Base(clean)
		named := base == name || strings.TrimSuffix(base, filepath.Ext(base)) == name
		if !filepath.IsAbs(source) || filepath.Dir(clean) != standServiceDir || !named {
			return fmt.Errorf("credential %s comes from %q, want %s/%s[.ext]", name, source, standServiceDir, name)
		}
	}
	for _, name := range secretFileVars() {
		value, set := unit.env[name]
		if !set {
			continue
		}
		credential, ok := strings.CutPrefix(value, standCredentials+"/")
		if !ok || strings.Contains(credential, "/") {
			return fmt.Errorf("secret %s=%q is not a systemd credential under %s", name, value, standCredentials)
		}
	}
	if len(unit.envFiles) != 1 || unit.envFiles[0] != standEnvFile {
		return fmt.Errorf("unit EnvironmentFile=%q, want exactly %s", unit.envFiles, standEnvFile)
	}
	// Merge in systemd order: Environment= of the unit first, EnvironmentFile= over it.
	// An env file that repeated a unit setting would silently replace it, so refuse that.
	vars := map[string]string{}
	for name, value := range unit.env {
		vars[name] = value
	}
	for name, value := range standEnvFileContents {
		if _, ok := unit.env[name]; ok {
			return fmt.Errorf("the env file would override the unit setting %s", name)
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
		return fmt.Errorf("stand unit configuration rejected: %w", err)
	}
	if c.Env != EnvStage || c.Auth.Mode != AuthVerify || c.Service != standServiceName {
		return fmt.Errorf("stand runs ENV=%s AUTH_MODE=%s SERVICE_NAME=%s, want stage, verify and %s", c.Env, c.Auth.Mode, c.Service, standServiceName)
	}
	if want := "postgres://db-primary-dsn/" + standServiceName; c.DB.PrimaryDSN != want {
		return fmt.Errorf("primary DSN %q, want %q from the db-primary-dsn credential", c.DB.PrimaryDSN, want)
	}
	stage := c.Auth.Identity.Stage
	if stage.SigningKeyFile != unit.env["AUTH_STAGE_SIGNING_KEY_FILE"] || stage.PublicKeysFile != standPublicKeys {
		return fmt.Errorf("stage key files: %+v", stage)
	}
	return nil
}

func readStandUnitText(t *testing.T) string {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", standUnitFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

func TestUnitStandUnitSatisfiesStageIdentity(t *testing.T) {
	if err := checkStandUnit(readStandUnitText(t)); err != nil {
		t.Fatal(err)
	}
}

// unitEdit is one text change of the real unit; the anchor must occur in it exactly once,
// so a mutation cannot silently stop applying when the unit changes.
type unitEdit struct{ old, new string }

const (
	anchorPublicKeys = "Environment=AUTH_STAGE_PUBLIC_KEYS_FILE=/etc/finassist/stage-public.jwks"
	anchorDSNFile    = "Environment=DB_PRIMARY_DSN_FILE=%d/db-primary-dsn"
	anchorDSNCred    = "LoadCredential=db-primary-dsn:/etc/finassist/bank-statement-data/db-primary-dsn"
	anchorEnvFile    = "EnvironmentFile=/etc/finassist/bank-statement-data/env"
	anchorSigningEnv = "Environment=AUTH_STAGE_SIGNING_KEY_FILE=%d/stage-signing-key"
	anchorSigning    = "LoadCredential=stage-signing-key:/etc/finassist/bank-statement-data/stage-signing-key.pem"
)

func after(anchor, line string) unitEdit { return unitEdit{anchor, anchor + "\n" + line} }

func mutateStandUnit(t *testing.T, text string, edits ...unitEdit) string {
	t.Helper()
	for _, e := range edits {
		if n := strings.Count(text, e.old); n != 1 {
			t.Fatalf("anchor %q occurs %d times in the unit, want 1", e.old, n)
		}
		text = strings.Replace(text, e.old, e.new, 1)
	}
	return text
}

// The check refuses every unit shape that would hide a secret, read the old JWKS path, or
// diverge from the configuration; each case names the refusal it expects.
func TestUnitStandUnitCheckRejects(t *testing.T) {
	text := readStandUnitText(t)
	cases := []struct {
		name  string
		edits []unitEdit
		want  string
	}{
		{"old public LoadCredential", []unitEdit{after(anchorPublicKeys, "LoadCredential=stage-public-keys:/etc/finassist/stand/stage-public-keys.json")}, "no variable reads"},
		{"spaced old public LoadCredential", []unitEdit{after(anchorPublicKeys, "LoadCredential = stage-public-keys:/etc/finassist/stand/stage-public-keys.json")}, "no variable reads"},
		{"JWKS credential from the shared path", []unitEdit{after(anchorPublicKeys, "LoadCredential=jwks:/etc/finassist/stage-public.jwks")}, "no variable reads"},
		{"unread credential in the service dir", []unitEdit{after(anchorPublicKeys, "LoadCredential=jwks:/etc/finassist/bank-statement-data/jwks.json")}, "no variable reads"},
		{"old JWKS path", []unitEdit{{anchorPublicKeys, "Environment=AUTH_STAGE_PUBLIC_KEYS_FILE=/etc/finassist/stand/stage-public-keys.json"}}, "want the shared"},
		{"wrong SERVICE_NAME", []unitEdit{{"Environment=SERVICE_NAME=bank-statement-data", "Environment=SERVICE_NAME=analytics"}}, "want bank-statement-data"},
		{"no ENV", []unitEdit{{"Environment=ENV=stage\n", ""}}, "want stage"},
		{"quoted secret", []unitEdit{after(anchorPublicKeys, `Environment="DB_REPLICA_DSN=postgres://u:p@db/x"`)}, "plain NAME=value"},
		{"two assignments", []unitEdit{after(anchorPublicKeys, "Environment=HTTP_ADDR=:9091 DB_REPLICA_DSN=postgres://u:p@db/x")}, "plain NAME=value"},
		{"backslash", []unitEdit{after(anchorPublicKeys, `Environment=HTTP_ADDR=:9091\`)}, "plain NAME=value"},
		{"secret by value", []unitEdit{after(anchorPublicKeys, "Environment=DB_REPLICA_DSN=postgres://u:p@db/x")}, "neither a declared non-secret setting"},
		{"spaced secret by value", []unitEdit{after(anchorPublicKeys, "Environment = DB_REPLICA_DSN=postgres://u:p@db/x")}, "neither a declared non-secret setting"},
		{"undeclared _FILE reading a credential", []unitEdit{after(anchorPublicKeys, "LoadCredential=stage-public-keys:/etc/finassist/bank-statement-data/stage-public-keys.json\nEnvironment=UNUSED_FILE=%d/stage-public-keys")}, "UNUSED_FILE, which is neither a declared non-secret setting"},
		{"PGPASSWORD", []unitEdit{after(anchorPublicKeys, "Environment=PGPASSWORD=s3cret")}, "neither a declared non-secret setting"},
		{"SetCredential", []unitEdit{after(anchorDSNCred, "SetCredential=db-primary-dsn:postgres://u:p@db/x")}, "only from LoadCredential"},
		{"duplicate LoadCredential with a foreign source first", []unitEdit{{anchorDSNCred, "LoadCredential=db-primary-dsn:/root/db-primary-dsn\n" + anchorDSNCred}}, "unit loads credential db-primary-dsn twice"},
		{"duplicate LoadCredential with valid sources", []unitEdit{{anchorDSNCred, "LoadCredential=db-primary-dsn:" + standServiceDir + "/db-primary-dsn.txt\n" + anchorDSNCred}}, "unit loads credential db-primary-dsn twice"},
		{"identical LoadCredential repeated", []unitEdit{after(anchorDSNCred, anchorDSNCred)}, "unit loads credential db-primary-dsn twice"},
		{"SetCredentialEncrypted", []unitEdit{after(anchorDSNCred, "SetCredentialEncrypted=db-primary-dsn:encrypted")}, "only from LoadCredential"},
		{"LoadCredentialEncrypted", []unitEdit{after(anchorDSNCred, "LoadCredentialEncrypted=db-primary-dsn:/root/db-primary-dsn")}, "only from LoadCredential"},
		{"spaced LoadCredentialEncrypted", []unitEdit{after(anchorDSNCred, "LoadCredentialEncrypted = db-primary-dsn:/root/db-primary-dsn")}, "only from LoadCredential"},
		{"ImportCredential", []unitEdit{after(anchorDSNCred, "ImportCredential=db-primary-dsn")}, "only from LoadCredential"},
		{"spaced ImportCredential", []unitEdit{after(anchorDSNCred, "ImportCredential = db-primary-dsn")}, "only from LoadCredential"},
		{"Environment outside [Service]", []unitEdit{{"[Install]", "[Install]\nEnvironment=HTTP_ADDR=:9091"}}, "want [Service]"},
		{"LoadCredential outside [Service]", []unitEdit{{"[Install]", "[Install]\nLoadCredential=jwks:/etc/finassist/stage-public.jwks"}}, "want [Service]"},
		{"DSN as a plain path", []unitEdit{{anchorDSNFile, "Environment=DB_PRIMARY_DSN_FILE=/etc/finassist/bank-statement-data/db-primary-dsn"}}, "no variable reads"},
		{"replica secret as a plain path", []unitEdit{after(anchorDSNFile, "Environment=DB_REPLICA_DSN_FILE=/etc/finassist/bank-statement-data/replica")}, "is not a systemd credential"},
		{"signing key as a plain path", []unitEdit{{anchorSigningEnv, "Environment=AUTH_STAGE_SIGNING_KEY_FILE=/etc/finassist/bank-statement-data/stage-signing-key.pem"}}, "no variable reads"},
		{"no DSN LoadCredential", []unitEdit{{anchorDSNCred + "\n", ""}}, "does not load"},
		{"DSN from the signing key", []unitEdit{{anchorDSNFile, "Environment=DB_PRIMARY_DSN_FILE=%d/stage-signing-key\nEnvironment=DB_REPLICA_DSN_FILE=%d/db-primary-dsn"}}, "from the db-primary-dsn credential"},
		{"source name mismatch", []unitEdit{{anchorDSNCred, "LoadCredential=db-primary-dsn:/etc/finassist/bank-statement-data/missing-dsn"}}, "want /etc/finassist/bank-statement-data/db-primary-dsn"},
		{"source outside via ..", []unitEdit{{anchorDSNCred, "LoadCredential=db-primary-dsn:/etc/finassist/bank-statement-data/../another-service/db-primary-dsn"}}, "want /etc/finassist/bank-statement-data/db-primary-dsn"},
		{"source in a subdirectory", []unitEdit{{anchorDSNCred, "LoadCredential=db-primary-dsn:/etc/finassist/bank-statement-data/sub/db-primary-dsn"}}, "want /etc/finassist/bank-statement-data/db-primary-dsn"},
		{"relative source", []unitEdit{{anchorDSNCred, "LoadCredential=db-primary-dsn:etc/finassist/bank-statement-data/db-primary-dsn"}}, "want /etc/finassist/bank-statement-data/db-primary-dsn"},
		{"source outside the service dir", []unitEdit{{anchorSigning, "LoadCredential=stage-signing-key:/root/stage-signing-key.pem"}}, "want /etc/finassist/bank-statement-data/stage-signing-key"},
		{"second EnvironmentFile", []unitEdit{after(anchorEnvFile, "EnvironmentFile=/etc/finassist/other/env")}, "want exactly"},
		{"foreign EnvironmentFile", []unitEdit{{anchorEnvFile, "EnvironmentFile=/etc/finassist/other/env"}}, "want exactly"},
		{"no EnvironmentFile", []unitEdit{{anchorEnvFile + "\n", ""}}, "want exactly"},
		{"env file overrides the unit", []unitEdit{after(anchorPublicKeys, "Environment=PEER_USER_ADDR=dns:///x:1")}, "would override the unit setting PEER_USER_ADDR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStandUnit(mutateStandUnit(t, text, tc.edits...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkStandUnit = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// Legitimate unit shapes the check must not refuse.
func TestUnitStandUnitCheckAccepts(t *testing.T) {
	text := readStandUnitText(t)
	cases := []struct {
		name  string
		edits []unitEdit
	}{
		{"spaces around the key", []unitEdit{{anchorPublicKeys, "Environment = AUTH_STAGE_PUBLIC_KEYS_FILE=/etc/finassist/stage-public.jwks"}, {anchorDSNCred, "LoadCredential = db-primary-dsn:/etc/finassist/bank-statement-data/db-primary-dsn"}}},
		{"credential name with a dot and the exact file name", []unitEdit{{anchorSigning, "LoadCredential=stage-signing.key:/etc/finassist/bank-statement-data/stage-signing.key"}, {anchorSigningEnv, "Environment=AUTH_STAGE_SIGNING_KEY_FILE=%d/stage-signing.key"}}},
		{"replica credential", []unitEdit{after(anchorDSNFile, "LoadCredential=db-replica-dsn:/etc/finassist/bank-statement-data/db-replica-dsn\nEnvironment=DB_REPLICA_DSN_FILE=%d/db-replica-dsn")}},
		{"peer address", []unitEdit{after(anchorPublicKeys, "Environment=PEER_ANALYTICS_ADDR=dns:///127.0.0.1:9093")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkStandUnit(mutateStandUnit(t, text, tc.edits...)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
