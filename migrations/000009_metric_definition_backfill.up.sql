ALTER TABLE recalc_queue ADD COLUMN registry_only boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN recalc_queue.registry_only IS 'Причина пересчёта только в изменении реестра: прежние снимки сохраняются без публичного признака recalculating; пользовательская правка переводит значение в false.';

ALTER TABLE metrics_publication ADD COLUMN pending_user_action boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN metrics_publication.pending_user_action IS 'С каким публичным признаком отправлено незавершённое начало пересчёта: пользовательская правка после тихого бэкфилла требует новой публикации true, а последующие повторы не создают лишних версий.';

CREATE TABLE metrics_backfill (
    catalog_version integer PRIMARY KEY,
    fingerprint text NOT NULL,
    cursor uuid,
    planned_users bigint NOT NULL DEFAULT 0,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE metrics_backfill IS 'Хранит, для каких пользователей сервис уже создал задания на пересчёт прошлых месяцев. После перезапуска сервис продолжает с того места, где остановился.';
COMMENT ON COLUMN metrics_backfill.catalog_version IS 'Поколение текущего выпуска реестра для защиты от обратного пересчёта старой репликой.';
COMMENT ON COLUMN metrics_backfill.fingerprint IS 'Отпечаток реестра выпуска; одинаковое поколение с другим содержимым отклоняется.';
COMMENT ON COLUMN metrics_backfill.cursor IS 'Последний просканированный идентификатор пользователя для возобновления прохода после рестарта; NULL до первого пакета и после завершения.';
COMMENT ON COLUMN metrics_backfill.planned_users IS 'Количество просканированных строк пользователей, включая пропущенные после одновременного удаления; не означает подтверждённую доставку.';
COMMENT ON COLUMN metrics_backfill.finished_at IS 'Момент окончания постановки истории в существующую очередь; не означает завершённый пересчёт или доставку.';
COMMENT ON COLUMN metrics_backfill.created_at IS 'Когда зарегистрирован текущий ограниченный проход выпуска реестра.';
