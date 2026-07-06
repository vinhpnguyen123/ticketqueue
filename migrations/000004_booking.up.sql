CREATE TABLE booking (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    event_id UUID NOT NULL REFERENCES events(id),
    ticket_tier_id UUID NOT NULL REFERENCES ticket_tier(id),
    quantity INT NOT NULL CHECK (quantity > 0),
    idempotency_key VARCHAR(255) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'held',
    total_cents BIGINT NOT NULL CHECK (total_cents >= 0),
    currency CHAR(3) NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
    hold_expires_at TIMESTAMPTZ NOT NULL,
    version INT NOT NULL DEFAULT 0,
    confirmed_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (status IN ('held', 'confirmed', 'failed', 'cancelled')),
    CHECK (
        (status = 'held' AND confirmed_at IS NULL AND failed_at IS NULL) OR
        (status = 'confirmed' AND confirmed_at IS NOT NULL) OR
        (status IN ('failed', 'cancelled') AND failed_at IS NOT NULL)
    )
);

CREATE INDEX idx_booking_reconciliation ON booking(hold_expires_at) WHERE status = 'held';
CREATE INDEX idx_booking_user_active ON booking(user_id) WHERE status = 'held';
CREATE INDEX idx_booking_user ON booking(user_id, id DESC);
CREATE INDEX idx_booking_idem ON booking(idempotency_key);
CREATE INDEX idx_booking_tier ON booking(ticket_tier_id);
