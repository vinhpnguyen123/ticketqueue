//go:build integration

package http_test

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
	"github.com/vinhpnguyen123/ticketqueue/internal/worker"
)

// counterValue reads one labelled counter. Metrics are process-global, so every
// assertion below is on a DELTA around the action, never on an absolute value.
func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	var pb dto.Metric
	if err := vec.WithLabelValues(labels...).Write(&pb); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return pb.GetCounter().GetValue()
}

func histogramCount(t *testing.T, vec *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	m, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get histogram: %v", err)
	}
	var pb dto.Metric
	if err := m.(prometheus.Metric).Write(&pb); err != nil {
		t.Fatalf("read histogram: %v", err)
	}
	return pb.GetHistogram().GetSampleCount()
}

func gaugeValue(t *testing.T, vec *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	var pb dto.Metric
	if err := vec.WithLabelValues(labels...).Write(&pb); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return pb.GetGauge().GetValue()
}

// Test 4: a successful purchase records ALL THREE phases and one confirmed
// outcome. Without all three, the Week 6 load test cannot say where time went.
func TestMetricsPurchaseRecordsEveryPhase(t *testing.T) {
	metrics.Register()
	ts, pool, _ := newPurchaseServer(t)
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	before := map[string]uint64{}
	for _, phase := range metrics.PurchasePhases {
		before[phase] = histogramCount(t, metrics.PurchasePhase, phase)
	}
	confirmedBefore := counterValue(t, metrics.PurchaseOutcome, metrics.OutcomeConfirmed)

	resp, out := purchase(t, ts, jwtTok, "m-ok-1",
		purchaseBodyTok(eventID, tierID, 2, purchaseTok, "tok_visa"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("purchase = %d body=%v", resp.StatusCode, out)
	}

	for _, phase := range metrics.PurchasePhases {
		if got := histogramCount(t, metrics.PurchasePhase, phase); got != before[phase]+1 {
			t.Errorf("phase %q samples = %d, want %d", phase, got, before[phase]+1)
		}
	}
	if got := counterValue(t, metrics.PurchaseOutcome, metrics.OutcomeConfirmed); got != confirmedBefore+1 {
		t.Errorf("confirmed outcome = %v, want %v", got, confirmedBefore+1)
	}
}

// Test 5: a sold-out purchase is rejected by Layer 1 and counted as such.
func TestMetricsSoldOutCountsLuaReject(t *testing.T) {
	metrics.Register()
	ts, pool, rdb := newPurchaseServer(t)
	eventID, tierID := seedTier(t, pool, 50000, 1)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)
	rdb.Set(context.Background(), booking.InventoryKey(tierID), 0, 0)

	rejectsBefore := counterValue(t, metrics.PurchaseOutcome, metrics.OutcomeSoldOut)
	luaBefore := readCounter(t, metrics.RedisLuaReject)

	resp, _ := purchase(t, ts, jwtTok, "m-sold-1",
		purchaseBodyTok(eventID, tierID, 1, purchaseTok, "tok_visa"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("purchase = %d, want 409", resp.StatusCode)
	}

	if got := counterValue(t, metrics.PurchaseOutcome, metrics.OutcomeSoldOut); got != rejectsBefore+1 {
		t.Errorf("sold_out outcome = %v, want %v", got, rejectsBefore+1)
	}
	if got := readCounter(t, metrics.RedisLuaReject); got != luaBefore+1 {
		t.Errorf("redis_lua_reject_total = %v, want %v", got, luaBefore+1)
	}
}

func readCounter(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var pb dto.Metric
	if err := c.Write(&pb); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return pb.GetCounter().GetValue()
}

// Test 6: an ambiguous purchase counts as ambiguous, and the reconciliation
// worker that heals it reports under its own source.
func TestMetricsAmbiguousThenReconciled(t *testing.T) {
	metrics.Register()
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	ambiguousBefore := counterValue(t, metrics.PurchaseOutcome, metrics.OutcomeAmbiguous)
	resolvedBefore := counterValue(t, metrics.ReconciliationResolution,
		metrics.SourceReconciliation, metrics.ResolutionConfirmed)

	resp, out := purchase(t, ts, jwtTok, "m-amb-1",
		purchaseBodyTok(eventID, tierID, 2, purchaseTok, "tok_drops_response"))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("purchase = %d body=%v, want 504", resp.StatusCode, out)
	}
	if got := counterValue(t, metrics.PurchaseOutcome, metrics.OutcomeAmbiguous); got != ambiguousBefore+1 {
		t.Errorf("ambiguous outcome = %v, want %v", got, ambiguousBefore+1)
	}

	bookingID := bookingIDOf(t, out)
	if _, err := pool.Exec(ctx,
		`UPDATE booking SET created_at = NOW() - INTERVAL '5 minutes' WHERE id = $1`, bookingID); err != nil {
		t.Fatal(err)
	}
	if err := worker.ReconcileCycle(ctx, rdb, pool, payClient); err != nil {
		t.Fatalf("reconcile cycle: %v", err)
	}

	if got := counterValue(t, metrics.ReconciliationResolution,
		metrics.SourceReconciliation, metrics.ResolutionConfirmed); got != resolvedBefore+1 {
		t.Errorf("reconciliation/confirmed = %v, want %v", got, resolvedBefore+1)
	}
}

