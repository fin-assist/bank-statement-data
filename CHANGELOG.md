# История изменений

## [Unreleased]

- FIN-266: справка `usage` называет бинарь `bank-statement-data`; документация миграций и `AGENTS.md` приведены к решениям FIN-194/FIN-195 и шаблону, описание `GRPC_MAX_RECV_MSG_SIZE` уточнено. Поведение сервиса не изменилось.
- FIN-258: общая инфраструктура подключена через go-grpc/go-migrate/go-worker v0.1.0 и go-postgres v0.2.0 вместо локальных копий. Сохранены SQL-миграции, схема, CLI, operational API, fail-closed identity, транзакции и shutdown; добавлены проверки сервисной сборки и публичного migration harness на трёх уровнях.

### Изменено

- FIN-240: транзакции переведены на go-postgres v0.1.1; mapper ограничений остаётся в адаптере сервиса и обрабатывает ошибки callback и COMMIT. Чужой transaction context отклоняется без autocommit.
