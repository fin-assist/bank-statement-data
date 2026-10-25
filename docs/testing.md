# Проверки сервиса (FIN-238 / FIN-258)

FIN-258 заменяет инфраструктурные копии опубликованными Go-библиотеками по канонике FIN-246. Бизнес-RPC не добавлены: AccessPolicy и wire остаются пустыми. Существующие PostgreSQL-тесты схемы и SQL-миграций сохранены, изолированный database harness теперь публичный go-migrate/migratetest.

- Unit без БД/сети: конфигурация/валидация, CLI usage/exit status, порядок shutdown и worker restart через consumer-интерфейсы, routing/owned и foreign transaction, assembly cleanup, IO failures тестового harness.
- Integration: реальные PostgreSQL18, up/down/up и service migration version3, существующие schema contracts, транзакции/constraint mapping, pool ownership и cleanup, HTTP/readiness, конфигурационные файлы и логирование.
- API: полная CLI/сервисная assembly с настоящими миграциями, gRPC health Watch/Check/reflection, отказ отсутствующим RPC, HTTP healthz/readyz/metrics/404, outage/recovery БД, shutdown, неудачные старты с verify identity/недоступной БД/занятым listener. Тестовые бизнес-RPC/SQL/jobs в runtime не добавлены.

`internal/testdb` выбирает PG18, публичный `migratetest.DSN(t, adminDSN)` владеет отдельной БД и cleanup, `CheckUpDownUp(t, dsn, src)` проверяет миграции на переданной БД. Ordinary .go сервисных testdb/apitest остаются во всех профилях. Три независимых80% по всему одинаковому модулю, race/count1/timeouts и guards сохранены. Combined информационный. Проценты текущего commit публикуются в CI artifacts; локальный TEST_DATABASE_URL не исполняет container branch, которая остаётся в знаменателе.

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
