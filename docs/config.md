# Конфигурация

Всё — переменные окружения (`internal/config`). Секрет можно передать файлом: `VAR_FILE=/путь` вместо `VAR` (systemd `LoadCredential`, смонтированный Secret Kubernetes); заданы оба — ошибка запуска. Ошибки конфигурации выводятся все сразу и не содержат значений секретов.

Шаблон не опирается на Kubernetes: адреса других сервисов, DSN и файл токена — обычные настройки, одинаковые для k3s и стенда под systemd.

## Хранилище выписок

Для `serve` нужны отдельный приватный бакет, ключ SSE-KMS и путь к файлу IAM-токена. `migrate` этих настроек не читает. Токен обновляет внешний механизм; приложение не кеширует его содержимое и перечитывает файл перед каждым обращением к S3.

До открытия listener сервис проверяет версионирование, lifecycle и Object Lock. Версионирование бакета никогда не должно было включаться: `Suspended` не подходит, потому что сохраняет старые версии. Активные правила expiration и Object Lock запрещены. Настройки бакета должны оставаться неизменными во время работы сервиса; его роль не должна менять настройки бакета.

Файл создаётся условным PUT с `If-None-Match: *`. Удаление заменяет содержимое подтверждённым зашифрованным пустым объектом по тому же ключу. Это технический маркер: он не содержит выписки и препятствует позднему PUT после таймаута. Маркеры не удаляются ни сборщиком сирот, ни lifecycle, ни внешними скриптами. Роль сервиса должна читать настройки бакета и выполнять PUT, GET и ListObjectsV2; DeleteObject приложению не нужен. Приватность бакета и доступ роли поддержки задаются инфраструктурой отдельно от этих проверок.

## Как меняется

Таблицы ниже **генерируются** из списка `Vars` в `internal/config/vars.go` — там у каждой переменной имя, значение по умолчанию, признак секрета и описание; код берёт значения по умолчанию оттуда же. Новая переменная или новое значение по умолчанию:

1. строка в `Vars`, чтение в `load` / `loadMigrate`;
2. `go test ./internal/config -run TestUnitDocIsCurrent -update` — переписывает таблицы между маркерами.

Руками блоки между маркерами не правятся. CI (`go test`) падает, если таблица устарела (`TestUnitDocIsCurrent`), если код читает необъявленную переменную или объявленная нигде не читается (`TestUnitVarsMatchWhatLoadReads`).

## `serve`

