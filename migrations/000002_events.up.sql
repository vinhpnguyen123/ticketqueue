CREATE TABLE events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(255) NOT NULL,
    venue VARCHAR(255) NOT NULL,
    event_at TIMESTAMPTZ NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'draft',
    max_concurrent_windows INT NOT NULL DEFAULT 50,
    purchase_window_seconds INT NOT NULL DEFAULT 300,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (status IN ('draft', 'on_sale', 'sold_out', 'closed', 'cancelled')),
    CHECK (max_concurrent_windows > 0),
    CHECK (purchase_window_seconds > 0)
);

CREATE INDEX idx_events_status ON events(status, event_at);
