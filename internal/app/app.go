// Package app assembles the service: identity, database pools, the gRPC server with its
// handlers, clients of other services and background jobs. cmd/bank-statement-data runs it with real
// listeners; apitest runs the same assembly in-process for API tests, so what the tests
// call is what production serves.
package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	identitylib "github.com/fin-assist/go-identity"
	tombstone "github.com/fin-assist/go-tombstone"

	"github.com/fin-assist/go-grpc/client"
	"github.com/fin-assist/go-grpc/server"
	"github.com/fin-assist/go-worker"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

// App is the assembled service. Wire reads its fields to build adapters and usecases.
type App struct {
	Cfg     config.Config
	Log     *slog.Logger
	DB      *postgres.DB
	GRPC    *server.Server
	Clients *client.Metrics
	Tokens  client.TokenSource
	Jobs    []worker.Job

	closers []func()
}

// New assembles the service from cfg. Close releases what it opened.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, reg prometheus.Registerer) (*App, error) {
	return assemble(ctx, cfg, log, reg, postgres.Open)
}

func assemble(ctx context.Context, cfg config.Config, log *slog.Logger, reg prometheus.Registerer, openDB func(context.Context, postgres.Options) (*postgres.DB, error)) (*App, error) {
	// Client metrics come first: the identity connection to user reports into them too.
	clients, err := client.NewMetrics(reg)
	if err != nil {
		return nil, err
	}
	id, err := newIdentity(cfg, reg, clients, time.Now)
	if err != nil {
		return nil, err
	}
	db, err := openDB(ctx, postgres.Options{
		PrimaryDSN: cfg.DB.PrimaryDSN, ReplicaDSN: cfg.DB.ReplicaDSN,
		MaxConns: cfg.DB.MaxConns, AppName: cfg.Service,
	})
	if err != nil {
		id.close()
		return nil, err
	}
	a := &App{Cfg: cfg, Log: log, DB: db, Clients: clients, Tokens: id.tokens, Jobs: id.jobs, closers: []func(){id.close, db.Close}}
	fail := func(err error) (*App, error) { a.Close(); return nil, err }

	admission, err := newTombstoneAdmission(db, reg)
	if err != nil {
		return fail(err)
	}
	if a.GRPC, err = server.New(server.Options{MaxRecvMsgSize: cfg.GRPC.MaxRecvMsgSize, Reflection: cfg.GRPC.Reflection}, server.Deps{
		Logger: log, Metrics: reg, Policy: AccessPolicy(),
		Services: id.services, Users: id.users, Tombstones: admission,
	}); err != nil {
		return fail(err)
	}
	jobs, closeWired, err := wire(a)
	if err != nil {
		return fail(err)
	}
	a.Jobs = append(a.Jobs, jobs...)
	a.closers = append(a.closers, closeWired)
	return a, nil
}

// Close releases what New opened, in reverse order. The identity connection is closed
// last: call Close after the jobs (identity-keys among them) have returned.
func (a *App) Close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
	a.closers = nil
}

// identityParts are the verifiers of incoming calls, the token of outgoing ones, the
// background jobs that keep their keys fresh and the release of what they opened.
type identityParts struct {
	services server.ServiceVerifier
	users    server.UserVerifier
	tokens   client.TokenSource
	jobs     []worker.Job
	close    func()
}

// userKeysRetry is the only method the identity connection to user retries: a read.
const userKeysRetry = "finassist.user.v1.UserInternalService/GetSigningKeys"

