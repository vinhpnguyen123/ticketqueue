// Shared response validators and the metric separation that makes the numbers
// mean something.
import { Trend, Counter } from 'k6/metrics';
import { check } from 'k6';

// Why four metrics instead of one: in a contention run roughly 99% of requests
// are rejected by the Redis fast path in a couple of milliseconds. A p(95) taken
// over all requests is therefore a p(95) of REJECTIONS — a fast, flattering,
// meaningless number. These are kept apart so the headline latency is the
// latency of an actual completed purchase.
export const completedPurchase = new Trend('purchase_completed_duration', true);
export const rejectedSoldOut = new Trend('purchase_rejected_duration', true);
export const ambiguous = new Trend('purchase_ambiguous_duration', true);
export const outcomes = new Counter('purchase_outcomes');

// recordPurchase classifies one response and returns its outcome label.
//
// 409 sold_out and 504 payment_processing_unclear are EXPECTED outcomes, not
// failures: the first is the fast path doing its job, the second is the system
// refusing to guess whether money moved. Only a 5xx is a real failure.
export function recordPurchase(res) {
  let outcome = 'unexpected';

  if (res.status === 201) {
    outcome = 'confirmed';
    completedPurchase.add(res.timings.duration);
  } else if (res.status === 409) {
    outcome = errorType(res) === 'sold_out' ? 'sold_out' : 'conflict';
    rejectedSoldOut.add(res.timings.duration);
  } else if (res.status === 504) {
    outcome = 'ambiguous';
    ambiguous.add(res.timings.duration);
  } else if (res.status === 402) {
    outcome = 'declined';
  } else if (res.status === 410) {
    outcome = errorType(res) === 'purchase_window_expired' ? 'window_expired' : 'duplicate_blocked';
  } else if (res.status === 422) {
    outcome = 'validation_failed';
  } else if (res.status >= 500) {
    outcome = 'server_error';
  }

  outcomes.add(1, { outcome: outcome });
  return outcome;
}

export function errorType(res) {
  try {
    return res.json().error.type;
  } catch (e) {
    return '';
  }
}

// checkPurchase asserts the response is one of the outcomes this system is
// allowed to produce. A 5xx fails the check; an expected rejection does not.
export function checkPurchase(res, outcome) {
  return check(res, {
    'purchase answered without a server error': (r) => r.status < 500,
    'purchase outcome is one the system defines': () =>
      ['confirmed', 'sold_out', 'ambiguous', 'declined', 'window_expired', 'duplicate_blocked'].includes(outcome),
  });
}

export function checkOK(res, name) {
  return check(res, { [name]: (r) => r.status >= 200 && r.status < 300 });
}
