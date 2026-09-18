# Week 8 README assets

Text captures are committed here verbatim (ANSI stripped). The two Grafana
panels still need a human with a browser; each link below has the run's exact
window pinned in `from`/`to`, so the capture can be taken any time within
Prometheus' 15-day retention and will show precisely the seconds the run
covered. Save each as the filename given.

## Captured

| File | What it is |
|---|---|
| `k6-oversell-500vu-100tickets-all-thresholds-pass.txt` | k6 summary, 500 VUs vs 100 tickets — **every threshold green**, p95 815 ms, 100/100 sold |
| `k6-oversell-run{1,2,3}-2000vu-100tickets.txt` | k6 summaries for the three 2,000-VU oversell runs (correctness thresholds green; the 2 s latency threshold fails at this VU level, by design of the hardware, see the report) |
| `k6-latency-2000vu-2000tickets.txt` | k6 summary for the latency proof, with p50/p95/p99 |
| `verify-run-all-checks-pass.txt` | `verify_run.sql` after the 500-VU run, all nine PASS |
| `verify-run-oversell-run{1,2,3}.txt` | `verify_run.sql` after each oversell run |
| `verify-run-latency-2000tickets.txt` | `verify_run.sql` after the latency run |

## To capture — Grafana, `http://localhost:3000/d/ticketqueue/ticketqueue`

Open the link, collapse the other rows, screenshot the named row.

| Save as | Row | Link (window pinned) |
|---|---|---|
| `grafana-row2-latency-by-phase-2000vu.png` | Row 2 — Latency breakdown, during the 2,000-VU latency proof | `http://localhost:3000/d/ticketqueue/ticketqueue?from=1789774700000&to=1789774735000` |
| `grafana-row2-latency-by-phase-peak-8000vu.png` | Row 2 during the 8,000 → 10,000 VU calibration steps (phase p95 is bucket-capped at 10 s here — say so in the caption) | `http://localhost:3000/d/ticketqueue/ticketqueue?from=1789761000000&to=1789761120000` |
| `grafana-row3-pool-saturation-2000vu.png` | Row 3 — Contention: `db_pool_in_use` pinned at 10 with acquire-wait climbing. Not on the original list; it is the picture of §4 in the report | `http://localhost:3000/d/ticketqueue/ticketqueue?from=1789774700000&to=1789774735000` |
| `grafana-row4-money-recovery-chaos2.png` | Row 4 — Money & recovery during CHAOS 2: `bookings_held_current` up to 500 then draining, `charged_not_delivered` flat at 0. **The most persuasive image in the project.** | `http://localhost:3000/d/ticketqueue/ticketqueue?from=1789773804000&to=1789774048000` |
| `grafana-row2-latency-chaos1-api-kill.png` | Row 2 during CHAOS 1 — where time went while the API was dead | `http://localhost:3000/d/ticketqueue/ticketqueue?from=1789773474000&to=1789773860000` |
