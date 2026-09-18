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

// Each request needs its own idempotency key: reusing one would exercise the
// replay path rather than the purchase path.
export function idemKey(prefix, vuID, iter) {
  return `${prefix}-${__ENV.RUN_ID || 'local'}-${vuID}-${iter}`;
}

export function purchaseBody(seedWorld, purchaseToken, quantity) {
  return JSON.stringify({
    event_id: seedWorld.event_id,
    tier_id: seedWorld.tier_id,
    quantity: quantity,
    payment_token: __ENV.PAYMENT_TOKEN || 'tok_visa',
    purchase_token: purchaseToken,
  });
}
