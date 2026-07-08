package response

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

func newRequest(requestID string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/test", nil)
	ctx := context.WithValue(r.Context(), middleware.RequestIDKey, requestID)
	return r.WithContext(ctx)
}

func TestJSON(t *testing.T) {
	w := httptest.NewRecorder()
	JSON(w, newRequest("req-123"), http.StatusCreated, map[string]string{"hello": "world"})

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}

	var body struct {
		Data      map[string]string `json:"data"`
		RequestID string            `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data["hello"] != "world" {
		t.Errorf("data = %v", body.Data)
	}
	if body.RequestID != "req-123" {
		t.Errorf("request_id = %q, want req-123", body.RequestID)
	}
}

func TestError(t *testing.T) {
	w := httptest.NewRecorder()
	Error(w, newRequest("req-456"), http.StatusNotFound, "not_found", "booking not found")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}

	var body struct {
		Error struct {
			Type      string         `json:"type"`
			Message   string         `json:"message"`
			RequestID string         `json:"request_id"`
			Details   map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Type != "not_found" || body.Error.Message != "booking not found" {
		t.Errorf("error body = %+v", body.Error)
	}
	if body.Error.RequestID != "req-456" {
		t.Errorf("request_id = %q, want req-456", body.Error.RequestID)
	}
	if body.Error.Details != nil {
		t.Errorf("details should be omitted, got %v", body.Error.Details)
	}
}

func TestErrorOmitsDetailsKey(t *testing.T) {
	w := httptest.NewRecorder()
	Error(w, newRequest("x"), http.StatusBadRequest, "validation_error", "bad input")

	var raw map[string]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["error"]["details"]; ok {
		t.Error("details key should be absent when nil")
	}
}

func TestErrorWithDetails(t *testing.T) {
	w := httptest.NewRecorder()
	ErrorWithDetails(w, newRequest("req-789"), http.StatusUnprocessableEntity,
		"validation_error", "invalid fields", map[string]any{"quantity": "must be > 0"})

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}

	var body struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Details["quantity"] != "must be > 0" {
		t.Errorf("details = %v", body.Error.Details)
	}
}

func TestJSONWithoutRequestID(t *testing.T) {
	w := httptest.NewRecorder()
	JSON(w, httptest.NewRequest(http.MethodGet, "/test", nil), http.StatusOK, nil)

	var body struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.RequestID != "" {
		t.Errorf("request_id = %q, want empty", body.RequestID)
	}
}
