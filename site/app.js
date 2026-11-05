// Plays back site/replay.json, which scripts/capture_replay.py records from a real
// run. Every number shown comes from that file.

const LOST_SPEED = 10;
const CROWD_DURATION_MS = 4000;

const reducedMotion = matchMedia("(prefers-reduced-motion: reduce)").matches;
const byId = (id) => document.getElementById(id);

// What each recorded audit action means, in the customer's terms.
export const OUR_EVENTS = {
  booking_created: ["Tickets set aside", "The booking is held for this customer."],
  payment_outcome_unknown: ["Payment outcome unknown", "No response came back, so the customer sees “we're checking” and nothing is released."],
  booking_confirmed: ["Booking confirmed", "A background worker asked the provider what happened, then finished the purchase."],
  payment_succeeded: ["Charge recorded, exactly once", "Both sides now agree."],
};

// Turns the recording into ordered steps. Times from the two systems come from
// different clocks, so steps are ordered by meaning and shown in whole seconds.
export function lostSteps(run) {
  const money = new Intl.NumberFormat("en-US", { style: "currency", currency: run.currency })
    .format(run.charge_amount / 100);
  const steps = [];
  for (const event of run.events) {
    const [title, detail] = OUR_EVENTS[event.action];
    if (event.action === "payment_outcome_unknown") {
      steps.push({ at: event.at_ms, side: "theirs", title: `Card charged ${money}`, detail: "The money moved. The response to our server was lost on the network." });
    }
    steps.push({ at: event.at_ms, side: "ours", title, detail, action: event.action });
  }
  return steps;
}

const wait = (ms) => new Promise((resolve) => setTimeout(resolve, reducedMotion ? 0 : ms));

function addEntry(step) {
  const item = document.createElement("li");
  const title = document.createElement("b");
  const detail = document.createElement("span");
  const time = document.createElement("time");
  title.textContent = step.title;
  detail.textContent = step.detail;
  time.textContent = ` ${Math.round(step.at / 1000)}s`;
  item.append(title, detail, time);
  byId(step.side).append(item);
}

function setStamp(state, text) {
  const stamp = byId("stamp");
  stamp.dataset.state = state;
  stamp.textContent = text;
}

function runClock(endMs) {
  if (reducedMotion) {
    byId("clock").textContent = Math.round(endMs / 1000);
    return () => {};
  }
  const started = performance.now();
  let frame = requestAnimationFrame(function tick(now) {
    const elapsed = Math.min((now - started) * LOST_SPEED, endMs);
    byId("clock").textContent = Math.round(elapsed / 1000);
    if (elapsed < endMs) frame = requestAnimationFrame(tick);
  });
  return () => cancelAnimationFrame(frame);
}

async function playLost(run) {
  const button = byId("play-lost");
  button.disabled = true;
  byId("ours").replaceChildren();
  byId("theirs").replaceChildren();
  byId("lost-verdict").hidden = true;
  setStamp("none", "");
  byId("clock").textContent = "0";

  const stopClock = runClock(run.resolved_at_ms);
  let played = 0;
  for (const step of lostSteps(run)) {
    await wait(Math.max((step.at - played) / LOST_SPEED, 700));
    played = step.at;
    addEntry(step);
    if (step.action === "booking_created") setStamp("held", "Held");
    if (step.action === "booking_confirmed") setStamp("confirmed", "Confirmed");
  }

  // A hidden tab pauses animation frames, so the clock must not depend on them.
  stopClock();
  byId("clock").textContent = Math.round(run.resolved_at_ms / 1000);

  const verdict = byId("lost-verdict");
  verdict.textContent = `The two records disagreed for ${Math.round(run.resolved_at_ms / 1000)} seconds. `
    + "In that time the system never guessed, and nobody had to retry.";
  verdict.hidden = false;
  button.disabled = false;
  button.textContent = "Replay again";
}

async function playCrowd(run) {
  const button = byId("play-crowd");
  button.disabled = true;
  byId("tally").hidden = true;
  const stubs = [...byId("stubs").children];
  stubs.forEach((stub) => stub.classList.remove("sold"));

  const last = run.claims.at(-1).at_ms || 1;
  let played = 0;
  for (const [index, claim] of run.claims.entries()) {
    const at = (claim.at_ms / last) * CROWD_DURATION_MS;
    await wait(at - played);
    played = at;
    stubs[index].classList.add("sold");
  }

  byId("sold").textContent = run.sold;
  byId("away").textContent = run.turned_away;
  byId("oversold").textContent = run.sold - run.tickets > 0 ? run.sold - run.tickets : 0;
  byId("tally").hidden = false;
  button.disabled = false;
  button.textContent = "Replay again";
}

async function start() {
  let data;
  try {
    const response = await fetch("replay.json");
    if (!response.ok) throw new Error(`status ${response.status}`);
    data = await response.json();
  } catch {
    byId("load-error").hidden = false;
    byId("play-lost").disabled = true;
    byId("play-crowd").disabled = true;
    return;
  }

  const lost = data.lost_response;
  const crowd = data.stampede;
  byId("ticket-qty").textContent = lost.quantity === 1 ? "one" : "two";
  byId("ticket-ref").textContent = `Booking ${lost.reference}`;
  byId("ticket-price").textContent = new Intl.NumberFormat("en-US", { style: "currency", currency: lost.currency })
    .format(lost.total_cents / 100);
  byId("buyers").textContent = crowd.buyers;
  byId("tickets").textContent = crowd.tickets;
  byId("crowd-ms").textContent = crowd.claims.at(-1).at_ms;
  byId("captured").textContent = data.captured_on;
  byId("stubs").replaceChildren(...crowd.claims.map((_, index) => {
    const stub = document.createElement("li");
    stub.textContent = index + 1;
    return stub;
  }));

  byId("play-lost").addEventListener("click", () => playLost(lost));
  byId("play-crowd").addEventListener("click", () => playCrowd(crowd));
}

if (typeof document !== "undefined") start();
