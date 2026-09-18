// Package metrics holds every Prometheus metric ticketqueue exposes, in one
// file, so names and labels stay consistent and cardinality stays reviewable.
//
// HELP strings say what a rising value MEANS operationally, not what the metric
// counts — these are read in Grafana at 2am, and in the README screenshot.
//
// Cardinality rule (§11a): never a booking id, user id, idempotency key, event
// id, or raw URL path as a label. Every label below has a small fixed value set,
// declared as constants so a typo fails a test instead of quietly creating a
// second empty series.
package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Purchase outcomes (purchase_outcome_total).
const (
	OutcomeConfirmed          = "confirmed"
	OutcomeDeclined           = "declined"
	OutcomeSoldOut            = "sold_out"
	OutcomeAmbiguous          = "ambiguous"
	OutcomeDuplicateBlocked   = "duplicate_blocked"
	OutcomeWindowExpired      = "window_expired"
	OutcomeExpiredAfterCharge = "expired_after_charge"
)

// PurchaseOutcomes is every value OutcomeLabel can take.
var PurchaseOutcomes = []string{
	OutcomeConfirmed, OutcomeDeclined, OutcomeSoldOut, OutcomeAmbiguous,
	OutcomeDuplicateBlocked, OutcomeWindowExpired, OutcomeExpiredAfterCharge,
}

// Purchase phases (purchase_phase_duration_seconds).
const (
	PhaseTX1         = "tx1"
	PhasePaymentCall = "payment_call"
	PhaseTX2         = "tx2"
)

var PurchasePhases = []string{PhaseTX1, PhasePaymentCall, PhaseTX2}

// Idempotency outcomes (idempotency_outcome_total).
const (
	IdemNew          = "new"
	IdemReplayed     = "replayed"
	IdemInProgress   = "in_progress"
	IdemHashMismatch = "hash_mismatch"
)

var IdempotencyOutcomes = []string{IdemNew, IdemReplayed, IdemInProgress, IdemHashMismatch}

// Worker names (worker_* metrics).
const (
	WorkerPromotion      = "promotion"
	WorkerHoldExpiry     = "hold_expiry"
	WorkerReconciliation = "reconciliation"
	WorkerDrift          = "drift"
	WorkerIdemCleanup    = "idem_cleanup"
)

var WorkerNames = []string{
	WorkerPromotion, WorkerHoldExpiry, WorkerReconciliation, WorkerDrift, WorkerIdemCleanup,
}

// Resolution sources (reconciliation_resolution_total). These are exactly the
// Actor.Source values the shared resolver is called with, so the audit trail and
// this metric agree by construction (§6). 'webhook' is declared ahead of the
// webhook endpoint so the label set never has to change later. 'purchase' is
// absent on purpose: the synchronous TX2 path does not go through the resolver.
const (
	SourceReconciliation = "reconciliation"
	SourceHoldExpiry     = "hold_expiry"
	SourceSyncRetry      = "sync_retry"
	SourceAdminManual    = "admin_manual"
	SourceWebhook        = "webhook"
)

var ResolutionSources = []string{
	SourceReconciliation, SourceHoldExpiry, SourceSyncRetry, SourceAdminManual, SourceWebhook,
}

// Resolution outcomes (reconciliation_resolution_total).
const (
	ResolutionConfirmed = "confirmed"
	ResolutionFailed    = "failed"
	ResolutionNoCharge  = "no_charge"
	ResolutionDeferred  = "deferred"
)

var ResolutionOutcomes = []string{
	ResolutionConfirmed, ResolutionFailed, ResolutionNoCharge, ResolutionDeferred,
}

// Stuck-booking categories (admin_stuck_bookings_current).
const (
	CategoryHeldTooLong         = "held_too_long"
	CategoryChargedNotDelivered = "charged_not_delivered"
)

var StuckCategories = []string{CategoryHeldTooLong, CategoryChargedNotDelivered}

// ---------------------------------------------------------------- API metrics

