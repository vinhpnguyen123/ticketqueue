-- Webhook delivery is at-least-once: the provider retries on any non-2xx, and a
-- retry after a successful-but-slow response is indistinguishable from a first
-- delivery. This table is the dedup ledger, keyed by the provider's event id.
CREATE TABLE webhook_events_seen (
    event_id VARCHAR(255) PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ
);

-- For the cleanup worker, which deletes by age.
CREATE INDEX idx_webhook_received ON webhook_events_seen(received_at);
