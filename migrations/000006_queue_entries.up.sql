CREATE TABLE queue_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id),
    event_id UUID NOT NULL REFERENCES events(id),
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status VARCHAR(20) NOT NULL DEFAULT 'waiting',
    admitted_at TIMESTAMPTZ,
    left_at TIMESTAMPTZ,
    CHECK (status IN ('waiting', 'admitted', 'expired', 'left', 'completed'))
);

CREATE UNIQUE INDEX idx_queue_active ON queue_entries(user_id, event_id) WHERE status IN ('waiting', 'admitted');
CREATE INDEX idx_queue_recovery ON queue_entries(event_id, joined_at) WHERE status = 'waiting';
CREATE INDEX idx_queue_window_count ON queue_entries(event_id, admitted_at) WHERE status = 'admitted';
