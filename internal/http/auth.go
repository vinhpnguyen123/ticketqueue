package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/ticketqueue/internal/auth"
	"github.com/vinhpnguyen123/ticketqueue/internal/domain/user"
	"github.com/vinhpnguyen123/ticketqueue/internal/http/response"
)

// TokenTTL is how long issued JWTs stay valid.
const TokenTTL = time.Hour

// zeroUUID is used as audit entity_id when no user exists (unknown-email login attempts).
const zeroUUID = "00000000-0000-0000-0000-000000000000"

// fakeHash is a valid argon2id hash verified against on unknown-email
// logins so response timing does not reveal whether an account exists.
var fakeHash = func() string {
	h, err := auth.HashPassword("timing-equalizer-not-a-real-password")
	if err != nil {
		panic(err)
	}
	return h
}()

// AuthHandler serves /v1/auth/* and /v1/me.
type AuthHandler struct {
	users  *user.Repo
	pool   *pgxpool.Pool
	secret string
}

func NewAuthHandler(pool *pgxpool.Pool, secret string) *AuthHandler {
	return &AuthHandler{users: user.NewRepo(pool), pool: pool, secret: secret}
}

// Mount registers the auth routes on r; /v1/me goes behind the JWT middleware.
func (h *AuthHandler) Mount(r chi.Router) {
	r.Post("/v1/auth/register", h.Register)
	r.Post("/v1/auth/login", h.Login)
	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(h.secret))
		r.Get("/v1/me", h.Me)
	})
}

type credentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userResponse struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	IsAdmin   bool      `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
}

func toUserResponse(u user.User) userResponse {
	return userResponse{ID: u.ID, Email: u.Email, IsAdmin: u.IsAdmin, CreatedAt: u.CreatedAt}
}

type authResponse struct {
	User      userResponse `json:"user"`
	Token     string       `json:"token"`
	ExpiresAt time.Time    `json:"expires_at"`
}

func validateCredentials(req credentialsRequest) map[string]any {
	details := map[string]any{}
	if addr, err := mail.ParseAddress(req.Email); err != nil || addr.Address != req.Email {
		details["email"] = "must be a valid email address"
	}
	if len(req.Password) < 8 {
		details["password"] = "must be at least 8 characters"
	}
	if len(details) == 0 {
		return nil
	}
	return details
}

func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, r, http.StatusBadRequest, "invalid_json", "request body must be valid JSON")
		return
	}

	if details := validateCredentials(req); details != nil {
		response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
			"validation_failed", "invalid registration fields", details)
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process registration")
		return
	}

	u, err := h.users.Create(r.Context(), req.Email, hash)
	if errors.Is(err, user.ErrEmailTaken) {
		response.Error(w, r, http.StatusUnprocessableEntity, "email_in_use", "email is already in use")
		return
	}
	if err != nil {
		slog.Error("register: create user", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process registration")
		return
	}

	h.audit(r, u.ID, "user_registered", &u.ID, nil)

	token, expiresAt, err := h.issueToken(u)
	if err != nil {
		slog.Error("register: issue token", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process registration")
		return
	}
	response.JSON(w, r, http.StatusCreated, authResponse{
		User: toUserResponse(u), Token: token, ExpiresAt: expiresAt,
	})
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, r, http.StatusBadRequest, "invalid_json", "request body must be valid JSON")
		return
	}

	u, err := h.users.GetByEmail(r.Context(), req.Email)
	if errors.Is(err, user.ErrNotFound) {
		// Burn the same argon2id cost as a real verify so timing
		// doesn't reveal that the account does not exist.
		_, _ = auth.VerifyPassword(req.Password, fakeHash)
		h.audit(r, zeroUUID, "login_failed", nil, map[string]any{"attempted_email": req.Email})
		response.Error(w, r, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}
	if err != nil {
		slog.Error("login: get user", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process login")
		return
	}

	match, err := auth.VerifyPassword(req.Password, u.PasswordHash)
	if err != nil {
		slog.Error("login: verify password", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process login")
		return
	}
	if !match {
		h.audit(r, u.ID, "login_failed", nil, map[string]any{"attempted_email": req.Email})
		response.Error(w, r, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}

	h.audit(r, u.ID, "login_succeeded", &u.ID, nil)

	token, expiresAt, err := h.issueToken(u)
	if err != nil {
		slog.Error("login: issue token", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process login")
		return
	}
	response.JSON(w, r, http.StatusOK, authResponse{
		User: toUserResponse(u), Token: token, ExpiresAt: expiresAt,
	})
}

func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		response.Error(w, r, http.StatusUnauthorized, "unauthorized", "missing authentication")
		return
	}

	u, err := h.users.GetByID(r.Context(), userID)
	if errors.Is(err, user.ErrNotFound) {
		response.Error(w, r, http.StatusUnauthorized, "unauthorized", "user no longer exists")
		return
	}
	if err != nil {
		slog.Error("me: get user", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to load user")
		return
	}
	response.JSON(w, r, http.StatusOK, map[string]any{"user": toUserResponse(u)})
}

func (h *AuthHandler) issueToken(u user.User) (string, time.Time, error) {
	expiresAt := time.Now().Add(TokenTTL)
	token, err := auth.GenerateToken(u.ID, u.Email, u.IsAdmin, h.secret, TokenTTL)
	return token, expiresAt, err
}

func (h *AuthHandler) audit(r *http.Request, entityID, action string, actorUserID *string, metadata map[string]any) {
	_, err := h.pool.Exec(r.Context(),
		`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, actor_user_id, metadata, request_id)
		 VALUES ('user', $1, $2, 'user', $3, $4, $5)`,
		entityID, action, actorUserID, metadata, middleware.GetReqID(r.Context()),
	)
	if err != nil {
		slog.Error("audit log insert failed", "action", action, "error", err)
	}
}
