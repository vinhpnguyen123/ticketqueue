// Command seed populates the database with a demo user, event, tiers,
// and pre-created ticket rows for local development.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/alexedwards/argon2id"

	"github.com/vinhpnguyen123/ticketqueue/internal/config"
	"github.com/vinhpnguyen123/ticketqueue/internal/db"
)

const demoEmail = "demo@ticketqueue.dev"

type tierSpec struct {
	name       string
	priceCents int64
	quantity   int
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(context.Background()); err != nil {
		logger.Error("seed failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)`, demoEmail).Scan(&exists); err != nil {
		return fmt.Errorf("check demo user: %w", err)
	}
	if exists {
		fmt.Println("already seeded")
		return nil
	}

	hash, err := argon2id.CreateHash("demo1234", argon2id.DefaultParams)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var userID string
	err = tx.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, is_admin) VALUES ($1, $2, TRUE) RETURNING id`,
		demoEmail, hash,
	).Scan(&userID)
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}

	var eventID string
	err = tx.QueryRow(ctx,
		`INSERT INTO events (name, venue, event_at, status, max_concurrent_windows, purchase_window_seconds)
		 VALUES ($1, $2, $3, 'on_sale', 50, 300) RETURNING id`,
		"Demo Concert", "Saigon Arena", time.Now().AddDate(0, 0, 30),
	).Scan(&eventID)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}

	tiers := []tierSpec{
		{name: "VIP", priceCents: 50000, quantity: 100},
		{name: "General", priceCents: 10000, quantity: 400},
	}

	tierIDs := make(map[string]string, len(tiers))
	for _, t := range tiers {
		var tierID string
		err = tx.QueryRow(ctx,
			`INSERT INTO ticket_tier (event_id, name, price_cents, currency, quantity_total)
			 VALUES ($1, $2, $3, 'USD', $4) RETURNING id`,
			eventID, t.name, t.priceCents, t.quantity,
		).Scan(&tierID)
		if err != nil {
			return fmt.Errorf("insert tier %s: %w", t.name, err)
		}
		tierIDs[t.name] = tierID

		tag, err := tx.Exec(ctx,
			`INSERT INTO tickets (ticket_tier_id, status)
			 SELECT $1, 'available' FROM generate_series(1, $2)`,
			tierID, t.quantity,
		)
		if err != nil {
			return fmt.Errorf("insert tickets for tier %s: %w", t.name, err)
		}
		if tag.RowsAffected() != int64(t.quantity) {
			return fmt.Errorf("tier %s: expected %d tickets, inserted %d", t.name, t.quantity, tag.RowsAffected())
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	fmt.Println("seeded demo data")
	fmt.Printf("  user:  %s (id %s)\n", demoEmail, userID)
	fmt.Printf("  event: Demo Concert (id %s)\n", eventID)
	for _, t := range tiers {
		fmt.Printf("  tier:  %-8s id %s — %d tickets\n", t.name, tierIDs[t.name], t.quantity)
	}
	return nil
}
