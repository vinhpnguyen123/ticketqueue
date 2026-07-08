// Package user provides the User entity and its Postgres repository.
package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrEmailTaken is returned by Create when the email already exists.
var ErrEmailTaken = errors.New("email already in use")

// ErrNotFound is returned when no user matches the lookup.
var ErrNotFound = errors.New("user not found")

// User mirrors the users table.
type User struct {
	ID           string
	Email        string
	PasswordHash string
	IsAdmin      bool
	CreatedAt    time.Time
}

// Repo is the Postgres-backed user repository.
type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

const userColumns = "id, email, password_hash, is_admin, created_at"

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt)
	return u, err
}

// Create inserts a new user, mapping a unique violation on email to ErrEmailTaken.
func (r *Repo) Create(ctx context.Context, email, passwordHash string) (User, error) {
	u, err := scanUser(r.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING `+userColumns,
		email, passwordHash,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	return u, nil
}

func (r *Repo) GetByEmail(ctx context.Context, email string) (User, error) {
	u, err := scanUser(r.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = $1`, email))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

func (r *Repo) GetByID(ctx context.Context, id string) (User, error) {
	u, err := scanUser(r.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user by id: %w", err)
	}
	return u, nil
}
