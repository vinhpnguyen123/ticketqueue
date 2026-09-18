// SCENARIO B — the journey a real user takes.
//
// join queue → poll position until admitted → purchase. Contention at the claim
// is naturally limited by max_concurrent_windows, which is the point: this
// measures queue throughput and admission pacing, NOT raw claim contention.
// Scenario A is the one that backs a "N concurrent buyers" claim.
//
// Registration and login are skipped: the API rate-limits them to 3 and 5 per
// minute per IP, so a load test cannot drive them from one machine. cmd/loadseed
// provides ready session JWTs instead, and this script measures everything after
// the front door.
//
//   make load-seed TICKETS=2000 USERS=2000 WINDOWS=200
//   k6 run -e VUS=500 scripts/k6/full_journey.js
import http from 'k6/http';
import { sleep } from 'k6';
import { Trend } from 'k6/metrics';
import { world, userFor, API, idemKey, purchaseBody } from './lib/setup.js';

// 409 sold_out and 504 payment_processing_unclear are outcomes this system is
// DESIGNED to produce, so k6's default (any 4xx/5xx is a failure) would report
// a healthy sell-out as a 50% failure rate. Narrowing 'expected' to < 500 makes
// http_req_failed mean what it should: the app broke.
http.setResponseCallback(http.expectedStatuses({ min: 200, max: 499 }));
import { recordPurchase, checkPurchase, checkOK } from './lib/checks.js';

const seed = world();
const VUS = parseInt(__ENV.VUS || '200', 10);
const DURATION = __ENV.DURATION || '2m';

// How long a user waits between joining the queue and being allowed to buy.
// This is the number a product owner cares about.
const timeToAdmission = new Trend('queue_time_to_admission', true);

// queue_position is rate-limited to 30 per minute per user, so polling faster
// than every 2 seconds would measure the limiter rather than the queue.
const POLL_INTERVAL = 2.5;
const MAX_WAIT_SECONDS = 120;

export const options = {
  scenarios: {
    journey: {
      executor: 'per-vu-iterations',
      vus: VUS,
      iterations: 1,
      maxDuration: DURATION,
    },
  },
  thresholds: {
    'purchase_completed_duration': ['p(95)<2000'],
    'checks': ['rate>0.99'],
    // Meaningful only because of setResponseCallback above: 5xx only.
    'http_req_failed': ['rate==0'],
    'purchase_outcomes{outcome:server_error}': ['count==0'],
  },
};

export default function () {
  const user = userFor(__VU);
  const auth = { 'Authorization': `Bearer ${user.jwt}`, 'Content-Type': 'application/json' };

  const joined = http.post(`${API}/v1/events/${seed.event_id}/queue/join`, null, {
    headers: auth,
    tags: { name: 'POST /v1/events/{id}/queue/join' },
  });
  checkOK(joined, 'joined the queue');
  if (joined.status >= 400) {
    return;
  }

  const start = Date.now();
  let purchaseToken = null;
  while ((Date.now() - start) / 1000 < MAX_WAIT_SECONDS) {
    sleep(POLL_INTERVAL);
    const pos = http.get(`${API}/v1/events/${seed.event_id}/queue/position`, {
      headers: auth,
      tags: { name: 'GET /v1/events/{id}/queue/position' },
    });
    if (pos.status !== 200) {
      continue;
    }
    const body = pos.json().data;
    if (body && body.status === 'admitted') {
      purchaseToken = body.purchase_token;
      timeToAdmission.add(Date.now() - start);
      break;
    }
  }

  if (!purchaseToken) {
    // Never admitted inside the window. Not a failure of the system — it is
    // the queue holding the line — but it is not a purchase either.
    return;
  }

  const res = http.post(`${API}/v1/purchases`, purchaseBody(seed, purchaseToken, 1), {
    headers: Object.assign({}, auth, { 'Idempotency-Key': idemKey('journey', __VU, __ITER) }),
    tags: { name: 'POST /v1/purchases' },
  });
  const outcome = recordPurchase(res);
  checkPurchase(res, outcome);
}
