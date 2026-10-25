# История изменений

## [Unreleased]

- FIN-258: общая инфраструктура подключена через go-grpc/go-migrate/go-worker v0.1.0 и go-postgres v0.2.0 вместо локальных копий. Сохранены SQL-миграции, схема, CLI, operational API, fail-closed identity, транзакции и shutdown; добавлены проверки сервисной сборки и публичного migration harness на трёх уровнях.

### Изменено

- FIN-240: транзакции переведены на go-postgres v0.1.1; mapper ограничений остаётся в адаптере сервиса и обрабатывает ошибки callback и COMMIT. Чужой transaction context отклоняется без autocommit.
