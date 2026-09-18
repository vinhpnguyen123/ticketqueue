// Reads the world that cmd/loadseed prepared.
//
// The seeding is NOT done here on purpose. The API rate-limits registration to
// 3 per minute per IP, so creating load-test identities over HTTP is impossible
// at any useful scale, and purchase tokens are signed by the same Go function
// the promotion worker uses rather than by a JavaScript reimplementation that
// could drift from it. See cmd/loadseed.

import { SharedArray } from 'k6/data';

// open() resolves relative to THIS file (scripts/k6/lib/), so the default
// reaches scripts/k6/seed.json. SEED may also be an absolute path.
const SEED_PATH = __ENV.SEED || '../seed.json';

// open() only works during init, which is exactly when we want a missing seed
// file to stop the run — before any load is generated.
//
// SharedArray, not a plain JSON.parse: every VU runs this module in its own JS
// runtime, so a plain parse would give each of N VUs its own copy of a 3.7MB
// seed (~15MB once it is objects). At 2,000 VUs that is tens of GB and the
// machine dies before the API is ever under load. SharedArray keeps one copy in
// Go memory and hands out elements on access.
function parseSeed() {
  return JSON.parse(open(SEED_PATH));
}

const users = new SharedArray('users', () => parseSeed().users);

// One-element array because SharedArray only holds arrays. Indexing it
// deserializes just this small object, never the 5,000 users.
const meta = new SharedArray('meta', () => {
  const s = parseSeed();
  return [{
    event_id: s.event_id,
    tier_id: s.tier_id,
    tier_quantity: s.tier_quantity,
    price_cents: s.price_cents,
  }];
});

export function world() {
  if (users.length === 0) {
    throw new Error('seed has no users — run: make load-seed');
  }
  return meta[0];
}

// userFor hands each VU its own identity, so no two VUs share a user. That
// matters: the purchase endpoint allows one held booking per user, so sharing
// would produce 410 active_booking_exists instead of real contention.
export function userFor(vuID) {
  return users[(vuID - 1) % users.length];
}

export const API = __ENV.API_URL || 'http://localhost:8081';

// RUN_ID defaults to a per-run value, NOT a constant. Idempotency keys are
// PRIMARY KEY (key, user_id), so a constant here is only safe while every run
// re-seeds fresh users. The moment a run reuses the seed — which is every
// chaos rerun and every quick repeat — a constant makes each iteration collide
// with the previous run's key and answer 422 idempotency_key_reuse. The test
// then measures the idempotency middleware instead of the purchase path, and
// the count of failures is fixed by history rather than by this run.
const RUN_ID = __ENV.RUN_ID || `${Date.now()}`;

// Each request needs its own idempotency key: reusing one would exercise the
// replay path rather than the purchase path.
export function idemKey(prefix, vuID, iter) {
  return `${prefix}-${RUN_ID}-${vuID}-${iter}`;
}

export function purchaseBody(seedWorld, purchaseToken, quantity) {
  return JSON.stringify({
    event_id: seedWorld.event_id,
    tier_id: seedWorld.tier_id,
    quantity: quantity,
    payment_token: paymentToken(),
    purchase_token: purchaseToken,
  });
}

// paymentToken picks this VU's card. DROP_PCT mixes in tok_drops_response,
// which CHAOS 3 needs: killing the worker only proves something if ambiguous
// bookings are being produced the whole time it is dead. The split is by VU id
// rather than per iteration so a VU's outcome is reproducible from its number.
function paymentToken() {
  const dropPct = parseInt(__ENV.DROP_PCT || '0', 10);
  if (dropPct > 0 && __VU % 100 < dropPct) {
    return 'tok_drops_response';
  }
  return __ENV.PAYMENT_TOKEN || 'tok_visa';
}

// sustained picks the executor. The oversell proof wants every VU to fire once
// at the same instant — a ramp would let the tier sell out before the late VUs
// arrive. Chaos wants the opposite: load still flowing 30s in, when a process
// gets killed, and still flowing while it comes back. DURATION picks which.
//
// Sustained runs need a tier big enough not to sell out, or every iteration
// after the first few measures a fast-path rejection instead of a purchase.
export function sustained(vus, oneShotMaxDuration) {
  if (__ENV.DURATION) {
    return { executor: 'constant-vus', vus: vus, duration: __ENV.DURATION };
  }
  return { executor: 'per-vu-iterations', vus: vus, iterations: 1, maxDuration: oneShotMaxDuration };
}
