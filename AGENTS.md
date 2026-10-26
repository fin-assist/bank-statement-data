# Instructions for code agents

`bank-statement-data` — the Go service of «Понятные финансы» (B2C personal finance) that owns statement imports (OFX
from T-Bank), accounts, operations and their source records, categories, regular payments and «не подписка» marks,
and computes the personal metrics it sends to `analytics` (grpc-api.md 7.2). Made from `fin-assist/go-service-template`
(FIN-190). Read by Claude Code (through `CLAUDE.md`) and by Codex. This file is the orientation map: read it and
`README.md` first, then open only what the task touches.

## Keep this file current

A PR that changes what this file, `README.md` or `docs/` say (layout, commands, CI checks, conventions, process)
updates them in the same PR. A service made from the template rewrites the first paragraph for itself.
- Blocks between `<!-- generated:… -->` markers are produced from code; do not edit them by hand. The tables of
  `docs/config.md` come from `Vars` in `internal/config/vars.go`: a new variable or default is a line there, then
  `go test ./internal/config -run TestUnitDocIsCurrent -update`. `go test` fails on a stale table, on a variable read
  but not declared, and on one declared but never read.

## Where things are decided

- Contracts: `fin-assist/contracts` — `docs/api.md` (REST), `docs/grpc-api.md` (services, call models, metadata,
  errors, idempotency, outbox, transport), `docs/calc.md` (calculations), `proto/`. The service imports the generated
  module `github.com/fin-assist/contracts/gen/go` (messages, gRPC, `finerr`) at a tag `gen/go/vX.Y.Z`. A service
  never defines its own `.proto`, error codes or REST fields: a change goes to `contracts` first (its own FIN issue
  and PR), then the service bumps the module.
- Shared code: Go libraries made from `fin-assist/go-library-template` (FIN-118) — `go-identity`, `go-idempotency`,
  `go-outbox`, `go-tombstone`, `go-postgres`, `go-grpc`, `go-migrate`, `go-worker`. The service imports tagged versions and wires them in `internal/app`
  by the «Внедрение» section of their README; versions and compatibility — `docs/releasing.md` of the library template.
  Code that more than one service needs goes into a library (its own FIN issue), not into a copy here.
