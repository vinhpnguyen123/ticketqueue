DROP INDEX idx_booking_user_active;
CREATE INDEX idx_booking_user_active ON booking(user_id) WHERE status = 'held';
