// The page only tells the truth if the recording is complete and consistent, so
// this fails on a recording it could not show faithfully.
import { readFileSync } from "node:fs";
import assert from "node:assert/strict";

globalThis.matchMedia = () => ({ matches: true });
const { lostSteps, OUR_EVENTS } = await import("../site/app.js");

const data = JSON.parse(readFileSync(new URL("../site/replay.json", import.meta.url)));
const lost = data.lost_response;
const crowd = data.stampede;

for (const event of lost.events) {
  assert.ok(OUR_EVENTS[event.action], `no wording for audit action ${event.action}`);
}
const steps = lostSteps(lost);
assert.equal(steps.filter((s) => s.side === "theirs").length, 1, "the provider charged exactly once");
assert.equal(steps.at(-1).side, "ours");
assert.equal(lost.charge_status, "succeeded");
assert.equal(lost.charge_amount, lost.total_cents, "charged amount must match the booking");

assert.equal(crowd.sold, crowd.tickets, "the recorded rush must sell out");
assert.equal(crowd.claims.length, crowd.sold, "one claim per ticket sold");
assert.equal(crowd.sold + crowd.turned_away, crowd.buyers, "every buyer is accounted for");
console.log("site recording ok");
