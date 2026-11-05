DROP INDEX idx_booking_idem;
CREATE INDEX idx_booking_idem ON booking(idempotency_key);