- Architecture, MVP scope, product decisions: Confluence PF (https://finassist.atlassian.net/wiki/spaces/PF),
  «Архитектура» (including «Миграции и доступ к БД» and «Репозитории и структура кода»). When the docs here and the
  contracts differ, the contracts and the owner's latest decision win; say so in the PR.
- Every change goes through a YouTrack issue (`FIN-<n>`, finassist.youtrack.cloud) referenced in the PR.
- Mismatches between the contracts, Confluence and this repository are expected. Resolve them in favour of the
  contracts and the owner's latest decision, say so in the PR, and propose (not impose) engineering changes; keep them
  non-radical. The owner writes backend code himself and is strong in PostgreSQL: schemas and queries are proposed
  with arguments, not decided for him.

## Repository map

| What | Where |
|---|---|
| Commands `serve`, `migrate`: listeners, shutdown | `cmd/bank-statement-data/` (`serve.go`, `migrate.go`) |
| Assembly: identity, pools, server; wiring of adapters, usecases and handlers (`wire()`), the access table (`AccessPolicy()`), migration sources | `internal/app/` (`app.go`, `wire.go`) |
| The whole service in-process for API tests | `internal/app/apitest/` |
| Entities and invariants (no pgx, gRPC or generated code) | `internal/domain/` |
| Usecases and the interfaces of repositories and clients they need | `internal/<feature>/` |
| Shared gRPC server, client and request context | `github.com/fin-assist/go-grpc/{server,client,reqctx}` v0.2.1; service wiring — `internal/app/`, `docs/server.md` |
| gRPC handlers (messages ↔ usecases) | `internal/adapter/grpc/<feature>/` |
| Primary/replica routing, go-postgres.OpenPools/transaction adapter, repositories | `internal/adapter/postgres/`; rules — `docs/migrations.md`, «Запросы на pgx» |
| Migration runner and public test harness | `github.com/fin-assist/go-migrate` / `migratetest` v0.2.0; ordered service sources — `internal/app/` |
| Test PostgreSQL server selection (not runtime business code) | `internal/testdb/`; public migratetest creates/checks databases; counted in every profile |
| Stand identity for verify-mode tests: key files, signed tokens, user GetSigningKeys peer | `internal/testidentity/`; never wired into serve; counted in every profile |
| SQL migrations (embedded) and schema constraint tests | `migrations/` (`schema_test.go`); rules — `docs/migrations.md`, schema and code invariants — `docs/schema.md` |
| Application-specific peer adapters, when needed | `internal/adapter/client/<peer>/`; shared dialing/token contracts — go-grpc/client |
| Shared job lifecycle; application jobs when needed | `github.com/fin-assist/go-worker` v0.1.1; `internal/worker/<job>/` |
| Configuration: every variable with its default and description | `internal/config/vars.go` (`Vars`); generated tables — `docs/config.md` |
| Request-scoped values (request ID, caller, user, bank-bff hints) | `github.com/fin-assist/go-grpc/reqctx` |
| Logger, metrics endpoint, readiness | `internal/logx/`, `internal/ops/` |
| Image, local run, stand | `Dockerfile`, `compose.yaml`, `Makefile`, `deploy/systemd/` |
| CI, lint | `.github/workflows/ci.yml`, `.golangci.yml`, `.squawk.toml` |
| Service versions, release notes and repeatable GitHub Release | `CHANGELOG.md`, `docs/releasing.md`, `.github/workflows/release.yml`, `scripts/changelog.py` |
| Dependency updates | `renovate.json` |
| PR review by Claude | `.github/workflows/claude.yml` |

FIN-258 keeps only service routing/assembly adapters locally. Do not copy library internals back into the service
or add dummy RPCs, SQL or jobs. FIN-267: AUTH_MODE=verify is go-identity v0.2.1 NewAssembly (prod: Kubernetes
ServiceAccounts, stage: file-key stand JWTs; user JWTs by user.GetSigningKeys through PEER_USER_ADDR, refreshed by
the identity-keys job); any other ENV with verify fails startup, insecure-dev stays dev-only in config and app.New.
go-tombstone v0.1.0 supplies the first migration source and the primary-backed go-grpc admission (TOKEN_REVOKED).
FIN-342 wires guarded import writes, UserDeletionService.PurgeUserData and settings sync.
Import usecases/rules/results live in internal/importing; adapters in adapter/{ofx,objectstore,statementwire,grpc/statements,postgres}.
Read docs/import.md before changing staged admission, permanent erasure fences or two-phase purge.
Object barrier uses a separately owned pool; remote I/O is forbidden inside data transactions.
The dedicated bucket must never enable versioning/Object Lock/marker expiration; CheckBucket precedes listeners.
Shared TLS object fixtures live in internal/testobjects and remain counted in every profile.
Pass the service logger, metric registry and worker clock/restart settings explicitly; library defaults
are not a substitute for existing service behaviour. go-postgres is pinned at v0.3.0. Paths with <feature>, <peer>
and <job> describe future application code only, not actual empty directories in the template.

The service test entrypoint selects a PostgreSQL18 admin DSN (external server or testcontainer) in internal/testdb.
Public migratetest.DSN(t, adminDSN) creates and cleans its own isolated database;
migratetest.CheckUpDownUp(t, dsn, source) uses the supplied database without owning/dropping it.
Keep cleanup and cancellation bounded. internal/testdb and internal/app/apitest ordinary .go files remain counted
in all three service profiles; there is no service pgtest exclusion. See docs/testing.md.

### Stack

Versions are in `go.mod` (Go toolchain included), `.github/workflows/ci.yml` and `Makefile` (golangci-lint), `compose.yaml`
and `internal/testdb.DefaultImage` (PostgreSQL). Main pieces: grpc-go, protobuf, protovalidate, pgx v5, golang-migrate,
prometheus client_golang, testcontainers-go, squawk; the fin-assist libraries above. A new dependency is named in
the PR with why it is needed.

## Build in an agent container

- Go: the toolchain from `go.mod` is downloaded by `GOTOOLCHAIN=auto` (the default) on the first `go` command.
- `fin-assist/*` modules come from git, not the proxy: `GOPRIVATE=github.com/fin-assist/*` (set by `Makefile`; export
  it for plain `go` commands).
- Docker is usually absent in an agent container, so testcontainers cannot start PostgreSQL. Run a local server and
  point the tests at it: `TEST_DATABASE_URL='postgres://postgres@localhost:<port>/postgres?sslmode=disable' make test`.
  Without either, integration tests are skipped locally and fail in CI (`CI` set) — a skip is not a pass.
- `make lint` runs the pinned golangci-lint through `go run`; no separate install.
- `make cover` (or `go test -coverpkg=./... -coverprofile=cover.out ./...`) needs the `covdata` tool, which a
  toolchain downloaded by `GOTOOLCHAIN` lacks ("no such tool covdata"): build it once with
  `T=$(go env GOTOOLDIR); chmod u+w "$T"; go build -o "$T/covdata" cmd/covdata`.
- The image needs Docker; without it say in the PR that the image was built only by the CI job `image`.
- Every test or lint run gets a timeout (`timeout 600 make test` or the tool's own): a hung run blocks the session.
- A local `serve` smoke check additionally requires `OBJECT_STORAGE_BUCKET`, `OBJECT_STORAGE_KMS_KEY_ID` and
  `OBJECT_STORAGE_IAM_TOKEN_FILE`, a reachable private Yandex bucket and IAM permissions for `CheckBucket`.
  With those configured: `ENV=dev AUTH_MODE=insecure-dev SERVICE_NAME=bank-statement-data DB_PRIMARY_DSN=… make run`.
  There is no production dev-memory/local-file object store; CheckBucket runs before listeners. For isolated validation
  use the real-PostgreSQL/TLS API fixtures, not a claimed local serve without cloud prerequisites. `/readyz` on `HTTP_ADDR` answers when the database does and drops to 503 at SIGTERM, before `SHUTDOWN_DELAY` (FIN-289).

## Principles

Priorities when they pull apart, in this order:

1. **Test coverage** — a change without its tests (Tests below) is not done, however good the code.
2. **Clarity** — code a reader understands without the author: plain control flow, explicit names, no cleverness.
3. **Security** — the Security rules of the review are requirements, not trade-offs; this place in the order decides
   only between options that all meet them.
4. **Performance** — optimise when a measurement shows the need, not in advance; a faster but less clear version
   comes with the benchmark that justifies it.

- **No frameworks.** No web or RPC frameworks (gin, echo, fiber, go-kit), no ORM (gorm, ent), no DI containers or
  generators (fx, wire, dig), no config frameworks (viper): the standard library, grpc-go and explicit wiring in
  `internal/app`. `depguard` in `.golangci.yml` rejects the common ones.
- **Standard library first.** Use `net/http`, `log/slog`, `encoding/json`, `errors`, `testing`, `context`,
  `database/sql` interfaces and the rest of the standard library wherever they do the job; tests use `testing` (no
  testify, gomock). A third-party package only where the standard library has no equivalent — the PostgreSQL driver
  (pgx), gRPC and protobuf, protovalidate, golang-migrate, the Prometheus client, testcontainers — and a new one is
  named in the PR with why the standard library is not enough.
- **SOLID, the Go way:**
  - single responsibility — a package and a type have one reason to change: a usecase does not format messages, a
    handler does not hold business rules, a repository does not decide;
  - open/closed — new behaviour is a new type behind an existing interface (a repository, a client, a job), not a
    flag or a type switch in old code;
  - Liskov — every implementation of an interface keeps its contract, fakes in tests included: the same errors for
    the same cases, no panics where the real one returns an error;
  - interface segregation — small interfaces declared by the consumer with only the methods it calls;
  - dependency inversion — usecases depend on those interfaces, never on adapters; `wire()` passes the
    implementations in.

## Writing code

- Code, comments, identifiers, commit messages — English. `docs/*.md`, `README.md`, PR title and description —
  Russian. Comments where the logic is not obvious; a doc comment on every exported identifier.
  Exception: `COMMENT ON TABLE` and `COMMENT ON COLUMN` in migrations are Russian, one for every table and
  column (owner's request, FIN-111; `docs/migrations.md`, «Что можно в схеме»).
- Layers: `domain` ← `<feature>` ← adapters ← `cmd`. A usecase declares the interface it needs (consumer side) and
  never imports an adapter, pgx or gRPC; a handler converts messages and calls a usecase; `wire()` connects them.
- Errors to callers: only `finerr` constructors (`finerr.NotFound()`, `finerr.LimitExceeded(resetsAt)`).
  `status.Error`, `status.New`, `errdetails.ErrorInfo` are forbidden by the linter; an internal refusal is a bare
  code through `bare()` in the server package. A new client reaction needs a new code in `contracts` first. A
  handler may return a `finerr` error wrapped with `%w` or any other error: the interceptor sends the caller the
  unwrapped status or a bare `INTERNAL` (`CANCELLED` / `DEADLINE_EXCEEDED` for context errors) and logs the
  original, so no text of a wrapped error leaves the process (the tagged go-grpc/server contract linked from `docs/server.md`).
- Identity: the user of a user call is `reqctx.UserID(ctx)` (the verified `sub`), never a request field; a system
  call takes `user_id` from the request. Every new method gets its row in `AccessPolicy()`, copied from
  grpc-api.md 3.3; the server does not start without it.
- **SQL construction:** write fixed-structure queries as plain SQL with parameters (`$1`, `$2`, … for
  PostgreSQL). Use [Squirrel](https://github.com/Masterminds/squirrel) (`github.com/Masterminds/squirrel`) only
  when assembling a query dynamically, such as optional predicates or varying column lists. Changing argument
  values alone does not make a query dynamic. This limited use is an approved exception to "standard library
  first". Parameterize values; keep identifiers and structural fragments constant or from an explicit allowlist.
  Use `PlaceholderFormat(sq.Dollar)` for PostgreSQL builders. If a dynamic query cannot be expressed by the
  builder, explain the limitation beside its manual construction and preserve parameterization.
- Database: SQL with parameters only (`$1`), table and column names are constants. A usecase opens the transaction
  with `db.Run` (delegated to `go-postgres`); repositories take `db.Writer(ctx)` / `db.Reader(ctx)`;
  no calls to other services inside `Run`.
  Migrations follow `docs/migrations.md` (two releases for an incompatible change, `CONCURRENTLY` alone in a file).
- Constraint errors (FIN-195): go-postgres owns the transaction mechanism. Adapters define one
  operation-specific mapper on the outer RunMapped, matching both SQLSTATE and constraint name;
  usecases see only domain errors and translate them with finerr per the method contract. Never
  install a global mapping or parse pgconn in a usecase. Deferred errors are mapped at actual COMMIT;
  nested work uses Run, not another mapper. Unknown violations remain INTERNAL. Never copy PgError.Detail
  or wrap the raw PgError into a domain error. See docs/migrations.md and internal/adapter/postgres/transactions_test.go.
- No validation or logic in the schema (FIN-194, `docs/migrations.md` «Что можно в схеме»): allowed are `PRIMARY KEY`,
  `UNIQUE`, `FOREIGN KEY` with any `ON DELETE` / `ON UPDATE` action (`SET DEFAULT` included), `NOT NULL`, `DEFAULT`,
  indexes (unique, partial and expression ones included) and generated columns `GENERATED ALWAYS AS (…) STORED` (a
  value computed for storage or search, not validation); an identity column (`GENERATED ALWAYS AS IDENTITY`) is a
  separate case, a key, and is allowed too. Not allowed: `CHECK`, `EXCLUDE`, triggers, stored functions or procedures,
  domains with constraints. Such invariants live in `internal/domain` or the usecase with unit tests; actions on a
  write (`pg_notify` included) run in the service's own `db.Run`. DDL inside a migration (a `DO` block creating the
  initial set of partitions, `RAISE EXCEPTION` in an irreversible down file) runs once on `migrate up` /
  `migrate down`, is not runtime logic and is allowed. Future partitions are created by the service's job, never
  `pg_partman` (`docs/migrations.md`, «Партиции»).
  A cross-row invariant (a former `EXCLUDE`, «one active row», non-overlapping intervals) is expressed by a unique
  index where possible (a partial `UNIQUE`, for example); otherwise code checks it under a lock in the same `db.Run`
  — `SELECT … FOR UPDATE` of the parent row or a transaction-level advisory lock, `pg_advisory_xact_lock(key)`
  (released on `COMMIT` / `ROLLBACK`; a session `pg_advisory_lock` would stay on the pooled connection) — and an
  integration test with two concurrent writers on PostgreSQL covers it.
  Every such rule of this service has its row in «Инварианты в коде сервиса» of `docs/schema.md`; a new rule adds
  its row there.
- Clients of other services: `client.Dial` with `Retry` listing only reads and methods idempotent by construction;
  the context carries the deadline.
- Time: "now" in business logic comes from an injected clock (`func() time.Time` or an interface passed by `wire()`),
  never `time.Now()` inside a usecase or repository query: tests pin the date, and month and year boundaries must be
  testable. Calendar days and months are computed in the user's profile time zone (grpc-api.md 8), never in
  `time.Local` or the server's zone; `now()` in SQL only for technical columns (`created_at`, outbox schedule).
  Durations for metrics and timeouts may use `time.Now()` / `time.Since`.
- Money: integer minor units (`int64`, `_minor`), never floats; the sign follows the contract.
- Tolerance of what the service does not know yet: an enum value of another service it does not know (newer
  producer) follows the rule of that field in grpc-api.md 8 — hide the field or fail the call with an alert — never
  a panic or a silent default that changes meaning; an unknown field is ignored (protobuf does it). A new enum
  value or error metadata key follows the rollout order of grpc-api.md 5.1 and 8.
- Dev-only hooks — `AUTH_MODE=insecure-dev`, dev verifiers and tokens, reflection, test helpers — stay unreachable
  outside `ENV=dev`: config rejects them, and code must not add another way around it.
- Logs: `slog` `*Context` methods; no tokens, DSNs, statement contents or assistant dialogue in logs or errors.
  The debug log of a request masks `token` fields and clears every `bytes` field; other personal data in a
  request is logged at `debug` as is. Metric labels: bounded values only — never `user_id`, an operation ID or
  free text.
- Document in moderation: a doc comment on exported identifiers of packages others use, comments where the logic is
  not obvious (accounting rules, idempotency, locks, transaction boundaries), not on every function.

## Tests

Every change comes with its automated tests in the same PR — not in a follow-up. Three levels, each mandatory:

- **Unit tests — all new code.** Every new or changed function or method with logic (domain rules, usecases,
  mappers, validation, config, helpers, interceptors) has unit tests in its package: no database, no network, fakes
  of the interfaces the code declares; the main path and every branch and error the code handles.
- **Integration tests — all new functionality.** Every new or changed behaviour is tested against its real
  dependencies: repositories, migrations, transactions and locks against PostgreSQL (service `internal/testdb.DSN`, shared `go-migrate/migratetest`), not mocks of
  pgx — the SQL itself is what is tested; clients of other services against a fake gRPC server on bufconn
  (retries, deadlines, metadata); jobs and the outbox through a real run. Every migration passes up → down → up
  (`migrations/migrations_test.go`).
- **API tests — all new functionality.** Every new or changed RPC is tested through the service's API as a caller
  sees it: `apitest.Start(t)` runs the whole assembly (`internal/app`: interceptors, access table, handlers,
  PostgreSQL with migrations) and the test calls it with the generated client as `bank-bff` (`apitest.User`) or
  another service (`apitest.Caller`). Assert the status code, `ErrorInfo.reason` (`apitest.Reason`) and metadata, and
  the response fields against the contract — not the message text. Cover the main path, every error the contract
  lists for the method, a foreign caller, another user's object and a deleted user.
- **API tests in `bank-bff` — every REST endpoint.** `bank-bff` is covered by API tests too, at its own API — the
  REST of `openapi.yaml` the mobile client calls: a real HTTP request to the `bank-bff` handler (`httptest`), the
  services behind it played by fake gRPC servers on bufconn that answer with success and with each registry error.
  Every response is validated against `openapi.yaml` (kin-openapi), and the test asserts the HTTP status,
  `error.code`, `details[]`, headers (`Retry-After`, `Idempotency-Key` handling) and SSE events — the main path, every
  error `docs/api.md` lists for the endpoint (including the translation of each `reason` by `finerr.ToREST` and an
  unknown one into 500), a missing or bad token, a missing `Idempotency-Key`, a body that fails the schema, an
  unsupported client version. Every operation of `openapi.yaml` that `bank-bff` serves has at least one API test.

Also:

- A bug fix comes with a test that fails without the fix and reproduces the reported case, at the lowest level that
  shows it, plus an API test when the caller saw the bug.
- Security-relevant behaviour (foreign caller, another user's object, deleted user, consent gates, hostile input)
  has negative tests: what must not happen is asserted — the handler did not run, no row was written, nothing leaked.
- Tests assert behaviour; a test that only executes lines or asserts on a fake it set up itself is not coverage.
  Run with `-race`; no sleeps for synchronisation — wait on a channel or a condition with a deadline.
- The measurable gate is at least 80% of all production-code statements for each of unit, integration and API
  separately (`make cover`, README and `docs/testing.md`). Combined coverage cannot compensate for a failing suite.
  Name every test `TestUnit…`, `TestIntegration…` or `TestAPI…` and every fuzz target `FuzzUnit…`,
  `FuzzIntegration…` or `FuzzAPI…` (its seed corpus runs in that suite); classify it by its actual dependencies and
  assembly. `make test-names` and CI reject any other `Test`/`Fuzz` name and any `Example…` (a service has no public
  Go API, and an example cannot carry a suite prefix) using native `go test -race -list`; every collect target
  (`-run "^(Test|Fuzz)<Suite>"`) requires this classification first.

## Checks

CI (table in `README.md`): `go mod tidy` clean, `go vet`, `go test -race` with PostgreSQL 18 in Docker and three independent statements coverage gates,
golangci-lint, image build, and on PRs the migration checks (existing files unchanged, squawk on new ones). Run
`make test` (or `make cover`) and `make lint` before pushing.

`make test` and CI also run the Python script tests and check CHANGELOG format. Service releases follow
`docs/releasing.md`: prepare a dated version section in a reviewed PR, wait for post-merge CI, then the owner
pushes the tag. Agents never create, move or delete version tags. Release publishes GitHub metadata only;
image publication and deployment remain separate. Existing releases are preserved after validating the tag.

### Coverage reports (FIN-238)

Use native Go profiles and pinned go-test-coverage v2.20.1. All three suites use `-race -count=1 -timeout=8m`
and `-coverpkg=./...` with the same whole-module denominator and no production-file exclusions. Generated code and
non-test `apitest`/`testdb` helpers remain included. Each suite must execute tests with no failures/skipped tests;
missing, empty, header-only and zero-statement profiles fail before the percentage tool runs. PostgreSQL is mandatory
(`CI=true`). The tool merges profiles for the supplemental combined result and rounds to one decimal before gating.
No custom percentage parser or changed-line calculation. CI reports unit/integration/api/combined and uploads the
profiles, execution evidence and native tool reports. Fork PRs cannot post comments. See `docs/testing.md` for the
honest baseline and the absent business API; configuring gates does not satisfy the unfinished service criteria.

## Workflow

- Branches: `<kind>/<slug>` — `feature/…`, `fix/…`, `tooling/…`; a PR targets `main`. Commits: imperative English
  subject (`Add …`, `Fix …`, `Address review: …`).
- Dependency updates come from Renovate (`renovate.json`; README «Библиотеки и обновления»): its `renovate/…`
  branches are the exception to the branch names and need no YouTrack issue while they change nothing but
  dependencies. FIN-211 permits merge/retry/close of allowed bot-only updates, including v0 minor/major changes,
  after required CI; deployment and rollout instructions in CHANGELOG/contracts remain the owner's responsibility.
  Human fixes make the PR manual and require a YouTrack issue. After automatic closure, create a separate fix PR
  with an issue rather than pushing to the closed Renovate branch; follow README dependency policy.
- PR title and description in Russian: what changed and why, deviations from the contracts or docs, how it was
  checked. A PR for a single issue starts its title with the key (`FIN-42 Поиск операций по сумме`); a PR for several
  issues lists all of them in the description, each linked (`https://finassist.youtrack.cloud/issue/FIN-<n>`).
- A dependency on a contract change: the contract PR is merged and `gen/go` tagged first, then the service PR bumps
  the module. Order of rollout for new enum values and error metadata — grpc-api.md 5.1 and 8.
- After the PR is open and all planned changes are pushed, request a review with one PR comment in Russian addressed
  to both reviewers; it starts with `@codex @claude`:

  ```
  @codex @claude проведите ревью этого PR по Code Review Rules в AGENTS.md, включая разделы Security и Tests.
  Замечания — на русском, inline на изменённых строках. Не придирайтесь к формулировкам документации и комментариев:
  замечание по тексту только если он искажает поведение кода. Особое внимание: <что именно и почему>.
  ```

  - `@claude` — `.github/workflows/claude.yml` (runs from the default branch), Claude Opus 5.5
    (`claude-opus-5-5`, `--effort high`; FIN-204). Needs the Claude GitHub App and the `CLAUDE_CODE_OAUTH_TOKEN` secret.
  - `@codex` — the Codex GitHub integration under the owner's ChatGPT subscription. Its model is chosen by OpenAI
    and cannot be pinned from here; a pinned model would need the OpenAI API with per-token billing, which was
    declined (FIN-103).
  - Request Claude only once `.github/workflows/claude.yml` is on `main`: comment-triggered workflows run
    from the default branch. This does not restrict Codex: request it separately when its integration is available.
  - Replies in threads and other comments never mention `@codex` or `@claude`: any mention with text starts them
    again.
- Waiting for the review: react to the PR's GitHub events when the session receives them; otherwise check 3 minutes
  after the request, then every 2 minutes. A reviewer has answered when it posted its review or summary for the
  latest commit; for Codex (`chatgpt-codex-connector[bot]`) «Didn't find any major issues» or a 👍 without a comment
  also counts. A reviewer silent for 15 minutes after the last request: say so in the PR and to the owner, stop
  waiting for it. The cycle ends when both answered the latest request and no thread is open.
- Address findings in follow-up commits on the same branch, reply in the thread and resolve it, then request a
  review again. Agents never merge a PR; the owner does.

## Code Review Rules

Read by `@claude` (through `CLAUDE.md`) and by the Codex integration, which looks for this heading. Two areas get a
dedicated pass on every PR: Security and Tests.

### General

- Write all review comments in Russian (summary and inline). Identifiers and quotes stay as they are.
- Report only verified problems with a concrete failure scenario: which caller, request or data, what goes wrong.
  Skip style nits, naming preferences and wording of comments and docs unless the text misstates the behaviour.
- Weigh findings by the priorities of «Principles»: missing tests first, then clarity, then security beyond the
  mandatory rules, then performance; a performance finding needs a measurement or an evident cost (an N+1, an
  unbounded load), not a guess.
- Post each finding as an inline comment on the changed line. The summary lists what was checked, with a
  «Безопасность» part and a «Тесты» part; «not affected» is a valid answer, with one line why.
- Check that the PR references its YouTrack issue and updates `README.md`, this file and `docs/` when it changes
  what they describe.

### Security

The service handles a user's bank operations, phone number, statement files and assistant dialogue (personal data
under 152-ФЗ). A finding names what leaks or breaks and how a client, another user or a compromised service gets to
it. Check:

- Object access: every query by an object ID is scoped by the user (`WHERE user_id = $n` with the user from
  `reqctx.UserID` or the system call's `user_id`); another user's object gives `NOT_FOUND`, not its data or a
  different error that confirms it exists (IDOR / BOLA).
- Identity: the user never comes from a request field in a user call; a new method has its row in `AccessPolicy()`
  matching grpc-api.md 3.3 (callers and model); hints `x-client-ip`, `x-client-version`, `x-read-primary` are used
  only as the server package sets them; `x-client-version` never decides access or limits.
- Errors: client-facing errors only through `finerr`; `status.message`, `error` details and logs contain no SQL,
  stack traces, tokens, DSNs or other users' data; an internal refusal is a bare code.
- Secrets: tokens and DSNs only in headers, files or `*_FILE` variables, never in logs, metric labels, URLs or error
  texts; the masking list in `docs/server.md` covers new secret metadata or fields.
- SQL: parameters only; nothing from a request selects a table, column, `ORDER BY` fragment or file path.
- Untrusted input: bounds come from protovalidate annotations in `contracts`; a handler that reads a size, count or
  page relies on them or checks itself; long-running or costly work (file parsing, LLM, calls, SMS) has a limit
  and a deadline.
- Transactions: an effect and its idempotency record or outbox row are written in one `db.Run`; no external call
  inside `Run`; a deleted user's data is not written back (tombstone).
- Clients: no user token forwarded to another service; retries only for reads and idempotent methods.
- Consent: a service that sends user data to the LLM checks the assistant consent first; an account is created only
  after the personal data consent; revoking a consent or deleting the account removes data as the contract says.
- Dev and test hooks (`insecure-dev`, dev verifiers, reflection, fixtures) cannot be enabled in `stage` or `prod`.
- Statement files and other uploads: size, element count and nesting depth are bounded; OFX 2.x (XML) is parsed with
  DTD and external entities disabled; a hostile file ends in a handled error, not a panic, a hang or memory growth.
- Trust boundaries: limits, quotas and permissions are enforced here, never left to the client or to `bank-bff`
  alone; `x-client-version` never decides access.
- Secrets in the repository: no real keys, tokens or DSNs with passwords (compose uses throwaway local ones).
- New dependencies: flag a new library that gets network, storage or data access, so the owner decides on it.

### Tests

The criterion is whether the use cases of the changed functionality are covered; the independent 80% statements
gates in Tests are minimums, and meeting them does not settle this check. For
each behaviour the PR adds or changes:

1. List its use cases from the code, the contract and the issue: the main path; alternative paths; every error a
   caller can reach (validation, not found, another user's object, consent missing, limit exceeded, a peer
   `UNAVAILABLE`, a database error, a cancelled context or an expired deadline); boundary values (zero, negative and
   very large amounts, dates at day, month and year boundaries, time zones, empty and maximal lists, long texts);
   repeated and concurrent calls (an idempotent retry with the same key, two writers at once); a deleted user.
2. Map each case to the test that covers it and check all three levels of Tests above: new code without a unit test,
   new functionality without an integration test or without an API test is a finding.
3. Post a finding for an uncovered case only when it is reachable in the changed code and its behaviour is added or
   changed by the PR; name the concrete case and the layer that should cover it. The list in step 1 is a prompt, not
   a checklist to report item by item; a case not worth a test is fine when the PR says why.

Also check that a test would fail if its case broke, that a bug fix's test reproduces the reported case, that every
migration passes up → down → up (an irreversible step marked as `docs/migrations.md` says), and that
security-relevant behaviour has negative tests.

### Consistency

- Field meanings, error codes, limits and call models match `fin-assist/contracts`; a deviation is stated in the PR.
- Names follow the existing ones; no second way to do what a package here already does (transactions, clients,
  config, logging).
- No framework, ORM or DI container; a new third-party dependency where the standard library would do is a finding.
- SOLID: business rules outside usecases, an interface declared by its implementation instead of its consumer, a
  usecase importing an adapter, or a fake that behaves unlike the real implementation is a finding.
- Metric labels stay bounded; a new metric has a help text and is listed in `docs/server.md`.

### Checklist

- New code has unit tests; new functionality has integration tests and API tests (`apitest`), see Tests.
- In `bank-bff`, a new or changed REST endpoint has API tests over HTTP with responses validated against
  `openapi.yaml`.
- A new gRPC method has its row in `AccessPolicy()` matching grpc-api.md 3.3.
- A new client-facing error goes through `finerr`; a new reason or metadata key exists in `contracts` first.
- A new query by object ID is scoped by the user and has a test with another user's ID.
- A new effect with an idempotency record or an outbox row is written in one `db.Run`, and a retry test shows it is
  not applied twice.
- A new migration follows `docs/migrations.md` and passes squawk and up → down → up.
- A new metric has bounded labels and is listed in `docs/server.md`; a new setting is declared in `Vars` and the
  tables of `docs/config.md` are regenerated, not edited by hand.
- Unit, integration and API each cover at least 80% of all production statements; combined does not replace a suite.
- Every test is selected by its suite prefix, actually runs and is not skipped; profile evidence is nonempty.

## Renovate automation

FIN-211: dependency-only Renovate PRs may be merged/closed by the reviewed `renovate-policy.yml` controller, after exact-head required CI and bot-only commit checks. Ordinary PRs and release-to-main PRs still require the owner. Do not enable native automerge or bypass protections. Preserve base policy in `renovate-base.json` (schedules, groups, allowed versions and manual Go upgrades); generated target/version approvals live on the separate `renovate-policy` branch. Failed exact values are isolated for explicit Dashboard approval; new values keep their original groups. AndroidX/Compose and mixed groups never auto-merge. See `docs/renovate-policy.md` for credentials, safeguards, bootstrap and verification.
