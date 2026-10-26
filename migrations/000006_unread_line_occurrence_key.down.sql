-- Do not merge or discard facts to restore the narrower historical key.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM unread_line
        GROUP BY user_id, upload_id, line_number
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'Cannot restore unread_line key while physical lines contain multiple unread facts';
    END IF;
END $$;

ALTER TABLE unread_line DROP CONSTRAINT unread_line_pkey;
ALTER TABLE unread_line ADD PRIMARY KEY (user_id, upload_id, line_number);
-- Recreate the prepared-index state of migration 005 so down-one/up-one works.
-- This rollback is a maintenance operation and uses the runner's bounded lock timeout.
CREATE UNIQUE INDEX unread_line_occurrence_key
    ON unread_line (user_id, upload_id, line_number, line_occurrence);
