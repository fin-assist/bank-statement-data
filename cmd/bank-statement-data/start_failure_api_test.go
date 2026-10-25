package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/testdb"
)

func TestAPIProcessBindFailureClosesApplicationPoolsAndNeverStartsHTTP(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	dsn := testdb.DSN(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	httpAddress, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := httpAddress.Addr().String()
	_ = httpAddress.Close()
	for key, value := range map[string]string{"LOG_FORMAT": "text", "SERVICE_NAME": "cli-bind-failure", "ENV": "dev", "AUTH_MODE": "insecure-dev", "DB_PRIMARY_DSN": dsn, "DB_PRIMARY_DSN_FILE": "", "GRPC_ADDR": occupied.Addr().String(), "HTTP_ADDR": address, "SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "1s"} {
		t.Setenv(key, value)
	}
	var stderr bytes.Buffer
	if code := mainExit([]string{"serve"}, &stderr); code != 1 {
		t.Fatalf("bind conflict accepted: code=%d log=%q", code, stderr.String())
	}
	probe, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		_ = probe.Close()
		t.Fatal("HTTP started after gRPC bind failure")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	ticker := time.NewTicker(time.Millisecond * 10)
	defer ticker.Stop()
	for {
		var remaining int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE application_name=$1", "cli-bind-failure").Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("application pools leaked after bind failure")
		case <-ticker.C:
		}
	}
}
