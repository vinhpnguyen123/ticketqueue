// Package http contains the router, handlers, and middleware for the API server.
package http

import "net/http"

// HealthHandler reports service liveness.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}
