package booking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PaymentClient talks to the (simulated) payment provider.
type PaymentClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func NewPaymentClient(baseURL, apiKey string) *PaymentClient {
	// 3s timeout: shorter than the provider's slow-token delays on purpose —
	// timing out against a still-processing charge is the ambiguity the
	// reconciliation flow exists to resolve.
	return &PaymentClient{BaseURL: baseURL, APIKey: apiKey, HTTP: &http.Client{Timeout: 3 * time.Second}}
}

// Charge POSTs to the provider with the booking's idempotency key.
// A non-nil error means we don't know the outcome (timeout, dropped
// response); a nil error with empty txnID means the provider answered
// with a non-charge response.
func (c *PaymentClient) Charge(ctx context.Context, idemKey string, amountCents int64, currency, paymentToken string) (txnID string, err error) {
	body, _ := json.Marshal(map[string]any{
		"amount":        amountCents,
		"currency":      currency,
		"payment_token": paymentToken,
		"description":   "ticketqueue booking",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/charges", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Idempotency-Key", idemKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("charge request: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode charge response (status %d): %w", resp.StatusCode, err)
	}
	return out.ID, nil
}

// InitiatePayment is the WEEK4 temporary seam: call the provider, record an
// 'initiated' payments row, and stop — every outcome (success, decline,
// timeout, dropped response) is treated the same. TX2 replaces this with
// real finalization: confirmed/failed booking, payments status, sold tickets.
func InitiatePayment(ctx context.Context, pool *pgxpool.Pool, pc *PaymentClient, held Held, idemKey, paymentToken string) {
	txnID, err := pc.Charge(ctx, idemKey, held.TotalCents, held.Currency, paymentToken)
	if err != nil {
		// Outcome unknown — exactly the ambiguity reconciliation resolves.
		// Never log the payment token itself.
		slog.Warn("payment outcome unknown", "booking_id", held.BookingID, "error", err)
	}
	var txn *string
	if txnID != "" {
		txn = &txnID
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO payments (booking_id, provider, provider_txn_id, status, amount_cents, currency)
		 VALUES ($1, 'simulated', $2, 'initiated', $3, $4)`,
		held.BookingID, txn, held.TotalCents, held.Currency); err != nil {
		slog.Error("insert payments row", "booking_id", held.BookingID, "error", err)
	}
}
