package paymentsim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testKey = "ps_test_localdev"

func init() {
	slow5s = 10 * time.Millisecond
	slow30s = 20 * time.Millisecond
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(NewServer(testKey))
	t.Cleanup(ts.Close)
	return ts
}

// postCharge sends a charge request and decodes the JSON body (if any).
func postCharge(t *testing.T, ts *httptest.Server, idemKey, token string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := chargeRaw(ts, testKey, idemKey, token)
	if err != nil {
		t.Fatalf("charge request: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode body %q: %v", raw, err)
		}
	}
	return resp, body
}

func chargeRaw(ts *httptest.Server, apiKey, idemKey, token string) (*http.Response, error) {
	payload := fmt.Sprintf(`{"amount": 5000, "currency": "USD", "payment_token": %q, "description": "test"}`, token)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/charges", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return http.DefaultClient.Do(req)
}

func lookupByKey(t *testing.T, ts *httptest.Server, key string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/charges/by-idempotency-key/"+key, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode lookup body: %v", err)
	}
	return resp, body
}

func TestSuccessTokens(t *testing.T) {
	ts := newTestServer(t)
	for _, tc := range []struct{ token, brand, last4 string }{
		{"tok_visa", "visa", "4242"},
		{"tok_mastercard", "mastercard", "4444"},
		{"tok_slow_5s", "visa", "4242"},
		{"tok_slow_30s", "visa", "4242"},
	} {
		resp, body := postCharge(t, ts, "key-"+tc.token, tc.token)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d, want 200", tc.token, resp.StatusCode)
			continue
		}
		if body["status"] != "succeeded" {
			t.Errorf("%s: status field %v, want succeeded", tc.token, body["status"])
		}
		if id, _ := body["id"].(string); !strings.HasPrefix(id, "ch_") || len(id) != 3+16 {
			t.Errorf("%s: bad charge id %v", tc.token, body["id"])
		}
		c := body["card"].(map[string]any)
		if c["brand"] != tc.brand || c["last4"] != tc.last4 {
			t.Errorf("%s: card %v, want %s/%s", tc.token, c, tc.brand, tc.last4)
		}
		if body["amount"].(float64) != 5000 || body["currency"] != "USD" {
			t.Errorf("%s: amount/currency %v/%v", tc.token, body["amount"], body["currency"])
		}
	}
}

func TestFailureTokens(t *testing.T) {
	ts := newTestServer(t)
	for _, tc := range []struct{ token, failureCode string }{
		{"tok_declined", "card_declined"},
		{"tok_insufficient_funds", "insufficient_funds"},
	} {
		resp, body := postCharge(t, ts, "key-"+tc.token, tc.token)
		if resp.StatusCode != http.StatusPaymentRequired {
			t.Errorf("%s: status %d, want 402", tc.token, resp.StatusCode)
		}
		if body["status"] != "failed" || body["failure_code"] != tc.failureCode {
			t.Errorf("%s: got status=%v code=%v", tc.token, body["status"], body["failure_code"])
		}
		// Failed charges are stored too — reconciliation must see them.
		if resp, stored := lookupByKey(t, ts, "key-"+tc.token); resp.StatusCode != http.StatusOK || stored["status"] != "failed" {
			t.Errorf("%s: stored lookup = %d %v, want 200 failed", tc.token, resp.StatusCode, stored["status"])
		}
	}
}

func TestUnknownToken(t *testing.T) {
	ts := newTestServer(t)
	resp, body := postCharge(t, ts, "key-unknown", "tok_bogus")
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", resp.StatusCode)
	}
	if body["error"].(map[string]any)["type"] != "invalid_payment_token" {
		t.Errorf("error type = %v, want invalid_payment_token", body["error"])
	}
}

func TestProviderIdempotency(t *testing.T) {
	ts := newTestServer(t)
	_, first := postCharge(t, ts, "idem-1", "tok_visa")
	resp, second := postCharge(t, ts, "idem-1", "tok_visa")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status %d, want 200", resp.StatusCode)
	}
	if first["id"] != second["id"] || first["created_at"] != second["created_at"] {
		t.Errorf("replay returned a different charge: %v vs %v", first["id"], second["id"])
	}
}

func TestMissingIdempotencyKey(t *testing.T) {
	ts := newTestServer(t)
	resp, body := postCharge(t, ts, "", "tok_visa")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if body["error"].(map[string]any)["type"] != "missing_idempotency_key" {
		t.Errorf("error type = %v", body["error"])
	}
}

func TestBadAuth(t *testing.T) {
	ts := newTestServer(t)
	resp, err := chargeRaw(ts, "wrong-key", "key-1", "tok_visa")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}

func TestLookupByKey(t *testing.T) {
	ts := newTestServer(t)
	postCharge(t, ts, "look-1", "tok_visa")

	resp, body := lookupByKey(t, ts, "look-1")
	if resp.StatusCode != http.StatusOK || body["status"] != "succeeded" || body["idempotency_key"] != "look-1" {
		t.Errorf("found lookup = %d %v", resp.StatusCode, body)
	}

	resp, body = lookupByKey(t, ts, "no-such-key")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing lookup status %d, want 404", resp.StatusCode)
	}
	if body["error"].(map[string]any)["type"] != "charge_not_found" {
		t.Errorf("error type = %v, want charge_not_found", body["error"])
	}
}

func Test500ThenSucceed(t *testing.T) {
	ts := newTestServer(t)

	resp, _ := postCharge(t, ts, "retry-1", "tok_500_then_succeed")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("first call status %d, want 500", resp.StatusCode)
	}
	// The 500 stored nothing — provider failed before charging.
	if resp, _ := lookupByKey(t, ts, "retry-1"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after 500, lookup = %d, want 404", resp.StatusCode)
	}

	resp, body := postCharge(t, ts, "retry-1", "tok_500_then_succeed")
	if resp.StatusCode != http.StatusOK || body["status"] != "succeeded" {
		t.Fatalf("retry = %d %v, want 200 succeeded", resp.StatusCode, body["status"])
	}
}

func TestDropsResponse(t *testing.T) {
	ts := newTestServer(t)

	// Client must see a transport-level failure, not an HTTP response.
	resp, err := chargeRaw(ts, testKey, "dropped-1", "tok_drops_response")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected connection error, got HTTP %d", resp.StatusCode)
	}

	// But the money moved: the charge is retrievable by key. This is the
	// reconciliation demo scenario.
	lresp, body := lookupByKey(t, ts, "dropped-1")
	if lresp.StatusCode != http.StatusOK || body["status"] != "succeeded" {
		t.Fatalf("post-drop lookup = %d %v, want 200 succeeded", lresp.StatusCode, body["status"])
	}
}
