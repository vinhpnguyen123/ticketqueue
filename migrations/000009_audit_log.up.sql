-- NO foreign keys — intentional (must outlive entities, high-throughput inserts).

CREATE TABLE audit_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_type VARCHAR(50) NOT NULL,
    entity_id UUID NOT NULL,
    action VARCHAR(50) NOT NULL,
    actor_type VARCHAR(20) NOT NULL CHECK (actor_type IN ('user', 'worker', 'admin', 'system')),
    actor_user_id UUID,
    before_state JSONB,
    after_state JSONB,
    metadata JSONB,
    request_id VARCHAR(255),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_audit_entity ON audit_log(entity_type, entity_id, created_at);
CREATE INDEX idx_audit_actor ON audit_log(actor_user_id, created_at) WHERE actor_user_id IS NOT NULL;
CREATE INDEX idx_audit_request ON audit_log(request_id) WHERE request_id IS NOT NULL;
