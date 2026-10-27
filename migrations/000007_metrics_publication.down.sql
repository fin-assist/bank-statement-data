DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM metrics_publication) OR EXISTS (SELECT 1 FROM metrics_delivery)
       OR EXISTS (SELECT 1 FROM user_state WHERE daily_recalc_attempts>0) THEN
        RAISE EXCEPTION 'Cannot remove published metric inventory or pending deliveries; roll back the binary while keeping the additive schema';
    END IF;
END $$;

DROP TABLE metrics_delivery;
DROP TABLE metrics_publication;

ALTER TABLE upload DROP COLUMN statement_range_source;
ALTER TABLE upload DROP COLUMN statement_to_instant;
ALTER TABLE upload DROP COLUMN statement_from_instant;

ALTER TABLE user_state DROP COLUMN daily_recalc_failed_since;
ALTER TABLE user_state DROP COLUMN daily_recalc_attempts;
