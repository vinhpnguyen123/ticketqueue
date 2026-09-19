# The dropped-payment demo

## What it proves

A payment provider charged the customer's card, and then the response was lost
on the network. Our server has no idea whether money moved.

Most systems guess here. Guessing "it failed" releases the tickets of a customer
who was charged. Guessing "it worked" hands out tickets nobody paid for. This
system does neither: it records what it knows, refuses to decide, and resolves
the booking later by asking the provider — using the same idempotency key it
sent with the original charge.

The demo shows that recovery happening with **no user retry and no manual
intervention**, and prints the audit trail that explains every step.

Concretely, it asserts:

| Assertion | Why it matters |
|---|---|
| Purchase returns `504 payment_processing_unclear` | The API never claims an outcome it cannot verify |
| Booking stays `held`, payment row is `unknown` | Durable evidence of the attempt, not a guess |
| The provider *does* hold a `succeeded` charge under the same key | The money really did move — this is the gap being closed |
| Booking flips to `confirmed` on its own | The reconciliation worker asked and acted |
| Exactly `quantity` tickets are `sold` | No partial fulfilment |
| Exactly **one** `succeeded` payment row | Charged once, never twice |
| Audit timeline reads `booking_created → tickets_held → payment_outcome_unknown → booking_confirmed + payment_succeeded` | Every state change is attributable, with the last two by `actor_type = 'worker'` |

## Recovery time

The worker only considers bookings older than **90 seconds** — comfortably past
the 3s payment timeout, so the synchronous path has definitely finished with
them — and runs every **30 seconds**. Real recovery therefore lands at
**90–120 seconds**. That is deliberately far inside the 10-minute
`hold_expires_at` deadline, so reconciliation resolves a payment long before the
hold-expiry worker would release the tickets.

## Running it

```bash
make up && make migrate-up && make seed

# three terminals
make run-payments    # simulated provider  :8082
make run             # API                 :8081
make run-worker      # promotion + reconciliation workers

make demo            # or: bash scripts/demo_reconciliation.sh --happy
```

`--happy` also runs the ordinary success path (`tok_visa` → `201 confirmed`
immediately) so the contrast is visible side by side.

The script exits non-zero if any assertion fails, so it can gate CI later.
Ports come from `API_URL` / `PAYMENT_URL` env vars if your local stack differs.

## The same scenario in the test suite

`internal/booking/demo_reconciliation_test.go` runs the identical scenario as a
Go integration test (`make test-integration`). It calls the reconciliation cycle
directly and ages `created_at` in-test, so it takes milliseconds instead of two
minutes.

It adds two symmetric money-correctness checks that must hold after *any*
sequence of events:

- **No confirmed booking lacks a succeeded payment** — nobody got a free ticket.
- **No succeeded payment lacks a confirmed booking** — nobody paid without
  getting one.

Together those two queries are the whole promise of the system, expressed as
SQL.

**429 on back-to-back runs:** register is rate-limited to 3 per window per IP.
Wait for the window to reset, or run against a fresh Redis (`docker compose restart redis`).


## Running this against the containers

`make docker-up` runs the built images through PgBouncer. The demo passes there
(measured: recovered in 110s), with one caveat worth knowing.

**The webhook must be off for this demo to demonstrate what it claims.** The
compose app profile wires `WEBHOOK_URL` by default, which is correct for a
deployment. With it on, `tok_drops_response` behaves differently from the story
above: the provider hijacks the connection and fires the webhook immediately, so
the webhook resolves the booking *before* the caller has even noticed the dropped
connection. TX2 then finds the booking already confirmed and returns **201**, not
504 — the sibling-confirmed branch, which is correct behaviour and has its own
test. The demo asserts the 504, so run it with the webhook disabled:

```bash
WEBHOOK_URL= docker compose --profile app up -d payment-service
make demo
```

This is not a bug in either path. It is the webhook doing exactly what it was
built for (§14 #1: a latency optimisation on the dropped-response path), and it
means the 90-120s worker recovery is the *backstop* timing, not the typical one.
