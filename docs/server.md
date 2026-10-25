# gRPC, фоновые задания и сборка сервиса

Контракт межсервисного взаимодействия — [`fin-assist/contracts/docs/grpc-api.md`](https://github.com/fin-assist/contracts/blob/main/docs/grpc-api.md). FIN-258 подключает опубликованные механизмы вместо локальных копий. Публичные параметры, порядок интерцепторов, правила metadata, ошибок и retry описаны в [`go-grpc v0.1.0`](https://github.com/fin-assist/go-grpc/tree/v0.1.0); lifecycle заданий — в [`go-worker v0.1.0`](https://github.com/fin-assist/go-worker/tree/v0.1.0).

## Ответственность сервиса

`internal/app` связывает `go-grpc/server` с identity/tombstone реализациями, logger, реестром метрик и настройками сервиса. `wire()` регистрирует прикладные обработчики и возвращает реальные исходящие клиенты и jobs; `AccessPolicy()` задаёт правила зарегистрированных методов по таблице grpc-api.md 3.3. Пустой каркас не добавляет бизнес-RPC, клиентов или заданий. Health и dev reflection принадлежат инфраструктурной сборке.

Обработчики остаются в `internal/adapter/grpc/<feature>/`, usecase — в `internal/<feature>/`. Данные контекста читаются через `github.com/fin-assist/go-grpc/reqctx`; локального `internal/reqctx` нет. Ошибки контракта создаются через `finerr`; внутренние bare status создаёт только библиотека `go-grpc/server`. Запрет обхода `finerr` в `.golangci.yml` сохраняется.

## Identity и удалённые пользователи

Сервис передаёт серверу реализации интерфейсов проверки сервисного и пользовательского токенов. В каркасе подключение production identity остаётся явным TODO: `AUTH_MODE=verify` отказывает при старте до интеграции, а `insecure-dev` разрешён только при `ENV=dev`. Это не обход проверки в stage/prod.

`NoTombstones` остаётся временной явной реализацией для каркаса. Сервис с данными пользователей обязан заменить её интеграцией `go-tombstone` в `internal/app`; вынос transport-кода не реализует эту интеграцию автоматически.

## Клиенты и задания

Общий dial, service token и request metadata — `github.com/fin-assist/go-grpc/client`. Адреса peers, retry-списки безопасных методов, таймауты и token source выбирает сервис. Прикладной адаптер конкретного peer появляется в `internal/adapter/client/<peer>/` только вместе с usecase; транспортную реализацию библиотеки не копируют.

Общий запуск и периодичность — `github.com/fin-assist/go-worker`. Сервис явно передаёт logger, clock и RestartDelay=5s, сохраняя прежнюю политику перезапуска независимо от default библиотеки. Реальные jobs размещаются в `internal/worker/<job>/`. Задания запускаются в каждом поде: единственность эффекта обеспечивает прикладная блокировка или `FOR UPDATE SKIP LOCKED`, а не сам runner.

## PostgreSQL и lifecycle

Сервис передаёт pool options в `go-postgres.OpenPools` v0.2.0. Локальный `postgres.DB` остаётся тонким адаптером менеджера транзакций и маршрутизации: `Writer` выбирает транзакцию/primary, `Reader` — транзакцию, primary для `x-read-primary` от `bank-bff` или реплику. Репозитории и domain mapping принадлежат сервису; подробнее — [migrations.md](migrations.md).

`cmd/bank-statement-data` владеет listeners и порядком shutdown: readiness/health перестают сообщать готовность, выдерживается `SHUTDOWN_DELAY`, gRPC graceful stop ограничен `SHUTDOWN_TIMEOUT`, затем останавливаются jobs и закрываются пулы. Вынос библиотек не меняет этот порядок или конфигурационные defaults.

## Наблюдаемость

Logger и Prometheus registry создаёт сервис и явно передаёт библиотекам. `go-grpc` не должен молча отключить прежние server/client metrics из-за nil metrics/registry; logger по умолчанию в библиотеке не заменяет сервисный logger. Имена и метки сохраняются: `grpc_server_handled_total{method,code,reason}`, `grpc_server_handling_seconds{method}`, `finassist_grpc_denied_total{method,caller,cause}`, `grpc_client_handled_total{peer,method,code}`, `grpc_client_handling_seconds{peer,method}`. Пользовательские идентификаторы и другие неограниченные значения в labels запрещены.

`internal/logx` использует значения из `go-grpc/reqctx`. Логи не раскрывают токены, ключи, DSN и банковские данные; параметры маскирования и безопасного сообщения об ошибке задаёт контракт библиотеки. `/metrics`, `/healthz`, `/readyz` и readiness ping primary остаются в `internal/ops` и сборке сервиса. Смена зависимостей не добавляет новые метрики или process-wide logger/registry.
