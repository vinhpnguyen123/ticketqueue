CREATE INDEX idx_audit_actor ON audit_log(actor_user_id, created_at) WHERE actor_user_id IS NOT NULL;
