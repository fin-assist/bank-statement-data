# История изменений

## [Unreleased]

### Добавлено

- FIN-342: несколько непрочитанных операций одной физической строки OFX сохраняются отдельно через внутренний `line_occurrence`; миграции 000004–000006 сохраняют прежние записи, строят индекс конкурентно и отказывают в откате, который потерял бы факты. Физические номера и публичная схема не меняются, исправление/удаление последующей выписки закрывает/открывает каждую ошибку независимо.

- FIN-342: полный путь импорта OFX: шесть RPC, долговечный приём/разбор и расчёты на PostgreSQL, приватное KMS-хранилище Yandex с защитой от позднего PUT, отмена/удаление/двухфазный Purge, история и непрочитанные строки; опубликованные contracts v0.5.0 и go-idempotency v0.6.0 (runner go-migrate v0.2.0 по MVS). Публичные API операций/аналитики и deployment не входят в изменение.

### Изменено

- FIN-342: открытый Watch повторно проверяет primary tombstone перед каждым опросом и после чтения перед отправкой; частичный и завершённый Purge отзывают поток без ожидания срока разбора, без Guard в RR и без блокировок через сетевую доставку.

- FIN-349: пример unit-файла стенда `deploy/systemd/bank-statement-data.service` читает общий JWKS стенда по единому пути `/etc/finassist/stage-public.jwks` (0644, каталог `/etc/finassist` 0755, DynamicUser читает напрямую, без `LoadCredential`); прежний `/etc/finassist/stand/stage-public-keys.json` больше не читается. `SERVICE_NAME=bank-statement-data` (kid/sub ключа подписи стенда) задаёт сам unit; шапка называет файлы и режимы и предупреждает, что `EnvironmentFile=` перекрывает `Environment=`. Unit-тест загружает конфигурацию из unit-файла и отклоняет env-файл, повторяющий переменные unit. Раскатка на стенде — по README, «Раскатка unit-файла на стенд (FIN-349)»: положить JWKS в `/etc/finassist/stage-public.jwks` (0644, владелец root), удалить из `/etc/finassist/bank-statement-data/env` переменные, которые старый unit задаёт сам (`ENV`, `DB_PRIMARY_DSN_FILE`, `AUTH_STAGE_SIGNING_KEY_FILE`, `AUTH_STAGE_PUBLIC_KEYS_FILE`; если значение отличается от значения unit, сначала положить нужное содержимое в источник credential unit), установить новый unit и `daemon-reload`, только затем удалить `SERVICE_NAME`, затем `migrate up` и перезапуск. Код сервиса не менялся.
- FIN-347: версии библиотек в `AGENTS.md`, README, `docs/server.md` и `docs/migrations.md` приведены к `go.mod`: go-postgres v0.3.0, go-grpc v0.2.0, go-migrate v0.1.1; шапка unit-файла стенда называет режимы файлов ключей стенда (ключ подписи 0600 root, общий JWKS 0644). Только документация: код и поведение сервиса не менялись.
- FIN-267: `AUTH_MODE=verify` работает: проверка токенов — `go-identity v0.2.0` (`NewAssembly`; prod — ServiceAccount Kubernetes, stage — JWT стенда с ключами из файлов, пользователи — ключи `user.GetSigningKeys`, задание `identity-keys`). Новые переменные `AUTH_USER_*`, `AUTH_KUBERNETES_*`, `AUTH_STAGE_*`; `PEER_USER_ADDR` обязателен. Удалённые пользователи — `go-tombstone v0.1.0`: источник миграций `tombstone` перед сервисным и `TOKEN_REVOKED` на admission. `verify` с `ENV=dev` — ошибка старта.
- FIN-290: `make test-names` проверяет fuzz-цели (`FuzzUnit…`/`FuzzIntegration…`/`FuzzAPI…`, seed-корпус — в своём наборе) и отклоняет `Example…`.

### Исправлено

- FIN-342: все S3 response bodies (объект, List и проверки bucket) проверяют исходный caller context и действующий HTTP budget до и после транспортного Read, включая EOF и пробу лимита; отмена/истечение бюджета не превращается в успешный EOF, частичные байты и zero-length semantics сохраняются. Monotonic expiration соответствует прежней HTTP-фазе после загрузки токена; после успешного EOF cap не распространяется на CPU-разбор, повторный EOF/Close сохраняются, новые timers/context не создаются.

- FIN-351: проверка systemd unit стенда отклоняет повтор имени `LoadCredential=`, `LoadCredentialEncrypted=` и `ImportCredential=`; добавлены отрицательные мутации unit-файла, включая чужой источник перед правильным. Изменены только тесты, действующий unit и runtime не меняются.
- FIN-289: во время `SHUTDOWN_DELAY` HTTP `/readyz` отвечает 503 вместе с gRPC health `NOT_SERVING`; `app.New` отказывает в `AUTH_MODE=insecure-dev` вне `ENV=dev` и без `config.Load`; `DB_MAX_CONNS` вне int32 — ошибка конфигурации, а не усечённое значение; глобальное исключение gosec G115 снято.

- FIN-266: справка `usage` называет бинарь `bank-statement-data`; документация миграций и `AGENTS.md` приведены к решениям FIN-194/FIN-195 и шаблону, описание `GRPC_MAX_RECV_MSG_SIZE` уточнено; тест миграций проверяет up → down → up каждого источника `MigrationSources()`. Поведение сервиса не изменилось.
- FIN-258: общая инфраструктура подключена через go-grpc/go-migrate/go-worker v0.1.0 и go-postgres v0.2.0 вместо локальных копий. Сохранены SQL-миграции, схема, CLI, operational API, fail-closed identity, транзакции и shutdown; добавлены проверки сервисной сборки и публичного migration harness на трёх уровнях.

### Изменено

- FIN-240: транзакции переведены на go-postgres v0.1.1; mapper ограничений остаётся в адаптере сервиса и обрабатывает ошибки callback и COMMIT. Чужой transaction context отклоняется без autocommit.