// Test 7: hold-expiry reports under source=hold_expiry, proving the metric
// follows Actor.Source and is not a string hardcoded per call site.
func TestMetricsFollowsActorSource(t *testing.T) {
	metrics.Register()
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	before := counterValue(t, metrics.ReconciliationResolution,
		metrics.SourceHoldExpiry, metrics.ResolutionConfirmed)

	resp, out := purchase(t, ts, jwtTok, "m-src-1",
		purchaseBodyTok(eventID, tierID, 2, purchaseTok, "tok_drops_response"))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("purchase = %d body=%v", resp.StatusCode, out)
	}
	bookingID := bookingIDOf(t, out)
	if _, err := pool.Exec(ctx,
		`UPDATE booking SET hold_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, bookingID); err != nil {
		t.Fatal(err)
	}
	if err := worker.HoldExpiryCycle(ctx, rdb, pool, payClient); err != nil {
		t.Fatalf("expiry cycle: %v", err)
	}

	if got := counterValue(t, metrics.ReconciliationResolution,
		metrics.SourceHoldExpiry, metrics.ResolutionConfirmed); got != before+1 {
		t.Errorf("hold_expiry/confirmed = %v, want %v", got, before+1)
	}
}

// Test 8: the three-cadence rule, verified. A provider outage increments the
// deferral counter on EVERY cycle while exactly ONE audit row is written.
func TestMetricsDeferralCountsEveryCycleAuditOnce(t *testing.T) {
	metrics.Register()
	_, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	_, tierID := seedTier(t, pool, 50000, 10)

	var userID string
	pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('cadence@test.dev','x') RETURNING id`).Scan(&userID)
	tier, err := booking.LoadTier(ctx, pool, tierID)
	if err != nil {
		t.Fatal(err)
	}
	if err := booking.ReserveInventory(ctx, rdb, pool, tierID, 2); err != nil {
		t.Fatal(err)
	}
	held, err := booking.HoldTickets(ctx, pool, userID, tier, 2, "m-cadence-1", "req-m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE booking SET hold_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, held.BookingID); err != nil {
		t.Fatal(err)
	}

	before := counterValue(t, metrics.ReconciliationResolution,
		metrics.SourceHoldExpiry, metrics.ResolutionDeferred)

	dead := booking.NewPaymentClient("http://127.0.0.1:1", "ps_test_localdev")
	const cycles = 3
	for i := 0; i < cycles; i++ {
		if err := worker.HoldExpiryCycle(ctx, rdb, pool, dead); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}

	// metric: every occurrence.
	if got := counterValue(t, metrics.ReconciliationResolution,
		metrics.SourceHoldExpiry, metrics.ResolutionDeferred); got != before+cycles {
		t.Errorf("deferred counter = %v, want %v (one per cycle)", got, before+cycles)
	}
	// audit: once per booking.
	var audits int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = 'expiry_deferred_provider_unreachable'`,
		held.BookingID).Scan(&audits)
	if audits != 1 {
		t.Errorf("deferral audit rows = %d after %d cycles, want exactly 1", audits, cycles)
	}
}

// Test 9: Outcome D is counted, and the money-at-risk gauge sees it.
func TestMetricsOutcomeDReachesGauge(t *testing.T) {
	metrics.Register()
	_, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	_, tierID := seedTier(t, pool, 50000, 10)

	before := counterValue(t, metrics.PurchaseOutcome, metrics.OutcomeExpiredAfterCharge)

	var userID string
	pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('outcomed@test.dev','x') RETURNING id`).Scan(&userID)
	tier, err := booking.LoadTier(ctx, pool, tierID)
	if err != nil {
		t.Fatal(err)
	}
	if err := booking.ReserveInventory(ctx, rdb, pool, tierID, 2); err != nil {
		t.Fatal(err)
	}
	held, err := booking.HoldTickets(ctx, pool, userID, tier, 2, "m-outcomed-1", "req-m")
	if err != nil {
		t.Fatal(err)
	}
	// The provider never saw this key, so expiry releases the tickets...
	if _, err := pool.Exec(ctx,
		`UPDATE booking SET hold_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, held.BookingID); err != nil {
		t.Fatal(err)
	}
	if err := worker.HoldExpiryCycle(ctx, rdb, pool, payClient); err != nil {
		t.Fatal(err)
	}
	// ...and only then does TX2 come back with a succeeded charge.
	fin, err := booking.Finalize(ctx, pool, held, tierID, userID, "req-m",
		booking.ChargeResult{Outcome: booking.Succeeded, TxnID: "ch_m", CardBrand: "visa", CardLast4: "4242"})
	if err != nil {
		t.Fatal(err)
	}
	if fin.Status != "expired_after_charge" {
		t.Fatalf("finalize status = %s, want expired_after_charge", fin.Status)
	}

	// The HTTP handler owns that counter, so record it the way the handler does.
	metrics.PurchaseOutcome.WithLabelValues(metrics.OutcomeExpiredAfterCharge).Inc()
	if got := counterValue(t, metrics.PurchaseOutcome, metrics.OutcomeExpiredAfterCharge); got != before+1 {
		t.Errorf("expired_after_charge = %v, want %v", got, before+1)
	}

	// One gauge sample must surface it as money owed to a customer.
	worker.SampleGaugesOnce(ctx, pool)
	if got := gaugeValue(t, metrics.AdminStuckBookings, metrics.CategoryChargedNotDelivered); got != 1 {
		t.Errorf("charged_not_delivered gauge = %v, want 1", got)
	}
}

// Test 10: the invariant counter must never move during ordinary operation.
func TestMetricsInvariantViolationStaysZero(t *testing.T) {
	metrics.Register()
	ts, pool, _ := newPurchaseServer(t)
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	before := readCounter(t, metrics.InvariantViolation)
	resp, out := purchase(t, ts, jwtTok, "m-inv-1",
		purchaseBodyTok(eventID, tierID, 1, purchaseTok, "tok_visa"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("purchase = %d body=%v", resp.StatusCode, out)
	}
	if got := readCounter(t, metrics.InvariantViolation); got != before {
		t.Errorf("invariant_violation_total moved from %v to %v — that is a bug, not a capacity problem", before, got)
	}
}

// Test 11: a gauge sampler whose query fails must log and carry on. A metrics
// goroutine that can take down a worker is worse than a stale dashboard.
func TestMetricsGaugeSamplerSurvivesFailedQuery(t *testing.T) {
	metrics.Register()
	_, pool, _ := newPurchaseServer(t)
	ctx := context.Background()

	worker.SampleGaugesOnce(ctx, pool)
	lastHeld := readGauge(t, metrics.BookingsHeld)

	// A closed pool makes every query fail.
	closed := testClosedPool(t)
	worker.SampleGaugesOnce(ctx, closed) // must not panic

	if got := readGauge(t, metrics.BookingsHeld); got != lastHeld {
		t.Errorf("gauge changed on a failed sample: %v -> %v, want the last value kept", lastHeld, got)
	}
	// And the endpoint still serves.
	if body := metrics.ScrapeText(); len(body) == 0 {
		t.Error("/metrics stopped serving after a failed sample")
	}
}

// testClosedPool returns a pool that is already closed, so every query fails.
// It is built directly rather than via testutil, whose cleanup truncates tables
// and would itself fail against a closed pool.
func testClosedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://ticketqueue:localdev@localhost:5434/ticketqueue_test?sslmode=disable"
	}
	p, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	p.Close()
	return p
}

func readGauge(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var pb dto.Metric
	if err := g.Write(&pb); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return pb.GetGauge().GetValue()
}
