# Order Executor — Submission README
// Written with assistance from Claude Sonnet 4.6

## How to Run

**Prerequisites:** Go 1.25+, Python 3

**1. Start the mock exchange** (Terminal 1):

```bash
python3 mock_exchange.py
```

**2. Configure the order** — edit `cmd/caller/config.json`:

An order can look like this

```json
{
  "symbol": "BTC-USD",
  "side": "buy",
  "qty": 10,
  "slices": 5,
  "duration": "1m"
}
```

**3. Run the caller** (Terminal 2):

```bash
cd order-executor
go run ./cmd/caller/
```

You'll see live progress updates for the configured `duration`, then a final result summary. A timestamped log file (`twap_YYYYMMDD_HHMMSS.log`) is written alongside stdout for post-mortem debugging.

**4. Test clean shutdown:** Run the caller again and press `Ctrl+C` mid-execution. The coordinator cancels active orders on the exchange, reports what filled, and exits cleanly.

**5. Run tests** (no mock exchange needed):

```bash
go test ./executor/ -v
```

All 12 tests use a deterministic `FakeExchange` — no network, no timing, instant results.

---

## Architecture

The executor is an **in-process library** — the caller imports it and calls functions directly. There is no HTTP server, gRPC, or serialization between caller and executor. The exchange is the only network boundary. 

Adding a second one between caller and executor would buy nothing the rubric rewards and would add boilerplate, serialization, and error handling. This is a scope judgment call — the exchange is already the network boundary.

```
Caller (main.go)
  │
  │ creates HTTPExchange (Exchange interface implementation)
  │ creates TWAPStrategy (Strategy interface implementation)
  │ calls twap.Execute(ctx, exchange) ← Strategy interface
  │ reads <-chan ProgressUpdate
  │
  └──→ TWAPStrategy.Execute()         ← implements Strategy
         │
         │ Inside the coordinator goroutine, calls Exchange interface:
         │   exchange.PlaceOrder(...)   ← on place tick
         │   exchange.GetOrder(...)     ← on poll tick
         │   exchange.CancelOrder(...)  ← on shutdown
         │
         ├──→ HTTPExchange             ← implements Exchange (production)
         │      │
         │      │ Translates to HTTP:
         │      │   POST /orders
         │      │   GET /orders/<id>
         │      │   DELETE /orders/<id>
         │      │
         │      └──→ Mock Exchange (Python server, port 9101)
         │
         └──→ FakeExchange             ← implements Exchange (tests)
                │
                │ Returns scripted deterministic responses.
                │ No network. Instant. Controls exactly which
                │ orders fill, partial-fill, or fail.
```

The **caller** only sees the `Strategy` interface — it never calls `PlaceOrder`, `GetOrder`, or `CancelOrder` directly. It creates both objects (exchange adapter + strategy), wires them together, and reads the output channel.

The **strategy** (`TWAPStrategy`) is where the `Exchange` interface methods are called — inside its coordinator goroutine. This is the I/O separation: TWAP logic calls `exchange.PlaceOrder(...)`, and whether that's real HTTP or a test fake is invisible to it.

### Components

| File | Role |
|---|---|
| `executor/types.go` | Shared types: `ProgressUpdate`, `Order`, `PlaceOrderRequest`, `Status` constants |
| `executor/exchange.go` | `Exchange` interface — 3 methods separating TWAP logic from I/O |
| `executor/http_exchange.go` | `HTTPExchange` — real HTTP to mock exchange |
| `executor/strategy.go` | `Strategy` interface — `Execute(ctx, exchange) <-chan ProgressUpdate` |
| `executor/twap.go` | `TWAPStrategy` — coordinator goroutine with two tickers |
| `executor/fake_exchange_test.go` | `FakeExchange` — deterministic scripted responses for tests |
| `executor/twap_test.go` | 12 test cases covering all edge cases |
| `cmd/caller/main.go` | Caller script — submits one TWAP, prints progress, exits |

---

## Progress/Result Interface Design

### The design

`Execute()` returns `<-chan ProgressUpdate`. The caller reads with `for range`. The loop exits when the channel closes. The last update sent before closing is the final result.

```go
progressCh := twap.Execute(ctx, exchange)
for update := range progressCh {
    // print progress — updates arrive as fills happen
}
// loop exited — last update has terminal status and final numbers
```

There is no separate result type. The last `ProgressUpdate` on the channel carries the terminal status (`success`, `failed`, `cancelled`, or `partial`) and complete fill numbers. `for range` naturally exits on channel close — "progress done" and "final result available" are the same event.

