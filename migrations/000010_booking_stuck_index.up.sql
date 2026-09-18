-- The reconciliation worker and the admin stuck-list both scan for bookings
-- left 'held' past an age threshold, filtering and ordering by created_at.
-- idx_booking_reconciliation is on hold_expires_at and cannot serve that.
-- Partial index: only held rows matter, and they are a tiny slice of the table.
CREATE INDEX idx_booking_stuck ON booking(created_at) WHERE status = 'held';
