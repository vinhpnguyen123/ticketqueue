-- The duplicate-charge guard read this index but nothing enforced it: two
-- requests could both see no held booking and both insert one.
DROP INDEX idx_booking_user_active;
CREATE UNIQUE INDEX idx_booking_user_active ON booking(user_id) WHERE status = 'held';