### ProgressUpdate fields

```go
type ProgressUpdate struct {
    TotalQty     float64    // what was requested (constant)
    FilledQty    float64    // aggregate filled across all children
    AvgPrice     float64    // weighted avg: sum(childFilledQty × childAvgPrice) / totalFilledQty
    SlicesTotal  int        // how many slices the order was split into
    SlicesPlaced int        // how many POSTed to exchange
    SlicesFilled int        // how many fully filled
    Status       Status     // running | success | failed | cancelled | partial
    Errors       []string   // per-slice failure descriptions
    Logs         []string   // delta-only retry/event messages
    Timestamp    time.Time
}
```

Updates are deduplicated: the coordinator hashes the current state before sending and skips identical updates. This prevents flooding the caller with unchanged data on every 200ms poll tick.

### Why channel over alternatives

**Polling (caller calls `GetStatus()` on a loop):** 
- Pushes complexity to the caller — it manages its own loop, interval, and stop condition. 
- Requires a mutex on shared state since the coordinator writes and the caller reads concurrently. 

Works, but the caller does more work.

**Callbacks (caller passes a function to `Execute`):** 
- The callback executes inside the coordinator's goroutine. Harder to reason about, harder to test. 
- Riskier to have the caller's code running inside the coordinator's context.

**Channel (chosen):** 
- Idiomatic Go feature for this purpose.
- Coordinator pushes, caller reads. Fully decoupled. No mutex needed — the channel IS the synchronization primitive. `for range` naturally terminates on channel close. 
- Testable — inject a `FakeExchange`, call `Execute`, drain the channel, assert on what came out.

This is **push-based streaming**, which aligns with the stretch goal ("a richer progress channel, e.g. streaming updates instead of polling"). We hit it naturally because Go channels are push-based — updates arrive at the caller without the caller polling.

### How failure is reported

Failure surfaces through two fields working together:

**`Status`** distinguishes total failure (`failed` — every slice failed) from partial failure (`partial` — some slices failed, rest succeeded) from cancellation (`cancelled` — caller cancelled mid-execution). 

The caller immediately knows the category.

**`Errors`** is a `[]string` carrying per-slice descriptions, e.g., `"slice 3: failed after 3 retries: exchange error"`. The caller knows exactly which slices failed and why.

**`FilledQty` vs `TotalQty`** gives the shortfall. On `partial`, the caller sees exactly how much was left unfilled.

Example: TWAP for 10 units in 5 slices. Slices 1-3 fill. Slice 4 fails after retries. Slice 5 fills. Final update: `Status: partial`, `FilledQty: 8.0`, `TotalQty: 10.0`, `Errors: ["slice 4: failed after 3 retries"]`.

---

## Exchange Adapter

The `Exchange` interface wraps all I/O between the TWAP coordinator and the trading venue:

```go
type Exchange interface {
    PlaceOrder(ctx context.Context, req PlaceOrderRequest) (*Order, error)
    GetOrder(ctx context.Context, orderID string) (*Order, error)
    CancelOrder(ctx context.Context, orderID string) error
}
```

The coordinator calls `exchange.PlaceOrder(...)` without knowing whether it's talking to a real HTTP server or a test fake. This is the I/O separation — TWAP logic is pure algorithm, no HTTP, no JSON, no URLs.

Two implementations exist:

1. **`HTTPExchange`** — real HTTP calls to the mock exchange.

**`FakeExchange`** — scripted deterministic responses. Used in all tests. The test author controls exactly which orders fill, how they partial-fill, which fail, and when.

**Why not test against the real mock exchange?** The mock exchange fills randomly, at random prices, in random chunks. Tests would be non-deterministic, timing-dependent, and flaky. `FakeExchange` gives instant, deterministic, reliable tests.

The interface is also **strategy-agnostic**. Any future strategy (VWAP, Iceberg) uses the same three methods. No strategy needs to know the transport layer.

---

## TWAP Internals

### Concurrency model

`Execute()` spawns a single coordinator goroutine and returns `<-chan ProgressUpdate` immediately. The caller is never blocked — this is the "asynchronous execution" the rubric asks for.

The coordinator runs a `select` loop with two tickers:

```
select {
case <-ctx.Done():       // shutdown — cancel active orders, report, exit
case <-placeTicker.C:    // POST next slice to exchange
case <-pollTicker.C:     // GET status of all active orders
}
```

