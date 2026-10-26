package config

import (
	"strings"
	"testing"
)

func TestUnitObjectStorageSettingsAndSafeErrors(t *testing.T) {
	vars := base()
	get, list := env(vars)
	c, err := load(get, noFiles, list)
	if err != nil {
		t.Fatal(err)
	}
	if c.ObjectStorage != (ObjectStorageConfig{Endpoint: "https://storage.yandexcloud.net", Bucket: "statement-fixture", KMSKeyID: "test-kms-key", IAMTokenFile: "/run/credentials/statement-iam"}) {
		t.Fatal("object configuration was not loaded")
	}
	for _, tc := range []struct{ key, value string }{
		{"OBJECT_STORAGE_ENDPOINT", "http://storage.example"},
		{"OBJECT_STORAGE_ENDPOINT", "https://user:private-credential@storage.example"},
		{"OBJECT_STORAGE_ENDPOINT", "https://storage.example/objects"},
		{"OBJECT_STORAGE_ENDPOINT", "https://storage.example/?secret=private-credential"},
		{"OBJECT_STORAGE_ENDPOINT", "https://storage.example/#private-credential"},
		{"OBJECT_STORAGE_ENDPOINT", "https:opaque"},
		{"OBJECT_STORAGE_ENDPOINT", "%"},
		{"OBJECT_STORAGE_ENDPOINT", "https://storage.example/?"},
		{"OBJECT_STORAGE_BUCKET", ""}, {"OBJECT_STORAGE_BUCKET", "BadBucket"}, {"OBJECT_STORAGE_BUCKET", "a..b"},
		{"OBJECT_STORAGE_KMS_KEY_ID", ""}, {"OBJECT_STORAGE_KMS_KEY_ID", "key\r\nprivate-credential"}, {"OBJECT_STORAGE_KMS_KEY_ID", strings.Repeat("x", 16385)},
		{"OBJECT_STORAGE_IAM_TOKEN_FILE", ""}, {"OBJECT_STORAGE_IAM_TOKEN_FILE", "relative/token"}, {"OBJECT_STORAGE_IAM_TOKEN_FILE", "/invalid\x00token"},
	} {
		t.Run(tc.key+" boundary", func(t *testing.T) {
			v := base()
			v[tc.key] = tc.value
			get, list := env(v)
			_, err := load(get, noFiles, list)
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("setting %s accepted or wrong error: %v", tc.key, err)
			}
			if strings.Contains(err.Error(), "private-credential") {
				t.Fatal("object configuration disclosed a credential")
			}
		})
	}
	vars = base()
	vars["OBJECT_STORAGE_ENDPOINT"] = "https://127.0.0.1:12345/"
	get, list = env(vars)
	if _, err = load(get, noFiles, list); err != nil {
		t.Fatal("explicit local TLS fixture endpoint rejected", err)
	}
}

func TestUnitObjectStorageIsServeOnlyAndCredentialsAreReloadedElsewhere(t *testing.T) {
	vars := base()
	delete(vars, "OBJECT_STORAGE_BUCKET")
	delete(vars, "OBJECT_STORAGE_KMS_KEY_ID")
	delete(vars, "OBJECT_STORAGE_IAM_TOKEN_FILE")
	get, list := env(vars)
	if _, err := loadMigrate(get, noFiles); err != nil {
		t.Fatal("migrations require cloud credentials", err)
	}
	if _, err := load(get, noFiles, list); err == nil {
		t.Fatal("serve accepted missing storage settings")
	}
	vars = base()
	get, list = env(vars)
	read := func(string) ([]byte, error) {
		t.Fatal("configuration must not cache or print the rotating IAM token")
		return nil, nil
	}
	if _, err := load(get, read, list); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationObjectStorageFromEnvironment(t *testing.T) {
	isolatedEnvironment(t)
	t.Setenv("ENV", "dev")
	t.Setenv("AUTH_MODE", "insecure-dev")
	t.Setenv("DB_PRIMARY_DSN", "postgres://fixture/service")
	t.Setenv("OBJECT_STORAGE_ENDPOINT", "https://storage.example")
	t.Setenv("OBJECT_STORAGE_BUCKET", "owned-statements")
	t.Setenv("OBJECT_STORAGE_KMS_KEY_ID", "configured-key")
	t.Setenv("OBJECT_STORAGE_IAM_TOKEN_FILE", "/run/credentials/rotating-iam")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ObjectStorage.Endpoint != "https://storage.example" || c.ObjectStorage.Bucket != "owned-statements" || c.ObjectStorage.KMSKeyID != "configured-key" || c.ObjectStorage.IAMTokenFile != "/run/credentials/rotating-iam" {
		t.Fatal("serve ignored storage configuration")
	}
	t.Setenv("OBJECT_STORAGE_BUCKET", "")
	if _, err = Load(); err == nil {
		t.Fatal("missing storage bucket accepted")
	}
	if _, err = LoadMigrate(); err != nil {
		t.Fatal("migration loaded serve-only storage values", err)
	}
}
