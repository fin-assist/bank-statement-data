// Command service is the binary of the service: "serve" runs the gRPC server with its
// background jobs (FIN-117), "migrate" applies database migrations before a rollout
// (FIN-115). Configuration comes from the environment (docs/config.md).
//
// Rename the directory to the service name (cmd/user, cmd/bank-bff, ...) in a new service.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time (-X main.version=...).
var version = "dev"

func main() { os.Exit(mainExit(os.Args[1:], os.Stderr)) }

// mainExit owns process setup and the command exit status. Returning before os.Exit
// also releases the signal subscription on unsuccessful invocations.
func mainExit(args []string, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, args); err != nil {
		var usage usageError
		if errors.As(err, &usage) {
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
		logger.Error("failed", "error", err)
		return 1
	}
	return 0
}

type usageError string

func (e usageError) Error() string { return string(e) }

const usage = `usage:
  service serve                            run the gRPC server, /metrics and background jobs
  service migrate up                       apply pending migrations of all sources
  service migrate version                  print applied versions
  service migrate down <source> <steps>    roll back the last steps of one source
  service migrate force <source> <version> set the version and clear the dirty flag`

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(usage)
	}
	switch args[0] {
	case "serve":
		if len(args) != 1 {
			return usageError(usage)
		}
		return runServe(ctx)
	case "migrate":
		return runMigrate(ctx, args[1:], os.Stdout)
	default:
		return usageError(usage)
	}
}
