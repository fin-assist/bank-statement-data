DROP TABLE metrics_delivery;
DROP TABLE metrics_publication;

ALTER TABLE upload DROP COLUMN statement_range_source;
ALTER TABLE upload DROP COLUMN statement_to_instant;
ALTER TABLE upload DROP COLUMN statement_from_instant;
