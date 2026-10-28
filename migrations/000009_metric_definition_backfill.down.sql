DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM metrics_backfill) OR EXISTS (SELECT 1 FROM recalc_queue WHERE registry_only) THEN
        RAISE EXCEPTION 'Cannot remove metric definition progress; roll back the binary while keeping the additive schema';
    END IF;
END $$;
DROP TABLE metrics_backfill;
ALTER TABLE recalc_queue DROP COLUMN registry_only;
