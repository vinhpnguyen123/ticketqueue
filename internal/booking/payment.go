package booking

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	Succeeded         // provider confirmed the charge (HTTP 200, status succeeded)
	Declined          // provider refused the card (HTTP 402, status failed)
	// NotFound: the provider has no charge under this idempotency key, so no
	// money moved. Only a lookup can produce it — a charge attempt cannot.
	NotFound
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

	var ch chargeBody

	switch resp.StatusCode {
	case http.StatusOK:
		if json.Unmarshal(raw, &ch) != nil || ch.Status != "succeeded" {
			return ChargeResult{Outcome: Ambiguous, Raw: raw}
		}
		return ch.result(Succeeded, raw)
	case http.StatusPaymentRequired:
		_ = json.Unmarshal(raw, &ch)
		return ch.result(Declined, raw)
	default:
		// 500 and anything else: no idempotency guarantee that a charge
		// exists. Treat as ambiguous (case D); reconciliation will find the
		// provider has no record and fail it cleanly.
		return ChargeResult{Outcome: Ambiguous, Raw: raw}
	}
}

// Lookup asks the provider what happened under an idempotency key. This is the
// reconciliation bridge: after an ambiguous charge, it is the ONLY source of
// truth about whether money moved. It is side-effect-free, which is why two
// workers racing on the same booking is harmless.
//
// Outcomes: Succeeded / Declined (the provider has a record), NotFound (it has
// none — no money moved, safe to fail the booking), Ambiguous (we could not ask
// — never guess, retry next cycle).
func (c *PaymentClient) Lookup(ctx context.Context, idemKey string) ChargeResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/v1/charges/by-idempotency-key/"+url.PathEscape(idemKey), nil)
	if err != nil {
		return ChargeResult{Outcome: Ambiguous}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return ChargeResult{Outcome: Ambiguous}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		var ch chargeBody
		if json.Unmarshal(raw, &ch) != nil {
			return ChargeResult{Outcome: Ambiguous, Raw: raw}
		}
		switch ch.Status {
		case "succeeded":
			return ch.result(Succeeded, raw)
		case "failed":
			return ch.result(Declined, raw)
		default:
			// A status we don't understand is not a licence to guess.
			return ChargeResult{Outcome: Ambiguous, Raw: raw}
		}
	case http.StatusNotFound:
		return ChargeResult{Outcome: NotFound, Raw: raw}
	default:
		// 401/5xx: we did not learn anything. Defer.
		return ChargeResult{Outcome: Ambiguous, Raw: raw}
	}
}

// chargeBody is the provider's charge representation, shared by POST /charges
// and the by-key lookup (same JSON shape on both endpoints).
type chargeBody struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	FailureCode string `json:"failure_code"`
	Card        struct {
		Brand string `json:"brand"`
		Last4 string `json:"last4"`
	} `json:"card"`
}

func (ch chargeBody) result(o Outcome, raw []byte) ChargeResult {
	return ChargeResult{Outcome: o, TxnID: ch.ID, FailureCode: ch.FailureCode,
		CardBrand: ch.Card.Brand, CardLast4: ch.Card.Last4, Raw: raw}
}