**`placeTicker`** fires every `duration / slices` (e.g., every 6 seconds for 30s/5 slices). On each tick, the coordinator POSTs a child order to the exchange with a `client_order_id` for idempotent retries, then adds the returned order ID to the active set.

**`pollTicker`** fires every `~200ms` (configurable). The coordinator iterates all active orders, GETs each one's status, and updates the aggregate filled quantity and weighted average price. Filled orders are removed from the active set. Cancelled orders (by the exchange) are recorded as errors.

**One goroutine owns all state** — no mutex, no races, no WaitGroup. Progress updates are pushed to the channel from this single goroutine.

Within the coordinator, HTTP calls are **synchronous blocking calls**. This is a deliberate design choice. On localhost, each call is sub-millisecond — 5 active orders means ~5ms of blocking, which is irrelevant for a TWAP running over seconds. In production, this would be the first thing to change (see "What I'd Do Next").

### Slicing and weighted average price

`total_qty` is divided evenly, with the last slice absorbing the remainder: `10 / 3` → `[3.33, 3.33, 3.34]`. No quantity is lost to rounding.

The aggregate fill price is the weighted average across all children: `sum(child_filled_qty × child_avg_price) / total_filled_qty`. This is easy to get wrong — a naive `(price1 + price2) / 2` gives incorrect results when children fill different quantities. There is a dedicated test case for this with unequal fill quantities.

### Retry logic

Each child order is placed with a `client_order_id` (e.g., `"slice-0"`, `"slice-1"`). If `PlaceOrder` fails, the coordinator retries up to `MaxRetries` times (default 3) with the same `client_order_id`. If the POST succeeded on the exchange but the response was lost, re-POSTing with the same ID returns the existing order rather than creating a duplicate.

If all retries are exhausted, the slice is dropped, an error description is added to the `Errors` field, and the coordinator continues with remaining slices. The final status is `partial` if some slices succeeded, `failed` if none did.

The mock exchange on localhost doesn't simulate server errors, so retries never fire in a live run. But retry logic is included because the rubric explicitly calls for "sensible handling of exchange errors," it enables the failed-child-order test case, and `client_order_id`-based retries protect against a real failure mode (response lost due to timeout) even on localhost.

### Error handling

**`PlaceOrder` fails:** Retry up to N times with the same `client_order_id`. If all retries fail, drop the slice, add to `Errors`, continue.

**`GetOrder` fails during polling:** Skip that order for this poll tick, try again next tick (~200ms). Don't count it as a failure — the order still exists on the exchange. We just couldn't read its status this time.

**`CancelOrder` fails during shutdown:** Best-effort with a short timeout. If the DELETE fails, proceed with shutdown and report last-known state. It is then the caller's responsibility to verify with the exchange whether orphaned orders were actually cancelled. This is an explicit tradeoff — responsive shutdown over guaranteed cancellation.

---

## Clean Shutdown

### Trigger

The caller catches `SIGINT`/`SIGTERM` via `signal.Notify` and calls `cancel()` on the context. The coordinator's `select` sees `ctx.Done()`.

### Sequence

1. **Stop placing new slices** — no more `PlaceOrder` calls.
2. **DELETE all open/partially-filled orders** on the exchange. A fresh `context.WithTimeout(context.Background(), 3s)` is used — the caller's context is already cancelled, so DELETE calls need their own deadline. Best-effort: if the exchange is unresponsive, shutdown is not blocked.
3. **Compute final state** — aggregate filled qty and avg price from what actually filled before cancellation.
4. **Send final `ProgressUpdate`** with `Status: cancelled` and accurate fill numbers.
5. **Close the channel** — the caller's `for range` exits cleanly.

```plaintext
Ctrl+C / SIGTERM ─→ sigCh ─→ cancel()
                                 │
                                 ▼
                            ctx.Done() closes
                                 │
              ┌──────────────────┴───────────────────┐
              ▼                                       ▼
   coordinator's select fires            any in-flight PlaceOrder/GetOrder
   ctx.Done() case → shutdown            HTTP call aborts
```
6. **Coordinator goroutine returns.**

### Why DELETE active orders?

Orphaned orders with no observer keep filling on the exchange and cause unexpected positions. Cancelling active orders on shutdown is the safe default. `DELETE` doesn't undo already-filled quantity — it only stops an open order from filling further.

---

## Tests

All 12 tests use `FakeExchange` for deterministic, instant execution. No real HTTP, no timing dependency, no flakiness.

