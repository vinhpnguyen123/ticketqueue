//go:build integration

package http_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	api "github.com/vinhpnguyen123/ticketqueue/internal/http"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

func newEventsServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.TestDB(t)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	api.NewEventsHandler(pool).Mount(r)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts, pool
}

// seedEvent inserts an event with optional tiers (name -> [price, ticket count]).
func seedEvent(t *testing.T, pool *pgxpool.Pool, name, status string, tiers map[string][2]int) string {
	t.Helper()
	ctx := context.Background()

	var eventID string
	err := pool.QueryRow(ctx,
		`INSERT INTO events (name, venue, event_at, status) VALUES ($1, 'Test Venue', $2, $3) RETURNING id`,
		name, time.Now().Add(30*24*time.Hour), status,
	).Scan(&eventID)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}

	for tierName, spec := range tiers {
		var tierID string
		err := pool.QueryRow(ctx,
			`INSERT INTO ticket_tier (event_id, name, price_cents, quantity_total) VALUES ($1, $2, $3, $4) RETURNING id`,
			eventID, tierName, spec[0], spec[1],
		).Scan(&tierID)
		if err != nil {
			t.Fatalf("insert tier: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO tickets (ticket_tier_id, status) SELECT $1, 'available' FROM generate_series(1, $2)`,
			tierID, spec[1],
		); err != nil {
			t.Fatalf("insert tickets: %v", err)
		}
	}
	return eventID
}

func getJSON(t *testing.T, url string, into any) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp
}

type listBody struct {
	Data []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Tiers  []struct {
			Name  string `json:"name"`
			Price struct {
				Amount   int64  `json:"amount"`
				Currency string `json:"currency"`
			} `json:"price"`
			AvailableCount int `json:"available_count"`
		} `json:"tiers"`
	} `json:"data"`
	Pagination struct {
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	} `json:"pagination"`
}

func TestListEventsWithTiersAndMoney(t *testing.T) {
	ts, pool := newEventsServer(t)
	seedEvent(t, pool, "Demo Concert", "on_sale", map[string][2]int{
		"VIP":     {50000, 100},
		"General": {10000, 400},
	})

	var body listBody
	resp := getJSON(t, ts.URL+"/v1/events", &body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(body.Data) != 1 || len(body.Data[0].Tiers) != 2 {
		t.Fatalf("data = %+v", body.Data)
	}

	byName := map[string]int{}
	for _, tier := range body.Data[0].Tiers {
		byName[tier.Name] = tier.AvailableCount
		if tier.Price.Currency != "USD" {
			t.Errorf("tier %s currency = %q", tier.Name, tier.Price.Currency)
		}
		if tier.Name == "VIP" && tier.Price.Amount != 50000 {
			t.Errorf("VIP amount = %d, want 50000", tier.Price.Amount)
		}
	}
	if byName["VIP"] != 100 || byName["General"] != 400 {
		t.Errorf("availability = %v, want VIP:100 General:400", byName)
	}
}

func TestMoneyIsObjectForm(t *testing.T) {
	ts, pool := newEventsServer(t)
	seedEvent(t, pool, "Money Event", "on_sale", map[string][2]int{"VIP": {50000, 1}})

	var raw map[string]any
	getJSON(t, ts.URL+"/v1/events", &raw)

	tier := raw["data"].([]any)[0].(map[string]any)["tiers"].([]any)[0].(map[string]any)
	price, ok := tier["price"].(map[string]any)
	if !ok {
		t.Fatalf("price is not an object: %v", tier["price"])
	}
	if price["amount"] != float64(50000) || price["currency"] != "USD" {
		t.Errorf("price = %v", price)
	}
}

func TestListPagination(t *testing.T) {
	ts, pool := newEventsServer(t)
	for _, name := range []string{"Event A", "Event B", "Event C"} {
		seedEvent(t, pool, name, "on_sale", nil)
	}

	seen := map[string]bool{}
	url := ts.URL + "/v1/events?limit=2"
	pages := 0
	for {
		var body listBody
		resp := getJSON(t, url, &body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		pages++
		for _, e := range body.Data {
			if seen[e.ID] {
				t.Fatalf("event %s returned twice", e.ID)
			}
			seen[e.ID] = true
		}
		if !body.Pagination.HasMore {
			if body.Pagination.NextCursor != nil {
				t.Error("next_cursor should be null on the last page")
			}
			break
		}
		if body.Pagination.NextCursor == nil {
			t.Fatal("has_more true but next_cursor null")
		}
		url = ts.URL + "/v1/events?limit=2&cursor=" + *body.Pagination.NextCursor
	}

	if len(seen) != 3 {
		t.Errorf("saw %d events, want 3", len(seen))
	}
	if pages != 2 {
		t.Errorf("pages = %d, want 2", pages)
	}
}

func TestGetEventByID(t *testing.T) {
	ts, pool := newEventsServer(t)
	id := seedEvent(t, pool, "Detail Event", "on_sale", map[string][2]int{"VIP": {50000, 5}})

	var body struct {
		Data struct {
			ID    string `json:"id"`
			Queue struct {
				Size int `json:"size"`
			} `json:"queue"`
			Tiers []struct {
				AvailableCount int `json:"available_count"`
			} `json:"tiers"`
		} `json:"data"`
	}
	resp := getJSON(t, ts.URL+"/v1/events/"+id, &body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if body.Data.ID != id || body.Data.Queue.Size != 0 {
		t.Errorf("data = %+v", body.Data)
	}
	if len(body.Data.Tiers) != 1 || body.Data.Tiers[0].AvailableCount != 5 {
		t.Errorf("tiers = %+v", body.Data.Tiers)
	}

	// Unknown but valid UUID → 404.
	var errBody errorBody
	resp = getJSON(t, ts.URL+"/v1/events/00000000-0000-0000-0000-000000000001", &errBody)
	if resp.StatusCode != http.StatusNotFound || errBody.Error.Type != "event_not_found" {
		t.Errorf("unknown id: status = %d, type = %q", resp.StatusCode, errBody.Error.Type)
	}

	// Malformed UUID → 422.
	resp = getJSON(t, ts.URL+"/v1/events/not-a-uuid", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("malformed id: status = %d, want 422", resp.StatusCode)
	}
}

func TestDraftEventHiddenFromList(t *testing.T) {
	ts, pool := newEventsServer(t)
	seedEvent(t, pool, "Public Event", "on_sale", nil)
	draftID := seedEvent(t, pool, "Secret Draft", "draft", nil)

	var body listBody
	getJSON(t, ts.URL+"/v1/events", &body)
	for _, e := range body.Data {
		if e.ID == draftID {
			t.Fatal("draft event appeared in public list")
		}
	}
	if len(body.Data) != 1 {
		t.Errorf("list length = %d, want 1", len(body.Data))
	}
}

func TestListInvalidCursor(t *testing.T) {
	ts, _ := newEventsServer(t)
	var errBody errorBody
	resp := getJSON(t, ts.URL+"/v1/events?cursor=not-a-uuid", &errBody)
	if resp.StatusCode != http.StatusUnprocessableEntity || errBody.Error.Type != "validation_failed" {
		t.Errorf("status = %d, type = %q", resp.StatusCode, errBody.Error.Type)
	}
}
