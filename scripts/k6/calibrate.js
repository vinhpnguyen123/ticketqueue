// Finds the ceiling of THIS MACHINE before the headline run.
//
// A load test that saturates the laptop rather than the application produces a
// number about the laptop. Watch, at each step: completed p(95), error rate, the
// db_pool_acquire_wait_seconds_total rate in Grafana, and whether k6 itself is
// pegged (CPU, open file descriptors).
//
// The honest reporting rule: the number you claim is the VU count you sustained
// with a clean error rate. If this machine tops out at 4k, the claim is 4k.
//
//   make load-seed TICKETS=20000 USERS=10000
//   k6 run scripts/k6/calibrate.js
//
// The tier is deliberately large here: this measures the machine, and a tier
// that sells out turns every later step into a fast-path rejection benchmark.
import http from 'k6/http';
import { world, userFor, API, idemKey, purchaseBody } from './lib/setup.js';

// 409 sold_out and 504 payment_processing_unclear are outcomes this system is
// DESIGNED to produce, so k6's default (any 4xx/5xx is a failure) would report
// a healthy sell-out as a 50% failure rate. Narrowing 'expected' to < 500 makes
// http_req_failed mean what it should: the app broke.
http.setResponseCallback(http.expectedStatuses({ min: 200, max: 499 }));
import { recordPurchase, checkPurchase } from './lib/checks.js';

const seed = world();

export const options = {
  scenarios: {
    ramp: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '10s', target: 500 }, { duration: '30s', target: 500 },
        { duration: '10s', target: 1000 }, { duration: '30s', target: 1000 },
        { duration: '10s', target: 2000 }, { duration: '30s', target: 2000 },
        { duration: '10s', target: 5000 }, { duration: '30s', target: 5000 },
        { duration: '10s', target: 10000 }, { duration: '30s', target: 10000 },
        { duration: '10s', target: 0 },
      ],
      gracefulRampDown: '10s',
    },
  },
  // No thresholds: this run is meant to find where things break, so failing it
  // would be reporting the intended result as an error.
};

export default function () {
  const user = userFor(__VU);
  const res = http.post(`${API}/v1/purchases`, purchaseBody(seed, user.purchase_token, 1), {
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${user.jwt}`,
      'Idempotency-Key': idemKey('cal', __VU, __ITER),
    },
    tags: { name: 'POST /v1/purchases' },
  });
  checkPurchase(res, recordPurchase(res));
}
