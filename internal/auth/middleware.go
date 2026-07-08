package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/vinhpnguyen123/ticketqueue/internal/http/response"
)

type ctxKey int

const (
	userIDKey ctxKey = iota
	isAdminKey
)

// UserIDFromContext returns the authenticated user's ID set by Middleware.
func UserIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(userIDKey).(string)
	return id, ok
}

// IsAdminFromContext returns the authenticated user's admin flag set by Middleware.
func IsAdminFromContext(ctx context.Context) (bool, bool) {
	isAdmin, ok := ctx.Value(isAdminKey).(bool)
	return isAdmin, ok
}

// Middleware validates the Authorization: Bearer token and injects the
// user ID and admin flag into the request context. Missing, invalid, or
// expired tokens get 401 with error type "unauthorized".
func Middleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			tokenString, ok := strings.CutPrefix(header, "Bearer ")
			if !ok || tokenString == "" {
				response.Error(w, r, http.StatusUnauthorized, "unauthorized", "missing bearer token")
				return
			}

			claims, err := ParseToken(tokenString, secret)
			if err != nil {
				msg := "invalid token"
				if errors.Is(err, ErrTokenExpired) {
					msg = "token expired"
				}
				response.Error(w, r, http.StatusUnauthorized, "unauthorized", msg)
				return
			}

			ctx := context.WithValue(r.Context(), userIDKey, claims.Subject)
			ctx = context.WithValue(ctx, isAdminKey, claims.IsAdmin)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