var (
	HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency. route is the chi route PATTERN, never the raw path, so ids never become labels. A rising P95 on one route with a flat purchase_phase_duration_seconds means the time is going somewhere other than the purchase flow.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method", "status"})

	// THE key metric (§14 never-cut): the Week 6 load test is uninterpretable
	// without it.
	PurchasePhase = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "purchase_phase_duration_seconds",
		Help:    "Time inside each phase of a purchase. tx1 rising means database contention on the ticket claim; payment_call rising means the provider is slow; tx2 rising means finalisation contention. This is what tells you WHERE latency went, not just that it went.",
		Buckets: prometheus.DefBuckets,
	}, []string{"phase"})

	PurchaseOutcome = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "purchase_outcome_total",
		Help: "How purchases ended. sold_out rising is normal during a drop; ambiguous rising means the provider is dropping responses and reconciliation is about to get busy; expired_after_charge is Outcome D (money moved, booking no longer held) and any non-zero value is money owed to a customer.",
	}, []string{"outcome"})

	TicketClaimRows = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "ticket_claim_rows_affected",
		Help:    "Rows actually claimed by the Layer-2 SKIP LOCKED select. Values below the requested quantity mean the Redis fast path raced ahead of Postgres truth; a spread here is the early warning for partial claims.",
		Buckets: []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
	})

	RedisLuaReject = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "redis_lua_reject_total",
		Help: "Purchases rejected by the Layer-1 Lua check before touching Postgres. During a sell-out this should dwarf the number of database claims — that ratio IS the proof the fast path is doing its job.",
	})

	IdempotencyOutcome = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "idempotency_outcome_total",
		Help: "How each idempotent request was handled. replayed rising means clients are retrying (normal after timeouts); in_progress rising means duplicate requests are arriving concurrently; hash_mismatch is a client bug — the same key sent with a different body.",
	}, []string{"outcome"})

	DBPoolInUse = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "db_pool_in_use",
		Help: "Connections currently checked out. Compare against db_pool_max: when this pins at max, the pool is the bottleneck and every added VU just queues.",
	})

	DBPoolIdle = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "db_pool_idle",
		Help: "Connections sitting idle. Falling to zero while db_pool_in_use pins at max is the signature of pool saturation.",
	})

	DBPoolMax = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "db_pool_max",
		Help: "Configured pool ceiling, exported so a dashboard can draw the saturation line without hardcoding it.",
	})

	// pgxpool only exposes cumulative totals, never per-acquire samples, so this
	// is a counter fed by deltas rather than a histogram. rate() of it in
	// Grafana gives seconds-waited-per-second, which is the pressure signal.
	DBPoolAcquireWaitSeconds = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "db_pool_acquire_wait_seconds_total",
		Help: "Total time spent waiting for a free connection. Take rate() of this: anything approaching 1 second-per-second means requests are spending as much time queueing for a connection as doing work.",
	})

	DBPoolEmptyAcquire = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "db_pool_empty_acquire_total",
		Help: "Acquires that had to wait because the pool was empty. The first of these during a load test marks the moment the pool became the constraint.",
	})
)

// ------------------------------------------------------------- Worker metrics

var (
	WorkerCycleDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "worker_cycle_duration_seconds",
		Help:    "How long one worker cycle took. A cycle duration approaching the worker's interval means it can no longer keep up with its own schedule.",
		Buckets: prometheus.DefBuckets,
	}, []string{"worker"})

	WorkerItems = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "worker_items_processed_total",
		Help: "Items a worker handled. Flat while bookings_held_current rises means the worker is not seeing the work it should be seeing.",
	}, []string{"worker"})

	WorkerErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "worker_errors_total",
		Help: "Errors inside worker cycles. A provider being unreachable is NOT counted here — that is a deferral, a normal outcome; this counter is for genuine failures.",
	}, []string{"worker"})

	// The operational story of the whole system in one metric.
	ReconciliationResolution = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "reconciliation_resolution_total",
		Help: "Stuck bookings resolved, by which path resolved them and how it ended. A spike in source=hold_expiry means reconciliation is falling behind and the 10-minute backstop is doing its job instead. A spike in outcome=deferred means the provider is in trouble and nothing is being guessed.",
	}, []string{"source", "outcome"})

	InventoryDrift = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "inventory_drift_detected_total",
		Help: "Times the Redis inventory counter disagreed with Postgres twice in a row and was corrected. Should sit at ~0; a rising value is a real finding worth investigating, not noise.",
	})

	InvariantViolation = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "invariant_violation_total",
		Help: "Structural invariant violations, e.g. a sold ticket on a non-confirmed booking. This MUST be zero. Any non-zero value is a bug in the system, not a capacity problem.",
	})

	BookingsHeld = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "bookings_held_current",
		Help: "Bookings currently sitting in 'held'. A steady small number is normal in-flight traffic; a rising line means a resolution path is falling behind or the payment provider is down.",
	})

	AdminStuckBookings = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "admin_stuck_bookings_current",
		Help: "Bookings on the admin stuck list. held_too_long means frozen inventory, which heals itself. charged_not_delivered above zero means MONEY IS OWED TO A CUSTOMER: in production this is the one gauge here worth paging a human for.",
	}, []string{"category"})
)

