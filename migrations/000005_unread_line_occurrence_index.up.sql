-- This file contains one statement; the migration runner executes it outside a transaction.
-- squawk-disable-assume-in-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS unread_line_occurrence_key
    ON unread_line (user_id, upload_id, line_number, line_occurrence);
