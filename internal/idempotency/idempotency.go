// Package idempotency implements HTTP-level request dedup backed by the
// idempotency_keys table: atomic claim via ON CONFLICT, cached-response
// replay, and in-progress serialization. This is the HTTP-level mechanism —
// distinct from booking.idempotency_key, the cross-system bridge to the
// payment provider.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/ticketqueue/internal/auth"
	"github.com/vinhpnguyen123/ticketqueue/internal/http/response"
)

// lockTimeout is how long an uncompleted claim is considered in-flight.
// Older than this and the original attempt is presumed dead.
const lockTimeout = 30 * time.Second

type ctxKey int

const bookingIDKey ctxKey = iota

// bookingIDHolder lets the handler pass its booking ID back up to the
// middleware, which persists it on the idempotency row.
type bookingIDHolder struct{ id *string }

// SetBookingID records the booking created by this request so the
// idempotency row links to it. Call from the handler after TX1 commits.
func SetBookingID(r *http.Request, bookingID string) {
	if h, ok := r.Context().Value(bookingIDKey).(*bookingIDHolder); ok {
		h.id = &bookingID
	}
}

// recorder buffers the handler's status and body so the middleware can
// persist them for replay.
type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (rec *recorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	rec.buf.Write(b)
	return rec.ResponseWriter.Write(b)
}

// Middleware wraps a state-creating endpoint with idempotency-key dedup.
// Must run after auth.Middleware (it needs the user ID).
func Middleware(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := auth.UserIDFromContext(r.Context())
			if !ok {
				response.Error(w, r, http.StatusUnauthorized, "unauthorized", "authentication required")
				return
			}

			key := r.Header.Get("Idempotency-Key")
			if key == "" || len(key) > 255 {
				response.Error(w, r, http.StatusBadRequest, "missing_idempotency_key",
					"Idempotency-Key header is required (1-255 chars)")
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				response.Error(w, r, http.StatusBadRequest, "invalid_request", "cannot read request body")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body)) // re-buffer for the handler
			sum := sha256.Sum256(body)
			requestHash := hex.EncodeToString(sum[:])

			// Atomic claim: exactly one concurrent request with this (key, user)
			// inserts; everyone else conflicts and branches on the stored row.
			tag, err := pool.Exec(r.Context(),
				`INSERT INTO idempotency_keys (key, user_id, request_hash, locked_at, expires_at)
				 VALUES ($1, $2, $3, NOW(), NOW() + INTERVAL '24 hours')
				 ON CONFLICT (key, user_id) DO NOTHING`,
				key, userID, requestHash)
			if err != nil {
				slog.Error("idempotency claim failed", "error", err)
				response.Error(w, r, http.StatusInternalServerError, "internal_error", "idempotency store unavailable")
				return
			}

			if tag.RowsAffected() == 0 {
				replayOrReject(w, r, pool, key, userID, requestHash)
				return
			}

			// We own the claim: run the handler, capturing its response.
			holder := &bookingIDHolder{}
			rec := &recorder{ResponseWriter: w}
			ctx := r.Context()
			next.ServeHTTP(rec, r.WithContext(context.WithValue(ctx, bookingIDKey, holder)))

			// response_body is JSONB: empty bytes are not valid JSON, store NULL.
			var respBody []byte
			if rec.buf.Len() > 0 {
				respBody = rec.buf.Bytes()
			}
			if _, err := pool.Exec(ctx,
				`UPDATE idempotency_keys
				 SET response_status = $3, response_body = $4, booking_id = $5, completed_at = NOW()
				 WHERE key = $1 AND user_id = $2`,
				key, userID, rec.status, respBody, holder.id); err != nil {
				// Response already went to the client; the row stays uncompleted
				// and a retry lands on the stale-lock branch (request_unresolved).
				slog.Error("idempotency outcome persist failed", "key", key, "error", err)
			}
		})
	}
}

func replayOrReject(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, key, userID, requestHash string) {
	var (
		storedHash  string
		completedAt *time.Time
		lockedAt    time.Time
		status      *int
		respBody    []byte
	)
	err := pool.QueryRow(r.Context(),
		`SELECT request_hash, completed_at, locked_at, response_status, response_body
		 FROM idempotency_keys WHERE key = $1 AND user_id = $2`,
		key, userID,
	).Scan(&storedHash, &completedAt, &lockedAt, &status, &respBody)
	if err != nil {
		slog.Error("idempotency lookup failed", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal_error", "idempotency store unavailable")
		return
	}

	switch {
	case storedHash != requestHash:
		response.Error(w, r, http.StatusUnprocessableEntity, "idempotency_key_reuse",
			"idempotency key was used for a different request")
	case completedAt == nil && time.Since(lockedAt) <= lockTimeout:
		response.Error(w, r, http.StatusConflict, "request_in_progress",
			"a request with this idempotency key is already in progress")
	case completedAt == nil:
		// Stale lock: the previous attempt died mid-flight. Do NOT take over
		// and re-execute — the purchase may be half-done (booking held, charge
		// outcome unknown). The reconciliation worker (Week 4) resolves the
		// underlying booking; the client should check booking status.
		response.Error(w, r, http.StatusConflict, "request_unresolved",
			"a previous attempt with this key did not complete; check your booking status")
	default:
		// Replay the stored response verbatim, no handler invocation.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotency-Replayed", "true")
		w.WriteHeader(*status)
		if _, err := w.Write(respBody); err != nil {
			slog.Error("idempotency replay write failed", "error", err)
		}
	}
}
