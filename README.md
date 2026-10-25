# bank-statement-data

Выпуск версии сервиса и повторная публикация GitHub Release описаны в [docs/releasing.md](docs/releasing.md). История изменений — [CHANGELOG.md](CHANGELOG.md). История начинается с пустого `Unreleased`; версии и теги создаёт владелец после PR и post-merge CI. Публикация образа и deployment — отдельные этапы.

Go-сервис «Понятных финансов»: приём выписок (OFX Т-Банка), счета, операции и записи источников, справочник категорий, регулярные платежи и отметки «Не подписка», расчёт персональных метрик для `analytics`, инструменты чтения для помощника (`docs/grpc-api.md` 7.2 в [`fin-assist/contracts`](https://github.com/fin-assist/contracts)). Создан из шаблона [`fin-assist/go-service-template`](https://github.com/fin-assist/go-service-template) ([FIN-117](https://finassist.youtrack.cloud/issue/FIN-117), перенос — [FIN-190](https://finassist.youtrack.cloud/issue/FIN-190)). Контракты (`.proto`, сгенерированный код, реестр ошибок `finerr`) — в `fin-assist/contracts`, модуль `github.com/fin-assist/contracts/gen/go`.

- [docs/server.md](docs/server.md) — gRPC-сервер и интерцепторы, идентичность, таблица доступа, клиенты, транзакции, логи и метрики.
- [docs/config.md](docs/config.md) — переменные окружения.
- [docs/migrations.md](docs/migrations.md) — миграции и запросы на pgx ([FIN-115](https://finassist.youtrack.cloud/issue/FIN-115)).
- [docs/schema.md](docs/schema.md) — что держит схема БД и какие инварианты проверяет код ([FIN-111](https://finassist.youtrack.cloud/issue/FIN-111)).
- [AGENTS.md](AGENTS.md) — карта и правила для агентов, Code Review Rules.

## Автоматизация обновлений зависимостей

[Политика Renovate](docs/renovate-policy.md) (FIN-211): одна актуальная release/main, автомерж полностью ботских PR после обязательного CI, один повтор красного CI и закрытие с явным повтором через Dependency Dashboard. AndroidX/Compose остаются на ручной проверке. Исходные группировки, ограничения версий и расписания — `renovate-base.json`; generated target и quarantine — ветка `renovate-policy`. Активация требует отдельного токена и строгой защиты веток; по умолчанию выключена.

## Что ещё не сделано после переноса шаблона

- `AccessPolicy()` и обработчики в `wire()` (`internal/app/wire.go`) — вместе с первыми RPC сервиса, строки из таблицы grpc-api.md 3.3.
- Из `go-tombstone` подключены только источник миграций и admission (FIN-267); `Guard` в пишущих сценариях и `UserDeletionService.PurgeUserData` (grpc-api.md 7: «каждый реализует») — вместе с первыми RPC, которые пишут данные пользователя (решение владельца 25.10.2026). Outbox (FIN-109) и идемпотентность (FIN-78) — отдельные задачи подключения в бизнес-сборке.
- Секреты репозитория (`CLAUDE_CODE_OAUTH_TOKEN` для ревью `@claude`, `GO_MODULES_TOKEN` для приватных модулей fin-assist), Claude GitHub App и Codex — владелец.

## Раскладка

```
cmd/bank-statement-data/           main: serve and migrate commands, listeners, shutdown
internal/app/                      assembly: identity, pools, server, wire() (handlers, clients, jobs), access table, migration sources
internal/app/apitest/              the whole service in-process for API tests
internal/domain/                   entities and invariants, no infrastructure imports
internal/<feature>/                usecases; interfaces of repositories and clients they need
go-grpc/{server,client,reqctx}     shared transport, token and request context; service assembly in internal/app
internal/adapter/grpc/<feature>/   gRPC handlers: messages <-> usecases
internal/adapter/postgres/         pools, transaction manager, repositories on pgx
go-migrate / migratetest          shared runner and isolated PostgreSQL database harness
internal/adapter/client/<peer>/    application peer adapters when needed; shared dial/token in go-grpc/client
go-worker                         shared job lifecycle; service jobs when implemented
internal/config/                   configuration from the environment
internal/testdb/                   test PostgreSQL server selection (counted in all profiles)
internal/testidentity/             stand key files, signed test tokens and a user GetSigningKeys peer (counted in all profiles)
internal/logx/                     slog with values from the context
internal/ops/                      /metrics, /healthz, /readyz, readiness
migrations/                        SQL migrations, embedded into the binary
deploy/systemd/                    example unit for the stand
```

Раскладка — по руководству go.dev «Organizing a Go module» (серверный проект), слои и интерфейсы на стороне потребителя — по go-clean-arch v4: сценарий в `internal/<feature>` объявляет интерфейсы нужных ему репозиториев и клиентов, адаптеры их реализуют, связывает всё `wire()`.

## Запуск

```bash
# PostgreSQL 18, migrations and the service in Docker (dev mode, docs/config.md)
make up
# the same service on the host against the compose PostgreSQL
make run
```

- **Образ** — `Dockerfile`: статический бинарь на distroless, пользователь `nonroot`. Один бинарь: `serve` в Deployment, `migrate up` в Job. Публикация в GHCR — FIN-148.
- **Стенд** (FIN-149) — тот же бинарь под systemd: пример unit-файла с секретами через `LoadCredential` — [deploy/systemd/bank-statement-data.service](deploy/systemd/bank-statement-data.service). Скрипт деплоя запускает `migrate up` перед перезапуском unit.
- **k3s** — манифесты в `fin-assist/infra` (FIN-147): ServiceAccount, projected token, headless Service, пробы `grpc`.

## Библиотеки и обновления

Общий код сервисов — отдельные Go-библиотеки из шаблона [`fin-assist/go-library-template`](https://github.com/fin-assist/go-library-template) ([FIN-118](https://finassist.youtrack.cloud/issue/FIN-118)): [`go-identity`](https://github.com/fin-assist/go-identity) (FIN-119), [`go-idempotency`](https://github.com/fin-assist/go-idempotency) (FIN-78), [`go-outbox`](https://github.com/fin-assist/go-outbox) (FIN-109), [`go-tombstone`](https://github.com/fin-assist/go-tombstone) (FIN-110).

- **Подключение** — по разделу «Внедрение» README библиотеки: `go get github.com/fin-assist/go-<имя>@vX.Y.Z`, источник миграций в `MigrationSources()` до источника сервиса, сборка в `wire()` — библиотека получает от сервиса транзакцию (`db.Tx`), логгер, реестр метрик, часы и настройки.
- **Версии** — semver-теги библиотеки, `go.mod` сервиса закрепляет версию; до первого прода — `v0`, несовместимое изменение — новый MINOR с записью «Несовместимо» в CHANGELOG библиотеки. Правила — `docs/releasing.md` в `fin-assist/go-library-template`.
- **Обновления приносит Renovate** (`renovate.json`): модули `github.com/fin-assist/*` (библиотеки и `contracts/gen/go`) — отдельный PR сразу после тега, остальные зависимости — один PR в неделю, версия Go — вручную. PR Renovate проходит CI сервиса и обрабатывается по [политике FIN-211](docs/renovate-policy.md). При несовместимом обновлении человеческий коммит в открытом PR переводит его на ручное сопровождение; если ботский PR уже закрыт после повторного падения, исправления сервиса идут отдельным PR с задачей YouTrack. Порядок миграции — по CHANGELOG библиотеки.
- **Проверить библиотеку до выпуска** — локально `go mod edit -replace github.com/fin-assist/go-<имя>=../go-<имя>`; `replace` в PR сервиса не попадает.

FIN-258 подключает опубликованные `go-grpc v0.1.0` (server/client/reqctx), `go-migrate v0.1.0`, `go-worker v0.1.0` и `go-postgres v0.2.0`. Локальные копии транспорта, контекста, runner и worker удалены. `postgres.DB` остаётся тонким адаптером транзакций и read-primary routing; пулы создаёт `go-postgres.OpenPools`. Сопоставление SQLSTATE и имени ограничения доменной ошибке принадлежит сервису. Новые библиотеки заменяют ранее скопированную инфраструктуру, stdlib не предоставляет её контрактных реализаций.

Схема и все существующие SQL-миграции сохранены; CLI мигрирует настоящий service source. Пустые `AccessPolicy()`/`wire()` сохраняются до отдельных задач реализации сервиса. Перенос инфраструктуры не добавляет бизнес-RPC, импорт OFX или задание партиций FIN-193. `serve` передаёт logger, registry, clock и интервал перезапуска 5 с явно.

## Идентичность и удалённые пользователи (FIN-267)

`AUTH_MODE=verify` собирается через `identity.NewAssembly` из [`go-identity v0.2.0`](https://github.com/fin-assist/go-identity/tree/v0.2.0) (README, «Внедрение», шаг 0): при `ENV=prod` — токены ServiceAccount Kubernetes (JWKS API-сервера по HTTPS с доверием только к `AUTH_KUBERNETES_CA_FILE`, исходящий токен — `SERVICE_TOKEN_FILE`), при `ENV=stage` — короткие JWT стенда с ключами из файлов `AUTH_STAGE_*`. Access JWT пользователя проверяется в обоих режимах по ключам `user.GetSigningKeys` (`PEER_USER_ADDR`, повтор — только этот метод); фоновое задание `identity-keys` обновляет ключи. Любое другое `ENV` с `verify`, в том числе `dev`, — ошибка старта; `insecure-dev` — только при `ENV=dev`, проверка и в `config.Load`, и в `app.New`. Ошибка сборки не открывает пулов и не оставляет зарегистрированных метрик. Новая обязательная зависимость старта — достижимый адрес `user` и файлы ключей; сами ключи загружаются лениво или заданием, поэтому кратковременная недоступность `user` старт не ломает, а проверяемые запросы получают `UNAVAILABLE`, не `INVALID_TOKEN`.

[`go-tombstone v0.1.0`](https://github.com/fin-assist/go-tombstone/tree/v0.1.0) подключён по разделу «Сборка с go-grpc»: источник миграций библиотеки (`tombstone_users`) стоит в `MigrationSources()` перед сервисным, admission `server.Deps.Tombstones` читает primary без кэша. Пользовательский вызов удалённого пользователя получает `TOKEN_REVOKED` до обработчика, сбой чтения — голый `UNAVAILABLE`; отказы считает `finassist_tombstone_revoked_total` (методов с `AllowDeleted` у сервиса нет). Переменные — [docs/config.md](docs/config.md), детали — [docs/server.md](docs/server.md).

Публичный `go-migrate/migratetest` создаёт изолированную БД из выбранного сервисом adminDSN и проверяет up/down/up; `internal/testdb` выбирает сервер PG18. Оба обычных Go helper-пакета сервиса (`testdb` и `apitest`) учитываются во всех трёх профилях. Подробнее — [docs/testing.md](docs/testing.md).

## Проверки

CI (`.github/workflows/ci.yml`):

| Job | Что |
|---|---|
| `go` | `go mod tidy` без изменений, `go vet`, три отдельных набора `unit`/`integration`/`api` с `-race`, PostgreSQL 18, проверкой фактического исполнения и порогом 80% statements каждого; combined дополнительно, отчёты в сводке и артефактах; тесты скриптов |
| `lint` | golangci-lint (`.golangci.yml`), в том числе `forbidigo` против обхода `finerr` и `depguard` против фреймворков, ORM и DI-контейнеров |
| `image` | сборка образа и запуск бинаря в нём |
| `migrations` (PR) | существующие миграции не изменены, squawk по новым |

Локально:

```bash
make test   # Docker for testcontainers, or TEST_DATABASE_URL=postgres://... to use your PostgreSQL
make lint
```

### Покрытие statements по трём наборам (FIN-238)

Для **каждого** набора `unit`, `integration` и `api` обязателен порог **80% statements всего production-кода сервиса**. Сбор — штатный Go coverage, проверка — `vladopajic/go-test-coverage/v2` **v2.20.1**. Общий результат `combined` показывает объединение профилей силами инструмента; он не компенсирует провал отдельного набора. Внешнего сервиса отчётности нет.

```bash
make collect-unit          # unit.out + unit.jsonl, without a percentage gate
make collect-integration   # real PostgreSQL required
make collect-api           # full application over its public API
make check-unit            # the corresponding 80% gate
make check-integration
make check-api
make cover-combined        # informational union of the three profiles
make cover                 # collect all three, then enforce each gate
```

Каждый прогон использует `-race -count=1 -timeout=8m -coverpkg=./...` и выбирает функции с префиксом `TestUnit`, `TestIntegration` или `TestAPI` (селектор `^(Test|Fuzz)<Набор>`). Имена проверяет `make test-names` до прогонов через штатный `go test -race -list`: каждый тест — `TestUnit…`/`TestIntegration…`/`TestAPI…`, каждая fuzz-цель — `FuzzUnit…`/`FuzzIntegration…`/`FuzzAPI…` (её seed-корпус выполняется в своём наборе). `Example…` в сервисе отклоняется: публичного Go API у сервиса нет, а префикс набора в имени примера невозможен, и он выполнялся бы вне всех трёх наборов (FIN-290). Все наборы инструментируют один и тот же Go-модуль целиком, включая generated production-код и Go-файлы инфраструктуры `apitest`/`testdb`; исключений пакетов или файлов ради порога нет. `_test.go` Go в production-профиль не включает. Чистые функции и правила — unit; PostgreSQL, транспорт, клиенты и фоновые процессы — integration; полная сборка приложения с вызовом публичного API — api. Прямой вызов handler не считается API-тестом.

`scripts/check-suite.sh` проверяет JSON-события Go: набор должен действительно выполнить хотя бы один тест и завершиться без failed/skipped тестов. Отсутствующий, пустой профиль, только заголовок и профиль без исполняемых statements отвергаются. Это проверка факта исполнения, не собственный расчёт процентов. Для всех coverage-прогонов `CI=true`: отсутствие PostgreSQL даёт ошибку, а не успешный skip. Инструмент округляет процент до одного знака перед проверкой порога; это согласованное поведение v2.20.1.

CI сохраняет `.out`, `.jsonl`, `*.coverage.txt` и `*.report.txt` как артефакт, публикует отдельные unit/integration/api/combined в сводке. Обязательный job `go` остаётся красным при провале любого отдельного набора. Fork PR не получают права записи комментариев.

Прежний критерий «80% изменённых исполняемых строк» заменён тремя порогами statements. Исторические проценты строк не сопоставимы с новыми. Проценты не доказывают полноту бизнес-сценариев.

Подробности классификации и исходные дефициты — [docs/testing.md](docs/testing.md).

Модули `github.com/fin-assist/*` берутся из git, минуя прокси (`GOPRIVATE`, задан в `Makefile` и CI). Для приватного — токен с чтением репозиториев fin-assist: локально `git config --global url."https://x-access-token:<token>@github.com/fin-assist/".insteadOf "https://github.com/fin-assist/"`, в CI — секрет `GO_MODULES_TOKEN`, в `docker build` — секрет `netrc`.