| # | Test | What it verifies |
|---|---|---|
| 1 | `TestComputeSliceQtys` | Slicing math: sum equals total, every slice positive, first n-1 are the even split, last absorbs the remainder |
| 2 | `TestWeightedAvgPrice` | Weighted avg: `(3×100 + 7×200) / 10 = 170`, not naive `(100+200)/2 = 150` |
| 3 | `TestPartialFills` | Order fills in multiple chunks; intermediate progress reflects partial state |
| 4 | `TestFailedChildOrder` | Retry fires, one slice dropped, `status=partial`, shortfall in `FilledQty` |
| 5 | `TestAllSlicesFail` | Every slice fails → `status=failed` (not partial), `FilledQty=0`, one error per slice |
| 6 | `TestCleanShutdown` | Context cancelled → `CancelOrder` called → `status=cancelled` |
| 7 | `TestHappyPath` | All slices fill → `status=success`, `FilledQty == TotalQty` |
| 8-12 | Sell-side mirrors | Same 5 scenarios for `side="sell"` to verify both directions |

Tests 3-6 directly cover the rubric's explicitly requested edge cases: partial fills, failed child orders (partial and total failure), and shutdown.

---

## Extensibility

The system is extensible along three axes without reworking existing code:

**New strategy types** (VWAP, Iceberg): Create a new struct implementing `Strategy` with its own `Execute` method. The caller, the progress channel type, and the exchange adapter don't change. Everything downstream is strategy-agnostic.

**Child order type** (market vs limit TWAP): The `TWAPStrategy` struct has an `OrderType` field (defaulting to `"market"`). The coordinator can branch inside `Execute` based on this field. To support limit orders, add `GetBBO` to the `Exchange` interface (currently omitted since market TWAP doesn't need it), a `LimitPrice` field to `PlaceOrderRequest`, and the branching logic. Not a new strategy — a parameter on the existing one.

**Progress channel is strategy-agnostic**: The caller reads `<-chan ProgressUpdate` regardless of which strategy runs. Same channel type, same `for range` loop, same final result shape.

---

## What I'd Do Next

1. **Spawn HTTP calls as goroutines (highest priority).** Currently, `PlaceOrder` and `GetOrder` calls are synchronous blocking calls inside the coordinator's `select` loop. On localhost this is sub-millisecond, but with real network latency (50ms per call × 10 active orders = 500ms blocking), the select loop stalls and place ticks get delayed. The fix: spawn HTTP calls as goroutines that send results back on a channel so the coordinator stays responsive. This reintroduces coordination complexity (result channels, error handling across goroutines), which is why it's a deliberate simplification for this assignment's scope.

2. **Limit-order TWAP.** Use `GetBBO` to read the current bid/ask, place limit orders at or near the market instead of market orders. Reduces slippage further but adds complexity around unfilled resting orders and price drift.

2. **Executor manager.** A struct that tracks multiple concurrent strategy runs with unified shutdown. The caller submits strategies to the manager; it manages contexts, aggregates status, and provides a single shutdown point across all running strategies.

4. **Metrics and observability.** Structured logging, latency histograms for exchange calls, fill-rate tracking.

---

## Go Idioms for Non-Go Readers

This section briefly explains Go-specific patterns used throughout the codebase, for evaluators who may be more familiar with Python or other languages.

**Goroutine** — a lightweight concurrent function launched with the `go` keyword. Cheaper than OS threads — Go's runtime multiplexes thousands onto a few threads. `go doWork()` starts it; it runs concurrently with the caller.

**Channel (`chan T`)** — a typed, thread-safe pipe for passing values between goroutines. The sender pushes with `ch <- value`, the receiver reads with `value := <-ch`. `for update := range ch` reads until the channel is closed. Channels are the primary synchronization mechanism in Go — "don't communicate by sharing memory; share memory by communicating."

**`context.Context`** — a standard library type that carries cancellation signals and deadlines across API boundaries. Functions accept a `ctx` parameter, and when the caller calls `cancel()`, every function holding that context can detect it via `ctx.Done()`. This is Go's idiomatic way to propagate "stop everything" through a call chain — from the caller through the coordinator into every HTTP call.

**`select`** — a control structure that listens on multiple channels simultaneously and executes whichever case fires first. It's the backbone of event-loop-style designs in Go — "wait for a timer tick, a cancellation signal, or a result, whichever comes first."

**Interface** — an implicitly satisfied contract. Any struct that implements the methods of an interface satisfies it automatically — no `implements` keyword needed. This makes it trivial to swap implementations (e.g., `HTTPExchange` for production, `FakeExchange` for tests) without changing the consumer.
