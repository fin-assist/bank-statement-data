-- FIN-358: read-only assistant tools must resolve stable merchant selections on replicas
-- before the first metrics calculation. Existing UUIDs are never replaced.
INSERT INTO merchant(user_id,key)
SELECT DISTINCT user_id,description FROM operation
ON CONFLICT(user_id,key) DO NOTHING;

ALTER TABLE user_state ADD COLUMN uploads_changed_at timestamptz;
COMMENT ON COLUMN user_state.uploads_changed_at IS
 'Монотонное время последнего успешного импорта или удаления загрузки для changed_since помощника. '
 'Изменяется в транзакции записи; расчёты и удаление файлов его не меняют.';
UPDATE user_state s SET uploads_changed_at=(
 SELECT max(GREATEST(u.finished_at,u.deleted_at)) FROM upload u
 WHERE u.user_id=s.user_id AND u.status IN('done','deleted')
);

INSERT INTO regular_payment(user_id,periodicity,description)
SELECT DISTINCT user_id,'monthly',description FROM operation WHERE direction='expense' AND description<>''
ON CONFLICT(user_id,periodicity,description) DO NOTHING;

ALTER TABLE user_state ADD COLUMN categories_revision bigint NOT NULL DEFAULT 0;
ALTER TABLE user_state ADD COLUMN uploads_revision bigint NOT NULL DEFAULT 0;
COMMENT ON COLUMN user_state.categories_revision IS
 'Монотонная ревизия учётных правок. Продвигается в транзакции изменения для точного changed_since помощника.';
COMMENT ON COLUMN user_state.uploads_revision IS
 'Монотонная ревизия успешных импортов и удалений загрузок. Не зависит от часов или фоновых расчётов.';
UPDATE user_state SET categories_revision=CASE WHEN categories_changed_at IS NULL THEN 0 ELSE 1 END,
 uploads_revision=CASE WHEN uploads_changed_at IS NULL THEN 0 ELSE 1 END;
