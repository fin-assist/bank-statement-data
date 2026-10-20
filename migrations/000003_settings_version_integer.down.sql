-- Rollback of 000003: settings_version back to smallint. Nothing is lost while every version
-- fits; a version above 32 767 cannot be stored in smallint, so the rollback refuses instead of
-- truncating it, and the whole file is rolled back. Then roll back the application, not the
-- schema (docs/migrations.md, «Откат»).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM user_state WHERE settings_version > 32767) THEN
        RAISE EXCEPTION '000003_settings_version_integer: settings_version above 32767 does not fit smallint, rollback refused';
    END IF;
END $$;

ALTER TABLE user_state ALTER COLUMN settings_version TYPE smallint;
