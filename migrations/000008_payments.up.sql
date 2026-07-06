CREATE TABLE payments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id UUID NOT NULL REFERENCES booking(id),
    provider VARCHAR(50) NOT NULL,
    provider_txn_id VARCHAR(255),
    status VARCHAR(20) NOT NULL,
    amount_cents BIGINT NOT NULL CHECK (amount_cents >= 0),
    currency CHAR(3) NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
    card_brand VARCHAR(20),
    card_last4 CHAR(4) CHECK (card_last4 IS NULL OR card_last4 ~ '^[0-9]{4}$'),
    provider_response JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (status IN ('initiated', 'succeeded', 'failed', 'unknown', 'refunded'))
);

CREATE INDEX idx_payments_booking ON payments(booking_id);
CREATE INDEX idx_payments_txn ON payments(provider_txn_id) WHERE provider_txn_id IS NOT NULL;
