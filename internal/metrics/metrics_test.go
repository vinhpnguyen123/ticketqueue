package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Test 1: everything registers, and nothing is declared twice.
func TestRegisterIsSafeAndComplete(t *testing.T) {
	// Any order, any mix, repeated: registration must never panic and must
	// never leave a later group unregistered.
	RegisterAPI()
	RegisterWorker()
	RegisterPayments()
	Register()
	Register()

	seen := map[string]bool{}
	for _, c := range allCollectors {
		descs := make(chan *prometheus.Desc, 16)
		go func(c prometheus.Collector) {
			c.Describe(descs)
			close(descs)
		}(c)
		for d := range descs {
			if seen[d.String()] {
				t.Errorf("collector declared twice: %s", d)
			}
			seen[d.String()] = true
		}
	}
	if len(seen) < len(allCollectors) {
		t.Errorf("described %d descs for %d collectors", len(seen), len(allCollectors))
	}
}

// Test 2: the route label is the chi PATTERN, never the raw path — an id in a
// label is an unbounded time series.
func TestRouteLabelUsesPattern(t *testing.T) {
	var got string
	r := chi.NewRouter()
	r.Get("/v1/bookings/{id}", func(w http.ResponseWriter, r *http.Request) {
		got = RouteLabel(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/bookings/9f3e7c21-0000-4000-8000-000000000000", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)

	if got != "/v1/bookings/{id}" {
		t.Errorf("route label = %q, want the pattern %q", got, "/v1/bookings/{id}")
	}
	if strings.Contains(got, "9f3e7c21") {
		t.Errorf("route label contains a request id: %q", got)
	}

	// An unmatched request must still produce a bounded label.
	unmatched := httptest.NewRequest(http.MethodGet, "/nope", nil)
	if l := RouteLabel(unmatched); l != "unmatched" {
		t.Errorf("unmatched route label = %q, want \"unmatched\"", l)
	}
}

// Test 3: every declared label value is exercised, so a typo in a constant
// shows up here instead of silently creating an empty series in Grafana.
func TestEveryLabelValueIsUsable(t *testing.T) {
	Register()

	for _, v := range PurchaseOutcomes {
		PurchaseOutcome.WithLabelValues(v).Add(0)
	}
	for _, v := range PurchasePhases {
		PurchasePhase.WithLabelValues(v).Observe(0)
	}
	for _, v := range IdempotencyOutcomes {
		IdempotencyOutcome.WithLabelValues(v).Add(0)
	}
	for _, v := range WorkerNames {
		WorkerCycleDuration.WithLabelValues(v).Observe(0)
		WorkerItems.WithLabelValues(v).Add(0)
		WorkerErrors.WithLabelValues(v).Add(0)
	}
	for _, source := range ResolutionSources {
		for _, outcome := range ResolutionOutcomes {
			ReconciliationResolution.WithLabelValues(source, outcome).Add(0)
		}
	}
	for _, v := range StuckCategories {
		AdminStuckBookings.WithLabelValues(v).Set(0)
	}

	if got := countSeries(t, PurchaseOutcome); got != len(PurchaseOutcomes) {
		t.Errorf("purchase_outcome_total series = %d, want %d", got, len(PurchaseOutcomes))
	}
	want := len(ResolutionSources) * len(ResolutionOutcomes)
	if got := countSeries(t, ReconciliationResolution); got != want {
		t.Errorf("reconciliation_resolution_total series = %d, want %d", got, want)
	}
}

func countSeries(t *testing.T, c prometheus.Collector) int {
	t.Helper()
	ch := make(chan prometheus.Metric, 128)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	n := 0
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		n++
	}
	return n
}
