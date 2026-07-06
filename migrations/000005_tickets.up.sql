-- INVARIANT: ticket rows are created only at tier creation time, exactly quantity_total per tier.
-- No runtime inserts. Continuously verified by the drift-checker worker.

CREATE TABLE tickets (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_tier_id UUID NOT NULL REFERENCES ticket_tier(id),
    booking_id UUID REFERENCES booking(id),
    status VARCHAR(20) NOT NULL DEFAULT 'available',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (status IN ('available', 'held', 'sold')),
    CONSTRAINT chk_tickets_booking CHECK (
        (status = 'available' AND booking_id IS NULL) OR
        (status IN ('held', 'sold') AND booking_id IS NOT NULL)
    )
);

CREATE INDEX idx_tickets_claim ON tickets(ticket_tier_id, status) WHERE status = 'available';
CREATE INDEX idx_tickets_booking ON tickets(booking_id) WHERE booking_id IS NOT NULL;
