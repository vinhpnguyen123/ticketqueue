//go:build integration

package http_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/vinhpnguyen123/ticketqueue/internal/auth"
	"github.com/vinhpnguyen123/ticketqueue/internal/worker"
)

// TestPromotionToPositionEndToEnd: real worker cycle admits a waiting user,
// then GET position returns a purchase_token with the right subject and exp.
func TestPromotionToPositionEndToEnd(t *testing.T) {
	ts, pool, rdb := newQueueServer(t)
	eventID := seedEvent(t, pool, "Promo E2E", "on_sale", nil)
	userID, tok := registerUser(t, ts, "promo-e2e@test.dev")
	base := fmt.Sprintf("%s/v1/events/%s/queue", ts.URL, eventID)

	doQueue(t, http.MethodPost, base+"/join", tok, nil)

	if err := worker.PromoteCycle(context.Background(), rdb, pool); err != nil {
		t.Fatalf("promote cycle: %v", err)
	}

	var pos queuePositionBody
	resp := doQueue(t, http.MethodGet, base+"/position", tok, &pos)
	if resp.StatusCode != http.StatusOK || pos.Data.Status != "admitted" {
		t.Fatalf("status=%d body=%+v, want 200 admitted", resp.StatusCode, pos.Data)
	}

	claims, err := auth.ParseToken(pos.Data.PurchaseToken, testSecret)
	if err != nil {
		t.Fatalf("purchase token does not parse: %v", err)
	}
	if claims.Subject != userID {
		t.Errorf("purchase token sub = %q, want %q", claims.Subject, userID)
	}
	// Default purchase_window_seconds is 300: exp must be ~admitted_at+300s.
	exp := claims.ExpiresAt.Time
	if until := time.Until(exp); until < 4*time.Minute || until > 5*time.Minute+10*time.Second {
		t.Errorf("token exp %v (in %v), want ~5 minutes out", exp, until)
	}
}
