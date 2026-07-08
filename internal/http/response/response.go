// Package response defines the standard JSON envelopes used by every
// ticketqueue endpoint: {"data": ...} for success and {"error": ...}
// for failures, both carrying the chi request ID.
package response

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

type dataEnvelope struct {
	Data      any    `json:"data"`
	RequestID string `json:"request_id"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Type      string         `json:"type"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Details   map[string]any `json:"details,omitempty"`
}

// JSON writes a success envelope: {"data": ..., "request_id": "..."}.
func JSON(w http.ResponseWriter, r *http.Request, status int, data any) {
	write(w, r, status, dataEnvelope{
		Data:      data,
		RequestID: middleware.GetReqID(r.Context()),
	})
}

// Error writes the standard error envelope:
// {"error": {"type": "...", "message": "...", "request_id": "..."}}.
func Error(w http.ResponseWriter, r *http.Request, status int, errType, message string) {
	ErrorWithDetails(w, r, status, errType, message, nil)
}

// ErrorWithDetails is Error with an extra details map (omitted when nil).
func ErrorWithDetails(w http.ResponseWriter, r *http.Request, status int, errType, message string, details map[string]any) {
	write(w, r, status, errorEnvelope{Error: errorBody{
		Type:      errType,
		Message:   message,
		RequestID: middleware.GetReqID(r.Context()),
		Details:   details,
	}})
}

func write(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("encode response", "error", err, "request_id", middleware.GetReqID(r.Context()))
	}
}
