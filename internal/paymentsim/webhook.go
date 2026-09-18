package paymentsim

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// Option configures the simulated provider. Variadic so the six existing
// NewServer callers keep working unchanged.
type Option func(*server)

// WithWebhook makes the provider notify ticketqueue when a charge settles.
// Optional on purpose: reconciliation is already correct without it, and the
// webhook is only a latency optimisation (§14 #1, first thing cut).
func WithWebhook(url, secret string) Option {
	return func(s *server) {
		s.webhookURL = url
		s.webhookSecret = secret
		s.webhookClient = &http.Client{Timeout: 3 * time.Second}
	}
}

// webhookRetries is how many times a delivery is attempted before giving up.
// Giving up is safe: the reconciliation worker resolves the booking anyway, a
// bit later. The webhook is never the only path.
const webhookRetries = 3

type webhookPayload struct {
	EventID        string    `json:"event_id"`
	Type           string    `json:"type"`
	IdempotencyKey string    `json:"idempotency_key"`
	ChargeID       string    `json:"charge_id"`
	Amount         int64     `json:"amount"`
	Currency       string    `json:"currency"`
	CreatedAt      time.Time `json:"created_at"`
}

// notify delivers a settled charge, in the background.
//
// Fire-and-forget by contract: it must never block or delay the charge
// response, and must never change what the client sees — tok_drops_response
// still drops, and the webhook still fires, because the money really did move.
//
// Called AFTER the response is written. Firing before would let the webhook
// reach ticketqueue ahead of its own TX2 and make the lost-guard path routine
// on every successful purchase; the guarded UPDATE handles that correctly, but
// there is no reason to provoke it on every transaction.
func (s *server) notify(ch charge) {
	if s.webhookURL == "" {
		return
	}

	typ := "charge.succeeded"
	if ch.Status != "succeeded" {
		typ = "charge.failed"
	}
	body, err := json.Marshal(webhookPayload{
		EventID:        uuid.NewString(),
		Type:           typ,
		IdempotencyKey: ch.IdempotencyKey,
		ChargeID:       ch.ID,
		Amount:         ch.Amount,
		Currency:       ch.Currency,
		CreatedAt:      ch.CreatedAt,
	})
	if err != nil {
		slog.Error("webhook marshal failed", "error", err)
		return
	}

	go s.deliver(body)
}

func (s *server) deliver(body []byte) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := Sign(body, s.webhookSecret)

	backoff := 100 * time.Millisecond
	for attempt := 1; attempt <= webhookRetries; attempt++ {
		req, err := http.NewRequest(http.MethodPost, s.webhookURL, bytes.NewReader(body))
		if err != nil {
			slog.Error("webhook request build failed", "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Webhook-Signature", sig)
		req.Header.Set("X-Webhook-Timestamp", ts)

		resp, err := s.webhookClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		slog.Warn("webhook delivery failed", "attempt", attempt, "error", err)
		if attempt < webhookRetries {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	// Giving up is not a lost outcome: reconciliation will resolve the booking.
	slog.Error("webhook delivery gave up; reconciliation will resolve the booking")
}

// Sign produces the X-Webhook-Signature value for a raw body. Exported so the
// receiver's tests can build a genuinely valid signature instead of copying the
// algorithm and drifting from it.
func Sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
