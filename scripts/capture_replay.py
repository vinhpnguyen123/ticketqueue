#!/usr/bin/env python3
"""Records two real runs into site/replay.json for the demo page.

Nothing here is invented: every time and count comes from the database, the
provider's charge record, or k6. Needs the three binaries running.

Usage: python3 scripts/capture_replay.py <k6-summary.json> <buyers> <tickets> <tier_id>
"""
import datetime, json, subprocess, sys


def query(sql):
    out = subprocess.run(
        ["docker", "compose", "exec", "-T", "postgres", "psql", "-U", "ticketqueue",
         "-d", "ticketqueue", "-At", "-c", sql],
        check=True, capture_output=True, text=True).stdout.strip()
    return json.loads(out) if out else None


def ms(later, earlier):
    def parse(value):
        return datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    return round((parse(later) - parse(earlier)).total_seconds() * 1000)


def lost_response():
    booking = query("""
      SELECT row_to_json(x) FROM (
        SELECT b.id, b.quantity, b.total_cents, b.currency, b.created_at, b.confirmed_at
        FROM booking b
        WHERE b.status = 'confirmed'
          AND EXISTS (SELECT 1 FROM payments p WHERE p.booking_id = b.id AND p.status = 'unknown')
          AND EXISTS (SELECT 1 FROM audit_log a WHERE a.entity_id = b.id
                      AND a.action = 'booking_confirmed' AND a.metadata->>'source' = 'reconciliation')
        ORDER BY b.created_at DESC LIMIT 1) x""")
    rows = query(f"""
      SELECT json_agg(x ORDER BY x.created_at, x.action) FROM (
        SELECT action, actor_type, created_at FROM audit_log
        WHERE entity_id = '{booking['id']}') x""")
    charge = query(f"""
      SELECT provider_response FROM payments
      WHERE booking_id = '{booking['id']}' AND status = 'succeeded'""")
    start = booking["created_at"]
    return {
        "reference": "TQ-" + booking["id"][:8].upper(),
        "quantity": booking["quantity"],
        "total_cents": booking["total_cents"],
        "currency": booking["currency"],
        "charged_at_ms": ms(charge["created_at"], start),
        "charge_amount": charge["amount"],
        "charge_status": charge["status"],
        "resolved_at_ms": ms(booking["confirmed_at"], start),
        "events": [{"at_ms": ms(r["created_at"], start), "action": r["action"],
                    "actor": r["actor_type"]} for r in rows],
    }


def stampede(summary_path, buyers, tickets, tier_id):
    metrics = json.load(open(summary_path))["metrics"]
    # k6 has no sold_out submetric, so it is the remainder once every other
    # outcome is proven to be zero.
    for outcome in ("server_error", "conflict", "validation_failed", "unexpected"):
        found = metrics[f"purchase_outcomes{{outcome:{outcome}}}"]["count"]
        if found:
            sys.exit(f"refusing to record a run with {found} {outcome} outcomes")
    total = metrics["purchase_outcomes"]["count"]
    if total != buyers:
        sys.exit(f"expected {buyers} purchase attempts, k6 reported {total}")
    claims = query(f"""
      SELECT json_agg(x ORDER BY x.created_at) FROM (
        SELECT b.id, b.created_at, b.status FROM booking b
        WHERE b.ticket_tier_id = '{tier_id}') x""") or []
    sold = query(f"""
      SELECT to_json(COUNT(*)) FROM tickets
      WHERE ticket_tier_id = '{tier_id}' AND status = 'sold'""")
    start = claims[0]["created_at"]
    return {
        "buyers": buyers,
        "tickets": tickets,
        "sold": sold,
        "turned_away": total - sold,
        "claims": [{"at_ms": ms(c["created_at"], start),
                    "reference": "TQ-" + c["id"][:8].upper()} for c in claims],
    }


if __name__ == "__main__":
    summary, buyers, tickets, tier = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
    data = {
        "captured_on": datetime.date.today().isoformat(),
        "lost_response": lost_response(),
        "stampede": stampede(summary, buyers, tickets, tier),
    }
    with open("site/replay.json", "w") as f:
        json.dump(data, f, indent=1)
        f.write("\n")
    print(json.dumps({k: v for k, v in data["stampede"].items() if k != "claims"}))
    print(data["lost_response"]["reference"], data["lost_response"]["resolved_at_ms"], "ms")
