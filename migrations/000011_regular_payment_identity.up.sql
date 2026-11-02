-- squawk-disable-assume-in-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS regular_payment_id_unique ON regular_payment(id);
