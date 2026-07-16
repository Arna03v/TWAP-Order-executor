# Schedule-Fidelity Metrics

Two metrics added to the TWAP executor that quantify how faithfully the child
orders tracked the *ideal uniform TWAP schedule*. They measure the part of the
algorithm the executor actually controls — placement cadence — rather than fill
price (which, against the random-fill mock exchange, would measure the mock's
RNG rather than the strategy).

Both are surfaced on the terminal `ProgressUpdate` and printed by the caller:

```
[result] schedule: deviation_from_ideal=0.48% max_interval_drift=0.9ms
```

---

## The metrics

### `max_interval_drift` (ms) — `ProgressUpdate.MaxIntervalDriftMs`

The largest deviation of an actual gap between two consecutive placements from
the target inter-slice interval.

> *"In the worst case, how far off-cadence was any single child order?"*

- Target interval = `Duration / Slices` (e.g. 10s over 50 slices → 200ms).
- For each pair of consecutive placements, `gap = placeTimes[i] - placeTimes[i-1]`.
- `drift_i = |gap - targetInterval|`, and the metric is `max` over all `i`, in ms.

### `deviation_from_ideal` (%) — `ProgressUpdate.ScheduleDeviationPct`

The mean absolute deviation of each placement from its ideal scheduled time,
expressed as a percentage of the target interval.

> *"On average, how tightly did execution track the schedule?"*

- The i-th placement (0-indexed) ideally lands at `execStart + (i+1)·interval`.
  The `(i+1)` is because a Go `time.Ticker` first fires one full interval after
  it is created, not immediately.
- `dev_i = |placeTimes[i] - idealTime_i|`.
- Metric = `mean(dev_i / targetInterval) · 100`.

---

## How it's calculated

All of it derives from one recorded slice: the wall-clock time each child order
was placed. No changes to placing, polling, fills, retries, or shutdown.

- `execStart` — captured at the top of the coordinator goroutine, anchors the
  ideal schedule.
- `placeTimes []time.Time` — one `time.Now()` appended each time a slice is
  actually handled in the `placeTicker` case.
- `computeScheduleMetrics(execStart, placeTimes, targetInterval)` — a pure
  function (no I/O, no clock reads) called once before each terminal update
  (both the completion path and the `ctx.Done()` shutdown path). Returns
  `(maxIntervalDriftMs, scheduleDeviationPct)`.

Pseudocode:

```go
func computeScheduleMetrics(execStart, placeTimes, targetInterval) (driftMs, devPct float64) {
    if len(placeTimes) == 0 || targetInterval <= 0 { return 0, 0 }
    target := float64(targetInterval)

    // max interval drift — consecutive gaps vs target
    for i := 1; i < len(placeTimes); i++ {
        gap := placeTimes[i] - placeTimes[i-1]
        driftMs = max(driftMs, |gap - target| / 1ms)
    }

    // mean cumulative deviation from ideal, as % of interval
    sum := 0.0
    for i, ts := range placeTimes {
        ideal := execStart + (i+1)*targetInterval
        sum += |ts - ideal| / target
    }
    devPct = sum / len(placeTimes) * 100
}
```

**Files touched:** `executor/twap.go` (helper + wiring), `executor/types.go`
(two `ProgressUpdate` fields), `cmd/caller/main.go` (result line),
`executor/twap_test.go` (`TestComputeScheduleMetrics`).

**Test:** `TestComputeScheduleMetrics` checks a perfect schedule (0/0), a single
20ms-late placement (20ms drift, 6.67% deviation), and degenerate inputs (empty
timeline, zero interval → no panic / divide-by-zero).

---

## Measured numbers (localhost mock)

Collected by running `mock_exchange.py` and the caller across several configs:

| Slices | Duration | Target interval | `deviation_from_ideal` | `max_interval_drift` |
|-------:|---------:|----------------:|-----------------------:|---------------------:|
| 5      | 30s      | 6000 ms         | 0.01%                  | 0.9 ms               |
| 5      | 5s       | 1000 ms         | 0.09%                  | 0.5 ms               |
| 10     | 5s       | 500 ms          | 0.15%                  | 0.8 ms               |
| 20     | 10s      | 500 ms          | 0.16%                  | 0.8 ms               |
| 50     | 10s      | 200 ms          | 0.48%                  | 0.9 ms               |

Across every config: **< 0.5% deviation from the ideal schedule, < 1 ms max
interval drift.**

### Caveat — these are localhost numbers

The drift is near-zero because Go's `time.Ticker` is highly accurate and the
mock exchange's HTTP calls are sub-millisecond, so the synchronous calls inside
the coordinator's `select` loop never stall it. These are honest
*demonstration* numbers, not a production-scale claim.

Where the metric becomes interesting: with real network latency, a blocking
`PlaceOrder`/`GetOrder` would stall the `select` loop past the next tick and
drift would spike. Moving HTTP calls off the loop (see README → "What I'd Do
Next" #1) would bring it back down — a genuine before/after story.
