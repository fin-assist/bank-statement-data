# История изменений

## [Unreleased]

### Добавлено

- FIN-342: полный путь импорта OFX: шесть RPC, долговечный приём/разбор и расчёты на PostgreSQL, приватное KMS-хранилище Yandex с защитой от позднего PUT, отмена/удаление/двухфазный Purge, история и непрочитанные строки; опубликованные contracts v0.5.0 и go-idempotency v0.5.0. Публичные API операций/аналитики и deployment не входят в изменение.

### Изменено

- FIN-347: версии библиотек в `AGENTS.md`, README, `docs/server.md` и `docs/migrations.md` приведены к `go.mod`: go-postgres v0.3.0, go-grpc v0.2.0, go-migrate v0.1.1; шапка unit-файла стенда называет режимы файлов ключей стенда (ключ подписи 0600 root, общий JWKS 0644). Только документация: код и поведение сервиса не менялись.
- FIN-267: `AUTH_MODE=verify` работает: проверка токенов — `go-identity v0.2.0` (`NewAssembly`; prod — ServiceAccount Kubernetes, stage — JWT стенда с ключами из файлов, пользователи — ключи `user.GetSigningKeys`, задание `identity-keys`). Новые переменные `AUTH_USER_*`, `AUTH_KUBERNETES_*`, `AUTH_STAGE_*`; `PEER_USER_ADDR` обязателен. Удалённые пользователи — `go-tombstone v0.1.0`: источник миграций `tombstone` перед сервисным и `TOKEN_REVOKED` на admission. `verify` с `ENV=dev` — ошибка старта.
- FIN-290: `make test-names` проверяет fuzz-цели (`FuzzUnit…`/`FuzzIntegration…`/`FuzzAPI…`, seed-корпус — в своём наборе) и отклоняет `Example…`.

### Исправлено

- FIN-289: во время `SHUTDOWN_DELAY` HTTP `/readyz` отвечает 503 вместе с gRPC health `NOT_SERVING`; `app.New` отказывает в `AUTH_MODE=insecure-dev` вне `ENV=dev` и без `config.Load`; `DB_MAX_CONNS` вне int32 — ошибка конфигурации, а не усечённое значение; глобальное исключение gosec G115 снято.

- FIN-266: справка `usage` называет бинарь `bank-statement-data`; документация миграций и `AGENTS.md` приведены к решениям FIN-194/FIN-195 и шаблону, описание `GRPC_MAX_RECV_MSG_SIZE` уточнено; тест миграций проверяет up → down → up каждого источника `MigrationSources()`. Поведение сервиса не изменилось.
- FIN-258: общая инфраструктура подключена через go-grpc/go-migrate/go-worker v0.1.0 и go-postgres v0.2.0 вместо локальных копий. Сохранены SQL-миграции, схема, CLI, operational API, fail-closed identity, транзакции и shutdown; добавлены проверки сервисной сборки и публичного migration harness на трёх уровнях.

### Изменено

- FIN-240: транзакции переведены на go-postgres v0.1.1; mapper ограничений остаётся в адаптере сервиса и обрабатывает ошибки callback и COMMIT. Чужой transaction context отклоняется без autocommit.
