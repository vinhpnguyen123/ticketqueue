// Package event provides the Event/TicketTier entities and their
// Postgres repository for the public browsing endpoints.
package event

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when no event matches the lookup.
var ErrNotFound = errors.New("event not found")

// Event mirrors the events table.
type Event struct {
	ID                    string
	Name                  string
	Venue                 string
	EventAt               time.Time
	Status                string
	MaxConcurrentWindows  int
	PurchaseWindowSeconds int
	CreatedAt             time.Time
}

// TicketTier mirrors the ticket_tier table plus its live availability.
type TicketTier struct {
	ID             string
	EventID        string
	Name           string
	PriceCents     int64
	Currency       string
	QuantityTotal  int
	SaleStart      *time.Time
	SaleEnd        *time.Time
	CreatedAt      time.Time
	AvailableCount int
}

// EventWithTiers is an event with its tiers and their availability.
type EventWithTiers struct {
	Event
	Tiers []TicketTier
}

// Repo is the Postgres-backed event repository.
type Repo struct {
	pool *pgxpool.Pool
}

func NewRepo(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

const eventColumns = "id, name, venue, event_at, status, max_concurrent_windows, purchase_window_seconds, created_at"

func scanEvent(row pgx.Row) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.Name, &e.Venue, &e.EventAt, &e.Status,
		&e.MaxConcurrentWindows, &e.PurchaseWindowSeconds, &e.CreatedAt)
	return e, err
}

// ListOnSale returns on-sale events with tiers, cursor-paginated by event
// id. It fetches limit+1 rows to detect whether more pages exist; when
// they do, nextCursor is the id of the last returned event.
func (r *Repo) ListOnSale(ctx context.Context, limit int, cursor *uuid.UUID) ([]EventWithTiers, *uuid.UUID, error) {
	var cursorArg *string
	if cursor != nil {
		s := cursor.String()
		cursorArg = &s
	}

	rows, err := r.pool.Query(ctx,
		`SELECT `+eventColumns+` FROM events
		 WHERE status = 'on_sale' AND ($1::uuid IS NULL OR id > $1)
		 ORDER BY id
		 LIMIT $2`,
		cursorArg, limit+1,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var events []EventWithTiers
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, EventWithTiers{Event: e})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list events: %w", err)
	}

	var nextCursor *uuid.UUID
	if len(events) > limit {
		events = events[:limit]
		last, err := uuid.Parse(events[limit-1].ID)
		if err != nil {
			return nil, nil, fmt.Errorf("parse cursor id: %w", err)
		}
		nextCursor = &last
	}

	if err := r.loadTiers(ctx, events); err != nil {
		return nil, nil, err
	}
	return events, nextCursor, nil
}

// GetByID returns one event (any status) with tiers and availability.
func (r *Repo) GetByID(ctx context.Context, id string) (EventWithTiers, error) {
	e, err := scanEvent(r.pool.QueryRow(ctx,
		`SELECT `+eventColumns+` FROM events WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return EventWithTiers{}, ErrNotFound
	}
	if err != nil {
		return EventWithTiers{}, fmt.Errorf("get event: %w", err)
	}

	events := []EventWithTiers{{Event: e}}
	if err := r.loadTiers(ctx, events); err != nil {
		return EventWithTiers{}, err
	}
	return events[0], nil
}

// loadTiers attaches tiers and availability to events in place, using one
// query for tiers and one grouped query for counts (never one per tier).
func (r *Repo) loadTiers(ctx context.Context, events []EventWithTiers) error {
	if len(events) == 0 {
		return nil
	}

	eventIDs := make([]string, len(events))
	byEventID := make(map[string]*EventWithTiers, len(events))
	for i := range events {
		eventIDs[i] = events[i].ID
		byEventID[events[i].ID] = &events[i]
	}

	rows, err := r.pool.Query(ctx,
		`SELECT id, event_id, name, price_cents, currency, quantity_total, sale_start, sale_end, created_at
		 FROM ticket_tier WHERE event_id = ANY($1) ORDER BY price_cents DESC, id`,
		eventIDs,
	)
	if err != nil {
		return fmt.Errorf("list tiers: %w", err)
	}
	defer rows.Close()

	var tierIDs []string
	for rows.Next() {
		var t TicketTier
		if err := rows.Scan(&t.ID, &t.EventID, &t.Name, &t.PriceCents, &t.Currency,
			&t.QuantityTotal, &t.SaleStart, &t.SaleEnd, &t.CreatedAt); err != nil {
			return fmt.Errorf("scan tier: %w", err)
		}
		ev := byEventID[t.EventID]
		ev.Tiers = append(ev.Tiers, t)
		tierIDs = append(tierIDs, t.ID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list tiers: %w", err)
	}
	if len(tierIDs) == 0 {
		return nil
	}

	// TODO(week 2): this Postgres count is the source-of-truth path; the
	// Redis fast-path availability cache will replace these reads.
	counts, err := r.pool.Query(ctx,
		`SELECT ticket_tier_id, count(*) FROM tickets
		 WHERE ticket_tier_id = ANY($1) AND status = 'available'
		 GROUP BY ticket_tier_id`,
		tierIDs,
	)
	if err != nil {
		return fmt.Errorf("count availability: %w", err)
	}
	defer counts.Close()

	availableByTier := make(map[string]int, len(tierIDs))
	for counts.Next() {
		var tierID string
		var available int
		if err := counts.Scan(&tierID, &available); err != nil {
			return fmt.Errorf("scan availability: %w", err)
		}
		availableByTier[tierID] = available
	}
	if err := counts.Err(); err != nil {
		return fmt.Errorf("count availability: %w", err)
	}

	for i := range events {
		for j := range events[i].Tiers {
			events[i].Tiers[j].AvailableCount = availableByTier[events[i].Tiers[j].ID]
		}
	}
	return nil
}
