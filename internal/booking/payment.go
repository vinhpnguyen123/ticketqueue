package booking

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"
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

// Outcome classifies a charge attempt into the three cases TX2 branches on.
type Outcome int

const (
	// Ambiguous: we don't know if money moved (timeout, dropped connection,
	// 5xx, unparseable body). Never guess — leave the booking held for
	// reconciliation. This is the zero value on purpose: any early return
	// without a definitive provider answer is ambiguous.
	Ambiguous Outcome = iota
	Succeeded          // provider confirmed the charge (HTTP 200, status succeeded)
	Declined           // provider refused the card (HTTP 402, status failed)
)

// ChargeResult is everything TX2 needs to finalize, with the outcome already
// classified so the money-path branching lives in one place.
type ChargeResult struct {
	Outcome     Outcome
	TxnID       string
	CardBrand   string
	CardLast4   string
	FailureCode string
	Raw         []byte // provider's raw JSON body → payments.provider_response
}

// Charge POSTs to the provider with the booking's idempotency key and
// classifies the result. It never returns an error: an unreachable or
// ambiguous provider is a valid, expected outcome (Ambiguous), not a bug.
func (c *PaymentClient) Charge(ctx context.Context, idemKey string, amountCents int64, currency, paymentToken string) ChargeResult {
	body, _ := json.Marshal(map[string]any{
		"amount":        amountCents,
		"currency":      currency,
		"payment_token": paymentToken,
		"description":   "ticketqueue booking",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/charges", bytes.NewReader(body))
	if err != nil {
		return ChargeResult{Outcome: Ambiguous}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Idempotency-Key", idemKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Timeout / dropped connection / reset: money may or may not have
		// moved. Never log the payment token. Reconciliation resolves this.
		slog.Warn("payment outcome unknown", "error", err)
		return ChargeResult{Outcome: Ambiguous}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var ch struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		FailureCode string `json:"failure_code"`
		Card        struct {
			Brand string `json:"brand"`
			Last4 string `json:"last4"`
		} `json:"card"`
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if json.Unmarshal(raw, &ch) != nil || ch.Status != "succeeded" {
			return ChargeResult{Outcome: Ambiguous, Raw: raw}
		}
		return ChargeResult{Outcome: Succeeded, TxnID: ch.ID,
			CardBrand: ch.Card.Brand, CardLast4: ch.Card.Last4, Raw: raw}
	case http.StatusPaymentRequired:
		_ = json.Unmarshal(raw, &ch)
		return ChargeResult{Outcome: Declined, TxnID: ch.ID, FailureCode: ch.FailureCode, Raw: raw}
	default:
		// 500 and anything else: no idempotency guarantee that a charge
		// exists. Treat as ambiguous (case D); reconciliation will find the
		// provider has no record and fail it cleanly.
		return ChargeResult{Outcome: Ambiguous, Raw: raw}
	}
}
