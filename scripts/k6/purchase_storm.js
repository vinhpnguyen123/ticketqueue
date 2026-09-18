// SCENARIO A — raw claim contention. The headline number.
//
// Every VU holds a pre-minted purchase token and hits POST /v1/purchases at the
// same instant. The tokens are minted by cmd/loadseed rather than earned
// through the queue PRECISELY so the queue does not throttle the thing under
// test: max_concurrent_windows exists to stop this from happening in
// production, which is why a realistic journey can never produce it. This is a
// load-test affordance, not a production path.
//
// What it proves: N buyers competing for a fixed number of tickets produce
// exactly that number of sales, no more — the three-layer defense under
// maximum contention.
//
//   make load-seed TICKETS=100 USERS=2000
//   k6 run -e VUS=2000 scripts/k6/purchase_storm.js
//   make verify-run
import http from 'k6/http';
import { world, userFor, API, idemKey, purchaseBody } from './lib/setup.js';

// 409 sold_out and 504 payment_processing_unclear are outcomes this system is
// DESIGNED to produce, so k6's default (any 4xx/5xx is a failure) would report
// a healthy sell-out as a 50% failure rate. Narrowing 'expected' to < 500 makes
// http_req_failed mean what it should: the app broke.
http.setResponseCallback(http.expectedStatuses({ min: 200, max: 499 }));
import { recordPurchase, checkPurchase } from './lib/checks.js';

const seed = world();
const VUS = parseInt(__ENV.VUS || '500', 10);
const QUANTITY = parseInt(__ENV.QUANTITY || '1', 10);

export const options = {
  scenarios: {
    storm: {
      // One shot each, all at once: this measures contention, not throughput
      // over time. A ramp would let the tier sell out before the later VUs
      // arrive, and they would measure an empty tier instead of a race.
      executor: 'per-vu-iterations',
      vus: VUS,
      iterations: 1,
      maxDuration: '2m',
    },
  },
  thresholds: {
    // THE latency number: completed purchases only. See lib/checks.js for why
    // this is not http_req_duration.
    'purchase_completed_duration': ['p(95)<2000'],
    'checks': ['rate>0.99'],
    // Expected 409s and 504s are not failures; a 5xx is. k6 counts 4xx/5xx as
    // failed by default, so this is scoped to what the app actually broke on.
    // Meaningful only because of setResponseCallback above: 5xx only.
    'http_req_failed': ['rate==0'],
    'purchase_outcomes{outcome:server_error}': ['count==0'],
    'purchase_outcomes{outcome:unexpected}': ['count==0'],
  },
};

export default function () {
  const user = userFor(__VU);
  const res = http.post(`${API}/v1/purchases`, purchaseBody(seed, user.purchase_token, QUANTITY), {
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${user.jwt}`,
      'Idempotency-Key': idemKey('storm', __VU, __ITER),
    },
    tags: { name: 'POST /v1/purchases' },
  });

  const outcome = recordPurchase(res);
  checkPurchase(res, outcome);
}

// No handleSummary override on purpose: returning a custom stdout REPLACES k6's
// own summary, which is where threshold results and check rates live. The
// headline number is the p(95) row of purchase_completed_duration in the
// standard output; scripts/k6/README.md says so too.
