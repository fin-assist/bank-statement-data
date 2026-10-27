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
