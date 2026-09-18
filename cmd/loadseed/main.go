// Command loadseed prepares a clean, isolated world for one k6 run: a fresh
// event and tier, N users, and the JWTs those users need — written to a JSON
// file the k6 scripts read at init.
//
// It exists for two reasons that k6 cannot solve on its own:
//
//  1. The API rate-limits registration to 3 per minute per IP. Creating even a
//     thousand users through HTTP would take hours, so the load harness inserts
//     them directly. That is a load-test affordance, not a production path.
//  2. Purchase tokens are minted here with auth.GeneratePurchaseToken — the
//     SAME function the promotion worker uses. Reimplementing HS256 signing in
//     JavaScript would create a second definition of a security token that can
//     drift from the real one without anyone noticing.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/ticketqueue/internal/auth"
	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/config"
	"github.com/vinhpnguyen123/ticketqueue/internal/db"
	"github.com/vinhpnguyen123/ticketqueue/internal/redis"
)

// User is one pre-provisioned load-test identity.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	// JWT is the ordinary session token, so k6 never has to call
	// /v1/auth/login (5 per minute per IP).
	JWT string `json:"jwt"`
	// PurchaseToken is minted directly rather than earned through the queue.
	// Scenario A uses it precisely so the queue does not throttle the thing
	// under test; Scenario B ignores it and earns its own.
	PurchaseToken string `json:"purchase_token"`
}

// Seed is what the k6 scripts read.
type Seed struct {
	EventID      string `json:"event_id"`
	TierID       string `json:"tier_id"`
	TierQuantity int    `json:"tier_quantity"`
	PriceCents   int64  `json:"price_cents"`
	Users        []User `json:"users"`
}

func main() {
	var (
		users      = flag.Int("users", 1000, "how many load-test identities to create")
		tickets    = flag.Int("tickets", 100, "tickets in the tier (100 = oversell proof, 2000 = latency proof)")
		windows    = flag.Int("windows", 50, "event max_concurrent_windows (Scenario B admission pacing)")
		windowSecs = flag.Int("window-seconds", 3600, "purchase window length; long so tokens outlive the run")
		out        = flag.String("out", "scripts/k6/seed.json", "where to write the seed file")
		price      = flag.Int64("price", 50000, "ticket price in cents")
	)
	flag.Parse()

	if err := run(context.Background(), *users, *tickets, *windows, *windowSecs, *price, *out); err != nil {
		slog.Error("loadseed failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, users, tickets, windows, windowSecs int, price int64, out string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	seed := Seed{TierQuantity: tickets, PriceCents: price}

	// A fresh event per run: counts only mean something against a tier nobody
	// has touched.
	stamp := time.Now().UnixNano()
	if err := pool.QueryRow(ctx,
		`INSERT INTO events (name, venue, event_at, status, max_concurrent_windows, purchase_window_seconds)
		 VALUES ($1, 'Load Arena', NOW() + INTERVAL '30 days', 'on_sale', $2, $3) RETURNING id`,
		fmt.Sprintf("Load Test %d", stamp), windows, windowSecs,
	).Scan(&seed.EventID); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ticket_tier (event_id, name, price_cents, quantity_total)
		 VALUES ($1, 'GA', $2, $3) RETURNING id`, seed.EventID, price, tickets,
	).Scan(&seed.TierID); err != nil {
		return fmt.Errorf("insert tier: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tickets (ticket_tier_id, status)
		 SELECT $1, 'available' FROM generate_series(1, $2)`, seed.TierID, tickets); err != nil {
		return fmt.Errorf("insert tickets: %w", err)
	}

	if err := seedUsers(ctx, pool, cfg.JWTSecret, &seed, users, windowSecs, stamp); err != nil {
		return err
	}

	// Prime the Layer-1 counter so the first purchases do not all race to lazy-
	// initialise it. Postgres is still the truth; this only avoids measuring
	// our own cold start.
	rdb, err := redis.NewClient(ctx, cfg.RedisAddr)
	if err != nil {
		return fmt.Errorf("redis: %w", err)
	}
	defer rdb.Close()
	if err := rdb.Set(ctx, booking.InventoryKey(seed.TierID), tickets, 0).Err(); err != nil {
		return fmt.Errorf("prime inventory: %w", err)
	}

	body, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal seed: %w", err)
	}
	if err := os.WriteFile(out, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}

	slog.Info("load world ready",
		"event_id", seed.EventID, "tier_id", seed.TierID,
		"tickets", tickets, "users", len(seed.Users), "out", out)
	return nil
}

// seedUsers inserts identities in batches and mints both tokens for each.
func seedUsers(ctx context.Context, pool *pgxpool.Pool, secret string, seed *Seed, n, windowSecs int, stamp int64) error {
	const batch = 500
	// A fixed hash: these identities never log in with a password, so hashing
	// a real one n times would only burn CPU (argon2id is deliberately slow).
	const placeholderHash = "loadtest-no-password"

	seed.Users = make([]User, 0, n)
	admittedAt := time.Now()

	for start := 0; start < n; start += batch {
		end := start + batch
		if end > n {
			end = n
		}
		emails := make([]string, 0, end-start)
		for i := start; i < end; i++ {
			emails = append(emails, fmt.Sprintf("load-%d-%d@loadtest.dev", stamp, i))
		}

		rows, err := pool.Query(ctx,
			`INSERT INTO users (email, password_hash)
			 SELECT unnest($1::text[]), $2
			 RETURNING id, email`, emails, placeholderHash)
		if err != nil {
			return fmt.Errorf("insert users: %w", err)
		}
		for rows.Next() {
			var u User
			if err := rows.Scan(&u.ID, &u.Email); err != nil {
				rows.Close()
				return fmt.Errorf("scan user: %w", err)
			}
			seed.Users = append(seed.Users, u)
		}
		rows.Close()
		if rows.Err() != nil {
			return fmt.Errorf("iterate users: %w", rows.Err())
		}
	}

	for i := range seed.Users {
		u := &seed.Users[i]
		jwt, err := auth.GenerateToken(u.ID, u.Email, false, secret, 6*time.Hour)
		if err != nil {
			return fmt.Errorf("session token: %w", err)
		}
		u.JWT = jwt

		pt, err := auth.GeneratePurchaseToken(u.ID, seed.EventID, admittedAt, windowSecs, secret)
		if err != nil {
			return fmt.Errorf("purchase token: %w", err)
		}
		u.PurchaseToken = pt
	}
	return nil
}
