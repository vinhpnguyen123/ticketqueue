-- Correctness verification for a load-test run.
--
-- Every check prints PASS or FAIL. A load-test number means nothing without
-- these: "10k concurrent" is only interesting if the tickets and the money still
-- add up afterwards.
--
-- Checks 1-8 live here. Check 9 (Redis vs Postgres) needs redis-cli and lives in
-- scripts/verify_run.sh, which runs this file and then does it.

\pset footer off

\echo ''
\echo '=== 1. No booking holds more tickets than it paid for (Layer 3) ==='
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS result,
       COUNT(*) AS offending_bookings
FROM (
    SELECT tk.booking_id
    FROM tickets tk
    WHERE tk.status = 'sold'
    GROUP BY tk.booking_id
    HAVING COUNT(*) > (SELECT b.quantity FROM booking b WHERE b.id = tk.booking_id)
) x;

\echo ''
\echo '=== 2. Sold count never exceeds tier capacity (the oversell check) ==='
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS result,
       COUNT(*) AS oversold_tiers
FROM (
    SELECT t.id
    FROM ticket_tier t
    JOIN tickets tk ON tk.ticket_tier_id = t.id
    GROUP BY t.id, t.quantity_total
    HAVING COUNT(tk.id) FILTER (WHERE tk.status = 'sold') > t.quantity_total
) x;

\echo ''
\echo '=== 3. Sold tickets == sum of confirmed booking quantities ==='
SELECT CASE WHEN sold_tickets = confirmed_qty THEN 'PASS' ELSE 'FAIL' END AS result,
       sold_tickets, confirmed_qty
FROM (
    SELECT (SELECT COUNT(*) FROM tickets WHERE status = 'sold') AS sold_tickets,
           (SELECT COALESCE(SUM(quantity), 0) FROM booking WHERE status = 'confirmed') AS confirmed_qty
) x;

\echo ''
\echo '=== 4. Nobody got a free ticket (confirmed without a succeeded payment) ==='
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS result,
       COUNT(*) AS free_tickets
FROM booking b
LEFT JOIN payments p ON p.booking_id = b.id AND p.status = 'succeeded'
WHERE b.status = 'confirmed' AND p.id IS NULL;

\echo ''
\echo '=== 5. Nobody paid without getting a ticket (succeeded without confirmed) ==='
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS result,
       COUNT(*) AS paid_for_nothing
FROM payments p
JOIN booking b ON b.id = p.booking_id
WHERE p.status = 'succeeded' AND b.status <> 'confirmed';

\echo ''
\echo '=== 6. Confirmed revenue == succeeded payment amounts ==='
SELECT CASE WHEN booked = charged THEN 'PASS' ELSE 'FAIL' END AS result,
       booked AS booked_cents, charged AS charged_cents
FROM (
    SELECT (SELECT COALESCE(SUM(total_cents), 0) FROM booking WHERE status = 'confirmed') AS booked,
           (SELECT COALESCE(SUM(amount_cents), 0) FROM payments WHERE status = 'succeeded') AS charged
) x;

\echo ''
\echo '=== 7a. Ticket count per tier matches quantity_total (drift checker B1) ==='
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS result,
       COUNT(*) AS bad_tiers
FROM (
    SELECT t.id
    FROM ticket_tier t
    LEFT JOIN tickets tk ON tk.ticket_tier_id = t.id
    GROUP BY t.id, t.quantity_total
    HAVING COUNT(tk.id) <> t.quantity_total
) x;

\echo ''
\echo '=== 7b. No sold ticket on a non-confirmed booking (drift checker B2) ==='
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS result,
       COUNT(*) AS bad_tickets
FROM tickets tk
JOIN booking b ON b.id = tk.booking_id
WHERE tk.status = 'sold' AND b.status <> 'confirmed';

\echo ''
\echo '=== 7c. No held ticket on a failed/cancelled booking (drift checker B3) ==='
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS result,
       COUNT(*) AS bad_tickets
FROM tickets tk
JOIN booking b ON b.id = tk.booking_id
WHERE tk.status = 'held' AND b.status IN ('failed', 'cancelled');

\echo ''
\echo '=== 8. Nothing still held after the run settled (5 min) ==='
SELECT CASE WHEN COUNT(*) = 0 THEN 'PASS' ELSE 'FAIL' END AS result,
       COUNT(*) AS stuck_bookings
FROM booking
WHERE status = 'held' AND created_at < NOW() - INTERVAL '5 minutes';
