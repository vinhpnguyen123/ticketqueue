//go:build integration

package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// CounterValue reads one labelled counter's current value.
func CounterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	var pb dto.Metric
	if err := vec.WithLabelValues(labels...).Write(&pb); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return pb.GetCounter().GetValue()
}

// GaugeValue reads one labelled gauge's current value.
func GaugeValue(t *testing.T, vec *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	var pb dto.Metric
	if err := vec.WithLabelValues(labels...).Write(&pb); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return pb.GetGauge().GetValue()
}

// HistogramCount reads how many observations a labelled histogram has.
func HistogramCount(t *testing.T, vec *prometheus.HistogramVec, labels ...string) uint64 {
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

// TestScrapeExposesEveryMetric is the smoke test for the endpoint itself: the
// handler serves, and every metric name appears with its HELP line.
func TestScrapeExposesEveryMetric(t *testing.T) {
	Register()

	// A *Vec emits nothing at all until it has at least one series, so route-
	// and token-labelled metrics only appear once traffic has touched them.
	// Their label values are open-ended (any chi pattern, any injection token),
	// so they are not in the constant sets TestEveryLabelValueIsUsable walks.
	HTTPDuration.WithLabelValues("/v1/bookings/{id}", "GET", "200").Observe(0)
	ChargeDuration.WithLabelValues("tok_visa").Observe(0)

	body := ScrapeText()

	for _, name := range []string{
		"http_request_duration_seconds", "purchase_phase_duration_seconds",
		"purchase_outcome_total", "ticket_claim_rows_affected", "redis_lua_reject_total",
		"idempotency_outcome_total", "db_pool_in_use", "db_pool_acquire_wait_seconds_total",
		"worker_cycle_duration_seconds", "reconciliation_resolution_total",
		"bookings_held_current", "admin_stuck_bookings_current", "invariant_violation_total",
		"charge_duration_seconds",
	} {
		if !strings.Contains(body, "# HELP "+name+" ") {
			t.Errorf("metric %s has no HELP line in the scrape", name)
		}
	}
	if strings.Contains(body, "booking_id=") || strings.Contains(body, "user_id=") {
		t.Error("scrape contains a high-cardinality label")
	}
}
