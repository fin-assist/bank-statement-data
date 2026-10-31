package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/config"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
	"github.com/fin-assist/bank-statement-data/internal/testobjects"
)

// commandObjectOptions supplies a real TLS private bucket and explicit trust to
// this invocation only. Production defaults never select this test boundary.
func commandObjectOptions(t *testing.T) app.Options {
	t.Helper()
	objects := testobjects.New(t)
	var cfg config.Config
	objects.Configure(&cfg)
	for key, value := range map[string]string{
		"TBANK_LOGIN_URL":               "https://tbank-online.com/login",
		"PEER_ANALYTICS_ADDR":           "passthrough:///analytics.invalid:9090",
		"OBJECT_STORAGE_ENDPOINT":       cfg.ObjectStorage.Endpoint,
		"OBJECT_STORAGE_BUCKET":         cfg.ObjectStorage.Bucket,
		"OBJECT_STORAGE_KMS_KEY_ID":     cfg.ObjectStorage.KMSKeyID,
		"OBJECT_STORAGE_IAM_TOKEN_FILE": cfg.ObjectStorage.IAMTokenFile,
	} {
		t.Setenv(key, value)
	}
	return app.Options{ObjectClient: objects.Server.Client()}
}

func TestIntegrationCommandDoesNotImplicitlyTrustFixtureTLS(t *testing.T) {
	_ = commandObjectOptions(t) // Valid configuration, deliberately do not forward fixture trust.
	for key, value := range map[string]string{
		"SERVICE_NAME": "cli-default-tls", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error",
		"DB_PRIMARY_DSN": testdb.DSN(t), "DB_PRIMARY_DSN_FILE": "", "GRPC_ADDR": "127.0.0.1:0", "HTTP_ADDR": "127.0.0.1:0",
	} {
		t.Setenv(key, value)
	}
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := runServe(ctx); err == nil || !strings.Contains(err.Error(), "object bucket safety check failed") {
		t.Fatal("default CLI assembly implicitly trusted the private fixture CA", err)
	}
}
