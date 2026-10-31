package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestAPIProcessRejectsInvalidTBankLoginURLBeforeConnecting(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, value := range []string{"", "http://bank.example/login", "https://user:private-password@bank.example/login"} {
		t.Run(value, func(t *testing.T) {
			for key, setting := range map[string]string{"SERVICE_NAME": "login-url-validation", "ENV": "dev", "AUTH_MODE": "insecure-dev", "DB_PRIMARY_DSN": "postgres://unreachable.invalid/db", "DB_PRIMARY_DSN_FILE": "", "TBANK_LOGIN_URL": value} {
				t.Setenv(key, setting)
			}
			var stderr bytes.Buffer
			if code := mainExit([]string{"serve"}, &stderr); code != 1 || !strings.Contains(stderr.String(), "TBANK_LOGIN_URL") || strings.Contains(stderr.String(), "private-password") {
				t.Fatalf("invalid startup URL was accepted or exposed: exit=%d", code)
			}
		})
	}
}