// newIdentity builds the identity of AUTH_MODE: go-identity NewAssembly for verify
// (Kubernetes ServiceAccounts in prod, file-key stand JWTs in stage, user JWTs by the
// keys of user.GetSigningKeys in both) or the dev verifiers of go-grpc for insecure-dev.
// The user connection reports into clients (grpc_client_*{peer="user"}). A refusal opens
// nothing and registers no identity metric; there is no weaker fallback.
func newIdentity(cfg config.Config, reg prometheus.Registerer, clients *client.Metrics, now func() time.Time) (identityParts, error) {
	switch cfg.Auth.Mode {
	case config.AuthInsecureDev:
		// New is exported and callable without config.Load, so the dev-only gate is
		// enforced here as well: unverified identities never outside ENV=dev.
		if cfg.Env != config.EnvDev {
			return identityParts{}, errors.New("AUTH_MODE=insecure-dev is allowed only with ENV=dev")
		}
		return identityParts{services: server.DevServiceVerifier{}, users: server.DevUserVerifier{},
			tokens: client.DevToken(cfg.Service), close: func() {}}, nil
	case config.AuthVerify:
	default:
		return identityParts{}, errors.New("unsupported AUTH_MODE")
	}
	if cfg.Peers["user"] == "" {
		return identityParts{}, errors.New("identity: PEER_USER_ADDR is required with AUTH_MODE=verify")
	}
	id := cfg.Auth.Identity
	options := identitylib.AssemblyOptions{
		Env: string(cfg.Env), Service: cfg.Service,
		UserIssuer: id.UserIssuer, UserAudience: id.UserAudience,
		DialUser: func(tokens identitylib.TokenSource) (*grpc.ClientConn, error) {
			return client.Dial(cfg.Peers["user"], client.Options{Peer: "user", Tokens: tokens, Metrics: clients, Retry: []string{userKeysRetry}})
		},
		Clock: now, Registerer: reg,
	}
	// Both sets go to the library: it uses the one of ENV and refuses stand keys in prod.
	options.Kubernetes = identitylib.KubernetesOptions{
		TokenFile: cfg.Auth.ServiceTokenFile, Issuer: id.Kubernetes.Issuer, JWKSURL: id.Kubernetes.JWKSURL,
		CAFile: id.Kubernetes.CAFile, APITokenFile: id.Kubernetes.APITokenFile, Namespace: id.Kubernetes.Namespace,
	}
	options.Stage = identitylib.StageKeyFiles{
		Issuer: id.Stage.Issuer, SigningKeyFile: id.Stage.SigningKeyFile, PublicKeysFile: id.Stage.PublicKeysFile,
	}
	// NewAssembly refuses every other ENV, dev included.
	assembly, err := identitylib.NewAssembly(options)
	if err != nil {
		return identityParts{}, err
	}
	return identityParts{
		services: assembly, users: assembly, tokens: assembly,
		jobs:  []worker.Job{{Name: "identity-keys", Run: assembly.Run}},
		close: func() { _ = assembly.Close() },
	}, nil
}

// deletedUsers is the part of the go-tombstone store the admission check needs.
type deletedUsers interface {
	Deleted(ctx context.Context, userID string) (bool, error)
}

// tombstoneAdmission answers go-grpc admission from the primary-backed tombstone store
// (go-tombstone README, «Сборка с go-grpc»): a user call of a deleted user gets
// TOKEN_REVOKED from the server, a read failure a bare UNAVAILABLE. No answer is cached:
// a negative cache would hide a deletion made by another pod. The service has no
// AllowDeleted method, so every positive answer is a refusal and is counted.
type tombstoneAdmission struct {
	store   deletedUsers
	revoked prometheus.Counter
}

// IsDeleted reports whether userID was deleted.
func (a tombstoneAdmission) IsDeleted(ctx context.Context, userID string) (bool, error) {
	deleted, err := a.store.Deleted(ctx, userID)
	if err == nil && deleted {
		a.revoked.Inc()
	}
	return deleted, err
}

func newTombstoneAdmission(db *postgres.DB, reg prometheus.Registerer) (tombstoneAdmission, error) {
	store, err := tombstone.New(db, db.Primary(), time.Now)
	if err != nil {
		return tombstoneAdmission{}, err
	}
	revoked, err := tombstone.NewRevokedCounter(reg)
	if err != nil {
		return tombstoneAdmission{}, err
	}
	return tombstoneAdmission{store: store, revoked: revoked}, nil
}