// ---------------------------------------------------- Payment service metrics

var (
	ChargeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "charge_duration_seconds",
		Help:    "Time the simulated provider took to answer a charge, by injection token. The slow tokens should show up here as the long tail they are meant to simulate.",
		Buckets: prometheus.DefBuckets,
	}, []string{"token_type"})

	ChargeOutcome = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "charge_outcome_total",
		Help: "Charge attempts by injection token and result. Compare against purchase_outcome_total: a charge the provider recorded as succeeded while the purchase ended ambiguous is exactly the dropped-response case reconciliation exists for.",
	}, []string{"token_type", "outcome"})
)

// Each binary registers only what it actually writes. Exposing everything
// everywhere would mean the API and the payment service both publishing
// bookings_held_current as a flat zero next to the worker's real value — three
// lines on a dashboard where only one means anything.
var (
	apiCollectors = []prometheus.Collector{
		HTTPDuration, PurchasePhase, PurchaseOutcome, TicketClaimRows, RedisLuaReject,
		IdempotencyOutcome, DBPoolInUse, DBPoolIdle, DBPoolMax, DBPoolAcquireWaitSeconds,
		DBPoolEmptyAcquire,
		// The API's sync-reconcile-on-retry path resolves bookings too.
		ReconciliationResolution,
	}

	workerCollectors = []prometheus.Collector{
		WorkerCycleDuration, WorkerItems, WorkerErrors, ReconciliationResolution,
		InventoryDrift, InvariantViolation, BookingsHeld, AdminStuckBookings,
		TicketClaimRows, RedisLuaReject,
	}

	paymentCollectors = []prometheus.Collector{ChargeDuration, ChargeOutcome}

	// allCollectors is every metric exactly once, for tests.
	allCollectors = dedupe(apiCollectors, workerCollectors, paymentCollectors)
)

func dedupe(groups ...[]prometheus.Collector) []prometheus.Collector {
	seen := map[prometheus.Collector]bool{}
	var out []prometheus.Collector
	for _, g := range groups {
		for _, c := range g {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// register is idempotent per collector rather than per call, so any mix of
// RegisterAPI/RegisterWorker/Register in any order is safe — a shared sync.Once
// would silently skip the second group.
func register(cs []prometheus.Collector) {
	for _, c := range cs {
		if err := prometheus.Register(c); err != nil {
			var already prometheus.AlreadyRegisteredError
			if errors.As(err, &already) {
				continue
			}
			panic(err) // a genuinely broken collector should fail loudly at startup
		}
	}
}

// RegisterAPI, RegisterWorker and RegisterPayments register the metrics that
// binary owns. Each is safe to call more than once.
func RegisterAPI()      { register(apiCollectors) }
func RegisterWorker()   { register(workerCollectors) }
func RegisterPayments() { register(paymentCollectors) }

// Register registers every metric. Tests use it; binaries use the specific one
// for what they own.
func Register() { register(allCollectors) }

// Handler serves the metrics endpoint. Registration is the binary's job, so a
// process that only owns some metrics does not publish the rest as flat zeroes.
//
// NEVER expose this publicly in the AWS deploy: it describes internal capacity
// and failure modes. Bind it to the VPC only (§11a).
func Handler() http.Handler {
	return promhttp.Handler()
}

// ScrapeText renders the registry exactly as Prometheus would scrape it. Useful
// in tests that want to assert on real exposition output rather than on
// in-process counters.
func ScrapeText() string {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// RouteLabel returns the chi route PATTERN for a request ("/v1/bookings/{id}"),
// never the raw path ("/v1/bookings/9f3e..."). Raw paths carry ids, and an id in
// a label is an unbounded time series.
func RouteLabel(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	return "unmatched"
}
