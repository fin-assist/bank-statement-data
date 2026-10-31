package config

import (
	"strings"
	"testing"
)

func TestUnitTBankLoginURLRequiredAndHTTPS(t *testing.T) {
	for _, value := range []string{"", "http://bank.example/login", "/login", "https:///login", "https://user:secret@bank.example/login", "https://bank.example/login?token=secret", "https://bank.example/login?", "https://bank.example/login#secret", "https://bank.example/login ", "https://%zz"} {
		vars := base()
		vars["TBANK_LOGIN_URL"] = value
		get, list := env(vars)
		_, err := load(get, noFiles, list)
		if err == nil || !strings.Contains(err.Error(), "TBANK_LOGIN_URL") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid login URL was accepted or exposed: %v", err)
		}
	}
	vars := base()
	delete(vars, "TBANK_LOGIN_URL")
	get, list := env(vars)
	if _, err := load(get, noFiles, list); err == nil {
		t.Fatal("missing URL accepted")
	}
	for _, value := range []string{"https://tbank-online.com/login", "https://bank.example/new-login"} {
		vars["TBANK_LOGIN_URL"] = value
		get, list := env(vars)
		cfg, err := load(get, noFiles, list)
		if err != nil || cfg.TBankLoginURL != value {
			t.Fatalf("configured destination was not preserved: %v", err)
		}
	}
}

func TestIntegrationTBankLoginURLReloadAndInvalidValue(t *testing.T) {
	isolatedEnvironment(t)
	t.Setenv("ENV", "dev")
	t.Setenv("AUTH_MODE", "insecure-dev")
	t.Setenv("DB_PRIMARY_DSN", "postgres://fixture/private")
	for _, value := range []string{"https://tbank-online.com/login", "https://bank.example/new-login"} {
		t.Setenv("TBANK_LOGIN_URL", value)
		cfg, err := Load()
		if err != nil || cfg.TBankLoginURL != value {
			t.Fatalf("environment reload failed: %v", err)
		}
	}
	for _, value := range []string{"", "http://bank.example/login"} {
		t.Setenv("TBANK_LOGIN_URL", value)
		if _, err := Load(); err == nil {
			t.Fatal("invalid environment accepted")
		}
	}
}
