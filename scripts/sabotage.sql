-- Deliberately corrupts data to prove the verification suite can FAIL.
--
-- A verification suite that has never failed is not evidence — it might be
-- checking nothing at all. This breaks two invariants in ways the application
-- cannot produce, runs against the DEV database only, and is undone by
-- re-seeding.
--
--   make sabotage && make verify-run     → expect FAIL on checks 4 and 2
--   make load-seed                       → back to a clean world
--
-- Note what is NOT done here: inflating the Redis inventory counter above the
-- tier's real capacity. That was the obvious sabotage and it does not work —
-- Layer 2 (the short-claim check in TX1) and Layer 3 (the guarded assignment)
-- both still refuse, so no oversell is produced. The system is built to survive
-- exactly that, which is why breaking it requires reaching past the application
-- and editing rows directly.

\echo 'SABOTAGE: confirming a booking with no succeeded payment (breaks check 4)'
UPDATE booking
SET status = 'confirmed', confirmed_at = NOW(), failed_at = NULL
WHERE id = (
    SELECT b.id FROM booking b
    LEFT JOIN payments p ON p.booking_id = b.id AND p.status = 'succeeded'
    WHERE b.status <> 'confirmed' AND p.id IS NULL
    LIMIT 1
);

\echo 'SABOTAGE: selling one more ticket than the tier has (breaks check 2)'
INSERT INTO tickets (ticket_tier_id, status, booking_id)
SELECT t.id, 'sold', (SELECT id FROM booking WHERE status = 'confirmed' LIMIT 1)
FROM ticket_tier t
JOIN events e ON e.id = t.event_id
WHERE e.status = 'on_sale'
ORDER BY t.created_at DESC
LIMIT 1;

\echo ''
\echo 'Now run: make verify-run   (checks 2 and 4 must FAIL, and 7a will too)'
\echo 'Then restore with: make load-seed'
