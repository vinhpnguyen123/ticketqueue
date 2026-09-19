-- Restore the original shape, same ordering rule: create before dropping.
CREATE INDEX idx_tickets_claim ON tickets(ticket_tier_id, status) WHERE status = 'available';

DROP INDEX idx_tickets_claim_ordered;