<!-- generated:config-serve:start (go test ./internal/config -run TestUnitDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `SERVICE_NAME` | — (обязательна) | имя сервиса: логи, `application_name`, токен в dev |
| `ENV` | `prod` | `dev`, `stage` или `prod`. Reflection включён только при `dev` |
| `AUTH_MODE` | `verify` | `verify` — проверка токенов через `go-identity` (`NewAssembly`): при `ENV=prod` — ServiceAccount Kubernetes, при `ENV=stage` — JWT стенда с ключами из файлов; access JWT пользователя — по ключам `user.GetSigningKeys`. `insecure-dev` — токены `dev:<имя>` без проверки, только при `ENV=dev` |
| `AUTH_USER_ISSUER` | — | доверенный issuer access JWT `user`; обязателен при `verify` |
| `AUTH_USER_AUDIENCE` | — | доверенная audience access JWT `user`; обязательна при `verify` |
| `SERVICE_TOKEN_FILE` | — | projected-токен ServiceAccount этого сервиса (audience `finassist-internal`) для исходящих вызовов; обязателен при `verify` и `ENV=prod`, отдельный от `AUTH_KUBERNETES_API_TOKEN_FILE` |
| `AUTH_KUBERNETES_ISSUER` | `https://kubernetes.default.svc.cluster.local` | `ENV=prod`: issuer токенов ServiceAccount; сверить с настройкой кластера |
| `AUTH_KUBERNETES_JWKS_URL` | `https://kubernetes.default.svc/openid/v1/jwks` | `ENV=prod`: HTTPS-адрес ключей ServiceAccount без query, fragment и credentials |
| `AUTH_KUBERNETES_CA_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | `ENV=prod`: CA Kubernetes API, единственное доверие для JWKS |
| `AUTH_KUBERNETES_API_TOKEN_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | `ENV=prod`: API-токен пода для чтения JWKS; отдельный от `finassist-internal` |
| `AUTH_KUBERNETES_NAMESPACE` | `finassist` | `ENV=prod`: доверенный namespace вызывающих ServiceAccount |
| `AUTH_STAGE_ISSUER` | — | `ENV=stage`: общий issuer сервисных JWT стенда; обязателен в stage, запрещён в prod |
| `AUTH_STAGE_SIGNING_KEY_FILE` | — | `ENV=stage`: PKCS#8 PEM Ed25519 ключ этого сервиса (права `0600`), читается при старте; обязателен в stage, запрещён в prod |
| `AUTH_STAGE_PUBLIC_KEYS_FILE` | — | `ENV=stage`: JWKS ключей сервисов стенда (только OKP Ed25519, `kid` — имя сервиса), читается при старте; на стенде — общий файл `/etc/finassist/stage-public.jwks` (права `0644`); обязателен в stage, запрещён в prod |
| `DB_PRIMARY_DSN` / `_FILE` | — (обязательна) | DSN primary (`-rw`) |
| `DB_REPLICA_DSN` / `_FILE` | — | DSN реплик (`-ro`); пусто — чтение тоже с primary. На стенде оба указывают на один PostgreSQL |
| `DB_MAX_CONNS` | `10` | размер каждого пула |
| `OBJECT_STORAGE_ENDPOINT` | `https://storage.yandexcloud.net` | HTTPS endpoint S3 без credentials, query и fragment; сервис не обращается к банку |
| `OBJECT_STORAGE_BUCKET` | — (обязательна) | отдельный приватный бакет выписок: версионирование никогда не включалось, без Object Lock и активного удаления по lifecycle |
| `OBJECT_STORAGE_KMS_KEY_ID` | — (обязательна) | ключ SSE-KMS; каждый PUT файла и пустого маркера требует aws:kms |
| `OBJECT_STORAGE_IAM_TOKEN_FILE` | — (обязательна) | абсолютный путь к файлу IAM-токена; внешний механизм обновляет токен, адаптер перечитывает его перед каждым запросом |
| `GRPC_ADDR` | `:9090` | gRPC |
| `GRPC_MAX_RECV_MSG_SIZE` | `4194304` | байт, предел размера входящего сообщения. Лимит 16 МиБ из grpc-api.md 9 относится к ответу `SearchOperations`, а не к запросам: приём не поднимать без метода с большим запросом |
| `HTTP_ADDR` | `:9091` | `/metrics`, `/healthz`, `/readyz` |
| `PEER_<NAME>_ADDR` | — | адрес другого сервиса: `PEER_BANK_STATEMENT_DATA_ADDR` → `cfg.Peers["bank-statement-data"]`. k3s: `dns:///bank-statement-data.finassist.svc.cluster.local:9090`; стенд: `dns:///127.0.0.1:9092`. `PEER_USER_ADDR` обязателен при `AUTH_MODE=verify`: ключи access JWT (`GetSigningKeys`) |
| `LOG_LEVEL` | `info` | `debug` добавляет в строку вызова metadata и запрос с замаскированными токенами |
| `LOG_FORMAT` | `json` | `json` или `text` |
| `SHUTDOWN_DELAY` | `5s` | после `NOT_SERVING` сервер ещё принимает вызовы, пока балансировщики не уберут под |
| `SHUTDOWN_TIMEOUT` | `20s` | сколько ждать завершения вызовов; оставшиеся обрываются (потоки ответа клиент переоткроет) |
<!-- generated:config-serve:end -->

## `migrate`

<!-- generated:config-migrate:start (go test ./internal/config -run TestUnitDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `DB_PRIMARY_DSN` / `_FILE` | — (обязательна) | DSN primary, роль сервиса. Миграции никогда не идут на реплику |
| `MIGRATE_LOCK_TIMEOUT` | `5s` | `lock_timeout`: DDL, который не дождался блокировки занятой таблицы, падает, а не держит очередь запросов сервиса за собой |
| `MIGRATE_STATEMENT_TIMEOUT` | `15m` | `statement_timeout` каждой команды |
| `MIGRATE_WAIT_TIMEOUT` | `15m` | сколько ждать, пока другой запуск миграций того же источника закончит |
<!-- generated:config-migrate:end -->

Как действуют таймауты миграций — [migrations.md](migrations.md), «Таймауты».
