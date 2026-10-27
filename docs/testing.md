# Проверки сервиса (FIN-238 / FIN-258 / FIN-342)

FIN-258 заменил инфраструктурные копии опубликованными Go-библиотеками по канонике FIN-246. FIN-342 заполнил AccessPolicy и wire: зарегистрированы шесть RPC StatementsService, SetUserTimezone и PurgeUserData, долговечная обработка импорта и очистка файлов. Существующие PostgreSQL-тесты схемы сохранены; миграции проверяются публичным go-migrate/migratetest, включая внутренний составной ключ unread_line.

- Unit без БД/сети: конфигурация/валидация, CLI usage/exit status, OFX и денежные границы, правила/результаты/непрочитанные факты, обработчики и worker lifecycle через consumer-интерфейсы, routing/owned и foreign transaction, assembly cleanup и IO failures.
- Integration: реальные PostgreSQL18, up/down/up и service migration version6, сохранение данных при upgrade и безопасный отказ rollback, parser→PG и durable recovery, транзакции/constraint mapping, отдельный object barrier, TLS-протокол object storage, gRPC SDK/interceptors, HTTP/readiness, конфигурационные файлы и логирование.
- API: полная CLI/сервисная assembly с настоящими миграциями, PostgreSQL и TLS provider; шесть RPC импорта и системные вызовы, EOF/лимиты/replay/конфликт, результаты/история/unread repair и удаление, tenant/caller isolation, двухфазный Purge и cloud outage/recovery. Инфраструктурные health Watch/Check/reflection, HTTP healthz/readyz/metrics/404, shutdown и отказы старта также сохранены. Изолированный adapter не выдаётся за API всей сборки; тестовые бизнес-RPC/SQL/jobs в runtime не добавлены.

`internal/testdb` выбирает PG18, публичный `migratetest.DSN(t, adminDSN)` владеет отдельной БД и cleanup, `CheckUpDownUp(t, dsn, src)` проверяет миграции на переданной БД. Ordinary .go сервисных testdb/apitest остаются во всех профилях. Три независимых80% по всему одинаковому модулю, race/count1/timeouts и guards сохранены. Combined информационный. Проценты текущего commit публикуются в CI artifacts; локальный TEST_DATABASE_URL не исполняет container branch, которая остаётся в знаменателе.

## FIN-329: identity, tombstone, остановка, имена наборов

- Unit: правила `AUTH_*` для prod/stage/других `ENV`, чтение и defaults; отказы сборки identity без регистрации метрик и обращений к `user`; сборка стенда без сети; dev-шлюз `insecure-dev`; admission tombstone (считаются только отказы, ошибки передаются); `/readyz` после `Drain` и порядок с уведомлением в полёте; `DB_MAX_CONNS` вне int32; фикстура `internal/testidentity` — её токены проверяются самим `go-identity` (подлинные проходят, поддельные и просроченные — нет), поддельный `user` отказывает без верного токена стенда.
- Integration: реальные ключи стенда, `user.GetSigningKeys` по TCP с проверкой сервисного токена, `tombstone_users` на PostgreSQL после всех источников миграций, цепочка go-grpc с проверочным методом пользовательской модели: живой пользователь, `TOKEN_REVOKED` удалённому (в том числе UUID в верхнем регистре), `INVALID_TOKEN` поддельному, просроченному и отсутствующему токену, `UNAVAILABLE` для `sub` не UUID, отказ поддельному сервисному токену и чужому вызывающему; обработчик в отказах не вызывается. Отдельно: ветка prod сборки против TLS JWKS Kubernetes (временный CA, отдельный API-токен, namespace и audience, недоверенный сертификат — `UNAVAILABLE` без запроса), недоступный `user.GetSigningKeys` при пустом кэше — голый `UNAVAILABLE` до обработчика без внутреннего текста, отказ виден в `grpc_client_handled_total{peer="user"}`.
- API: полная сборка `apitest` в `ENV=stage`/`verify`, процесс `serve` на стенде (готовность, задание `identity-keys` получает ключи `user` своим токеном, остановка), отказ старта без настроек identity, `/readyz` 503 во время `SHUTDOWN_DELAY`; `make test-names` для Fuzz/Example (тесты скриптов).

До FIN-342 публичных прикладных RPC не было: неизвестный метод отвечал `UNIMPLEMENTED` до интерцепторов, поэтому отказы пользовательского токена и `TOKEN_REVOKED` проверялись integration-тестами сборки. Теперь API-набор вызывает настоящие зарегистрированные методы; admission, caller/tenant isolation и revocation проверяются через них.

## Исторический результат до FIN-258

## Исходная точка 24.10.2026

| Набор | Statements | Выполнение тестов | Порог 80% |
|---|---|---|---|
| unit | 35.1% | Успех, PostgreSQL не нужен | Не выполнен |
| integration | 70.0% | Успех на PostgreSQL 18 | Не выполнен |
| api | 27.4% | Успех на PostgreSQL 18 | Не выполнен |
| combined | 86.5% | Информационное объединение | Отдельные наборы не заменяет |

Это штатный замер go-test-coverage v2.20.1 после FIN-240 на Go 1.26.0 darwin/amd64 (общий знаменатель 1015 statements) с `-race -count=1 -timeout=8m -coverpkg=./...`, а не подтверждение завершения FIN-238. Старые метрики строк сюда не переносились. Целевой 80% применяется к каждому набору; код и тестовая инфраструктура из знаменателя не исключались. Недостаток покрытия не скрывается и не заполняется искусственными тестами ради процентов.

`AccessPolicy` и `wire` ещё не реализуют бизнес-RPC сервиса. API-набор проверяет инфраструктуру, но не бизнес-сценарии. Их реализация остаётся в задачах разработки сервиса. В том историческом состоянии обязательный CI gate был красным; результаты FIN-258 проверяются заново на текущем commit.

Команды, версия инструмента, одинаковый знаменатель, проверка пустого/неисполненного набора и артефакты описаны в [README](../README.md#покрытие-statements-по-трём-наборам-fin-238). Процент statements не доказывает полноту сценариев; проверка через тестовый gRPC-сервер не подтверждает работу с развёрнутыми соседними сервисами.

После актуализации на main с FIN-240 сохранены adapter/constraint mappings и все новые тесты go-postgres. Транспортные/PG проверки относятся к integration; изолированные routing/rejected-query contracts — к unit. Бизнес-RPC по-прежнему отсутствуют.

## FIN-355: финансовые снимки

Новые unit/integration/API сценарии расчёта, fixture-источники, проверка UUID/отметок и границы
публичного API описаны в [calculations.md](calculations.md#проверки). Все три набора по-прежнему
инструментируют весь production-модуль; пороги/исключения не менялись.

FIN-355 review: hand-computed OFX-сценарий проверяет на обоих реальных уровнях суммы/статусы/базы
наблюдений и исторических метрик, каждую финансовую selection; отдельный Apia-сценарий проходит
импорт/расчёт через PG и API. Unit различает exclusive global/inclusive period bounds, signed trunc,
целочисленную границу среднего и точный ApplyRules-кейс ручного transfer/refund. Гонка calculation/purge
подтверждает PostgreSQL blocker до освобождения транзакции; UUID-проверка использует два непустых tenant.
