-- booking.idempotency_key is the bridge to the provider, whose ledger is global:
-- one key must identify exactly one booking.
DROP INDEX idx_booking_idem;
CREATE UNIQUE INDEX idx_booking_idem ON booking(idempotency_key);
