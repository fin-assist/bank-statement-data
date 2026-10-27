-- Durable merchant identities may already be referenced by clients. Keeping them
-- is intentional; the prior schema and calculator accept these rows unchanged.
SELECT 1;
ALTER TABLE user_state DROP COLUMN uploads_changed_at;
