package config

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolatedEnvironment(t *testing.T) {
	t.Helper()
	for _, v := range Vars {
		t.Setenv(v.Name, "")
		if v.Secret {
			t.Setenv(v.Name+"_FILE", "")
		}
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if peerVar.MatchString(key) {
			t.Setenv(key, "")
		}
	}
	t.Setenv("SERVICE_NAME", "integration-service")
}

func TestIntegrationEnvironmentAndMountedCredentials(t *testing.T) {
	isolatedEnvironment(t)
	primary := "postgres://fixture:private-password@primary/service"
	replica := "postgres://fixture:replica-password@replica/service"
	dir := t.TempDir()
	for name, value := range map[string]string{"primary": primary, "replica": replica} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value+"\r\n\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("DB_"+strings.ToUpper(name)+"_DSN_FILE", path)
	}
	t.Setenv("ENV", "dev")
	t.Setenv("AUTH_MODE", "insecure-dev")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("DB_MAX_CONNS", "3")
	t.Setenv("GRPC_MAX_RECV_MSG_SIZE", "1024")
	t.Setenv("GRPC_ADDR", "127.0.0.1:9090")
	t.Setenv("HTTP_ADDR", "127.0.0.1:9091")
	t.Setenv("SHUTDOWN_DELAY", "0s")
	t.Setenv("SHUTDOWN_TIMEOUT", "9s")
	t.Setenv("PEER_BANK_STATEMENT_DATA_ADDR", "dns:///statements:9090")
	t.Setenv("SERVICE_TOKEN_FILE", filepath.Join(dir, "service-token"))
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DB.PrimaryDSN != primary || c.DB.ReplicaDSN != replica {
		t.Fatal("mounted credentials not loaded exactly")
	}
	if c.Service != "integration-service" || c.Env != EnvDev || c.Auth.Mode != AuthInsecureDev || !c.GRPC.Reflection || c.Log.Level != slog.LevelDebug || c.Log.Format != "text" || c.DB.MaxConns != 3 || c.GRPC.MaxRecvMsgSize != 1024 || c.ShutdownDelay != 0 || c.ShutdownTimeout != 9*time.Second || c.Peers["bank-statement-data"] != "dns:///statements:9090" || c.Auth.ServiceTokenFile != filepath.Join(dir, "service-token") {
		t.Fatal("environment overrides not applied")
	}
	t.Setenv("MIGRATE_LOCK_TIMEOUT", "2s")
	t.Setenv("MIGRATE_STATEMENT_TIMEOUT", "3m")
	t.Setenv("MIGRATE_WAIT_TIMEOUT", "4m")
	m, err := LoadMigrate()
	if err != nil {
		t.Fatal(err)
	}
	if m.PrimaryDSN != primary || m.LockTimeout != 2*time.Second || m.StatementTimeout != 3*time.Minute || m.WaitTimeout != 4*time.Minute {
		t.Fatal("migration overrides not applied")
	}
	// The checked-in generated tables must describe the configuration actually loaded.
	doc, err := os.ReadFile("../../docs/config.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"serve", "migrate"} {
		if !strings.Contains(string(doc), Table(command)) {
			t.Fatalf("generated %s table differs from declarations", command)
		}
	}
}

func TestIntegrationCredentialFailuresAndConfigurationValidation(t *testing.T) {
	for _, tc := range []struct{ name, key, value, want string }{
		{"invalid environment", "ENV", "qa", "ENV="}, {"invalid format", "LOG_FORMAT", "xml", "LOG_FORMAT="},
		{"invalid level", "LOG_LEVEL", "verbose", "LOG_LEVEL="}, {"invalid integer", "DB_MAX_CONNS", "many", "not an integer"},
		{"zero connections", "DB_MAX_CONNS", "0", "must be positive"}, {"zero message limit", "GRPC_MAX_RECV_MSG_SIZE", "0", "must be positive"},
		{"negative delay", "SHUTDOWN_DELAY", "-1s", "non-negative duration"}, {"invalid timeout", "SHUTDOWN_TIMEOUT", "later", "non-negative duration"},
		{"invalid auth", "AUTH_MODE", "unknown", "AUTH_MODE="}, {"production dev auth", "AUTH_MODE", "insecure-dev", "only with ENV=dev"},
		{"missing service", "SERVICE_NAME", "", "SERVICE_NAME is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedEnvironment(t)
			t.Setenv("DB_PRIMARY_DSN", "postgres://fixture/private")
			t.Setenv(tc.key, tc.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %s, got %v", tc.want, err)
			}
		})
	}
	for _, command := range []string{"serve", "migrate"} {
		t.Run(command, func(t *testing.T) {
			isolatedEnvironment(t)
			load := func() error {
				if command == "serve" {
					_, err := Load()
					return err
				}
				_, err := LoadMigrate()
				return err
			}
			if err := load(); err == nil || !strings.Contains(err.Error(), "DB_PRIMARY_DSN") {
				t.Fatal("missing credential accepted")
			}
			t.Setenv("DB_PRIMARY_DSN_FILE", filepath.Join(t.TempDir(), "missing"))
			if err := load(); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("missing file cause lost: %v", err)
			}
			t.Setenv("DB_PRIMARY_DSN", "postgres://secret-user:secret-password@primary/private")
			err := load()
			if err == nil || !strings.Contains(err.Error(), "both DB_PRIMARY_DSN") {
				t.Fatal("conflicting secret sources accepted")
			}
			if strings.Contains(err.Error(), "secret-password") {
				t.Fatal("credential leaked")
			}
		})
	}
	for _, value := range []string{"0s", "-1s", "later"} {
		t.Run("migration timeout "+value, func(t *testing.T) {
			isolatedEnvironment(t)
			t.Setenv("DB_PRIMARY_DSN", "postgres://fixture/private")
			for _, key := range []string{"MIGRATE_LOCK_TIMEOUT", "MIGRATE_STATEMENT_TIMEOUT", "MIGRATE_WAIT_TIMEOUT"} {
				t.Setenv(key, value)
			}
			_, err := LoadMigrate()
			if err == nil {
				t.Fatal("invalid timeouts accepted")
			}
			for _, key := range []string{"MIGRATE_LOCK_TIMEOUT", "MIGRATE_STATEMENT_TIMEOUT", "MIGRATE_WAIT_TIMEOUT"} {
				if !strings.Contains(err.Error(), key) {
					t.Fatalf("missing joined validation for %s", key)
				}
			}
		})
	}
}
