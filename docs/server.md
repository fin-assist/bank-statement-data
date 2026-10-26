# gRPC, фоновые задания и сборка сервиса

Контракт межсервисного взаимодействия — [`fin-assist/contracts/docs/grpc-api.md`](https://github.com/fin-assist/contracts/blob/main/docs/grpc-api.md). FIN-258 подключает опубликованные механизмы вместо локальных копий. Публичные параметры, порядок интерцепторов, правила metadata, ошибок и retry описаны в [`go-grpc v0.2.1`](https://github.com/fin-assist/go-grpc/tree/v0.2.1); lifecycle заданий — в [`go-worker v0.1.1`](https://github.com/fin-assist/go-worker/tree/v0.1.1).

## Ответственность сервиса

`internal/app` связывает `go-grpc/server` с identity/tombstone реализациями, logger, реестром метрик и настройками сервиса. `wire()` регистрирует прикладные обработчики и возвращает реальные исходящие клиенты и jobs; `AccessPolicy()` задаёт правила зарегистрированных методов по таблице grpc-api.md 3.3. FIN-342 регистрирует шесть RPC StatementsService, UserSettingsSyncService.SetUserTimezone и UserDeletionService.PurgeUserData; прикладной жизненный цикл описан в [import.md](import.md). Health и dev reflection принадлежат инфраструктурной сборке.

Обработчики остаются в `internal/adapter/grpc/<feature>/`, usecase — в `internal/<feature>/`. Данные контекста читаются через `github.com/fin-assist/go-grpc/reqctx`; локального `internal/reqctx` нет. Ошибки контракта создаются через `finerr`; внутренние bare status создаёт только библиотека `go-grpc/server`. Запрет обхода `finerr` в `.golangci.yml` сохраняется.

## Identity и удалённые пользователи

`AUTH_MODE=verify` (FIN-267) — одна сборка `identity.NewAssembly` из `go-identity v0.2.1`: она реализует `server.ServiceVerifier`, `server.UserVerifier` и `client.TokenSource`, вторая цепочка аутентификации не ставится. При `ENV=prod` вызывающие — ServiceAccount Kubernetes (JWKS API-сервера по HTTPS только с CA из `AUTH_KUBERNETES_CA_FILE`, отдельный API-токен, проверка namespace и `sub`), исходящий токен — projected `SERVICE_TOKEN_FILE`. При `ENV=stage` — JWT стенда (`kid` = `sub` = имя сервиса, TTL 1 минута) по ключам из `AUTH_STAGE_*`; ключи стенда в prod — ошибка старта. Access JWT пользователя в обоих режимах — по ключам `user.GetSigningKeys`: соединение с `user` (`PEER_USER_ADDR`) принадлежит сборке, повторяет только этот метод и пишет в клиентские метрики сервиса (`grpc_client_*{peer="user"}`; поэтому `client.NewMetrics` вызывается до сборки identity); задание `identity-keys` обновляет кэш ключей, `App.Close` после остановки заданий закрывает соединение. Нет свежих ключей — голый `UNAVAILABLE`, не `INVALID_TOKEN` (FIN-293). Метрики библиотеки: `finassist_identity_rejected_total{kind,reason}`, `finassist_identity_jwks_skipped_keys{kind,reason}`. `verify` с `ENV=dev` или неизвестным `ENV` отклоняется, `insecure-dev` разрешён только при `ENV=dev` — и в `config.Load`, и в `app.New`.

Удалённые пользователи (FIN-267) — `go-tombstone v0.1.0`, «Сборка с go-grpc»: admission над `Store.Deleted` читает primary при каждом пользовательском вызове без кэша; удалённому — `TOKEN_REVOKED` до обработчика, сбой чтения или `sub` не UUID — голый `UNAVAILABLE`. Отказы считает `finassist_tombstone_revoked_total` (методов с `AllowDeleted` нет, поэтому каждый положительный ответ — отказ). `Interceptors` библиотеки не ставятся. `Guard` проверяется заново в пишущих транзакциях; `PurgeUserData` и `SetUserTimezone` доступны только `{Callers: user, Model: ModelSystem}`. UUID проверяются RPC-адаптером до обращения к repository; tombstone admission дополнительно проверяет UUID пользовательского sub.

## Клиенты и задания

Общий dial, service token и request metadata — `github.com/fin-assist/go-grpc/client`. Адреса peers, retry-списки безопасных методов, таймауты и token source выбирает сервис. Прикладной адаптер конкретного peer появляется в `internal/adapter/client/<peer>/` только вместе с usecase; транспортную реализацию библиотеки не копируют.

Общий запуск и периодичность — `github.com/fin-assist/go-worker`. Сервис явно передаёт logger, clock и RestartDelay=5s, сохраняя прежнюю политику перезапуска независимо от default библиотеки. Сборка запускает `statement-import` (два восстанавливаемых обработчика durable queue), `statement-files` (очистка и аудит сирот) и `idempotency-cleanup` (сохранённые ключи); их реализации — `internal/importing` и опубликованная библиотека идемпотентности. В verify добавляется `identity-keys`. Задания запускаются в каждом поде: единственность эффекта обеспечивает прикладная блокировка или `FOR UPDATE SKIP LOCKED`, а не сам runner.

## PostgreSQL и lifecycle

Сервис передаёт pool options в `go-postgres.OpenPools` v0.3.0. Локальный `postgres.DB` остаётся тонким адаптером менеджера транзакций и маршрутизации: `Writer` выбирает транзакцию/primary, `Reader` — транзакцию, primary для `x-read-primary` от `bank-bff` или реплику. Репозитории и domain mapping принадлежат сервису; подробнее — [migrations.md](migrations.md).

`cmd/bank-statement-data` владеет listeners и порядком shutdown: HTTP `/readyz` (`ops.Readiness.Drain`, необратимо до конца процесса) и gRPC health одновременно перестают сообщать готовность, выдерживается `SHUTDOWN_DELAY` (вызовы ещё принимаются, `/healthz` отвечает 200), gRPC graceful stop ограничен `SHUTDOWN_TIMEOUT`, затем останавливаются jobs и закрываются пулы. Вынос библиотек не меняет этот порядок или конфигурационные defaults.

## Наблюдаемость

Logger и Prometheus registry создаёт сервис и явно передаёт библиотекам. `go-grpc` не должен молча отключить прежние server/client metrics из-за nil metrics/registry; logger по умолчанию в библиотеке не заменяет сервисный logger. Имена и метки сохраняются: `grpc_server_handled_total{method,code,reason}`, `grpc_server_handling_seconds{method}`, `finassist_grpc_denied_total{method,caller,cause}`, `grpc_client_handled_total{peer,method,code}`, `grpc_client_handling_seconds{peer,method}`. Пользовательские идентификаторы и другие неограниченные значения в labels запрещены.

`internal/logx` использует значения из `go-grpc/reqctx`. Логи не раскрывают токены, ключи, DSN и банковские данные; параметры маскирования и безопасного сообщения об ошибке задаёт контракт библиотеки. `/metrics`, `/healthz`, `/readyz` и readiness ping primary остаются в `internal/ops` и сборке сервиса. FIN-342 регистрирует `finassist_idempotency_requests_total{endpoint,outcome}` опубликованной go-idempotency v0.6.0 в том же реестре: endpoint ограничен зарегистрированным UploadStatement, outcome — фиксированным набором решений библиотеки. Нового process-wide logger/registry нет.
