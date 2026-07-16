# Order Executor — Master Design Document

## Overview

This document captures every architecture and design decision for the Order Executor take-home. It is the single source of truth — the submission README and the code agent's implementation guide are both derived from it.

The system has three components:

```
Caller (Go script)  ←—function calls—→  Executor (Go package)  ←—HTTP—→  Mock Exchange (Python server)
```

The caller imports the executor as a library, submits one TWAP order, reads progress from a channel, prints updates to stdout, prints the final result, and exits.

---

## Decision 1 — Language: Go

**Choice:** Go (latest stable).

**Reasons, mapped to rubric criteria:**

| Rubric criterion | Why Go fits |
|---|---|
| Async TWAP with progress (1) | Goroutines + channels are the most natural model for background execution with streamed progress. No event loop, no async/await coloring. |
| Progress/result interface (2) | `<-chan ProgressUpdate` is idiomatic push-based streaming. The channel IS the synchronization — no mutex between producer and consumer. |
| Exchange adapter (3) | Go interfaces are first-class. Define `Exchange` with 4 methods, swap implementations for production vs test. No frameworks. |
| Shutdown (4) | `context.Context` is purpose-built for cancellation propagation. One mechanism flows from caller through coordinator into every HTTP call. |
| Tests, clarity, scope (5) | Built-in `testing` package, table-driven test idiom, no external deps needed. |

**Additional:** The candidate knows Go well and can walk through every line in a live interview. Python was considered but rejected — candidate doesn't know it, and the Go concurrency primitives are a better fit for this problem.

**README note for evaluators:** The README should briefly explain Go idioms (channels, context, interfaces, goroutines) for evaluators who may be Python-native.

---

## Decision 2 — Concurrency Model

### 2a. Process Boundary: In-Process Library

The executor is a Go package the caller imports and calls via functions. There is no network layer between caller and executor — no HTTP server, no gRPC, no serialization.

**Why:** The exchange is already the network boundary. Adding a second one between caller and executor buys nothing the rubric rewards and adds server boilerplate, serialization, and network error handling. It also makes testing dramatically simpler. This is a good scope judgment call (criterion 5).

### 2b. Single Coordinator Goroutine

When `Execute()` is called, it spawns a single coordinator goroutine that runs a `select` loop with two tickers:

```
select {
case <-ctx.Done():
    // shutdown: cancel active orders, compute final state, send final update, exit
case <-placeTicker.C:
    // POST next slice to exchange, record order ID
    // stop ticker after all slices placed
case <-pollTicker.C:
    // for each active order ID: GET status from exchange
    //   filled → update aggregate, remove from active list
    //   partially_filled → update aggregate, keep polling
    //   failed → increment retry count, re-POST or drop
}
```

- `placeTicker` fires every `duration / slices` interval.
- `pollTicker` fires every ~200ms (matching the mock exchange's internal tick rate).

**What this buys:**

- **Zero shared state.** One goroutine owns all state — no mutex, no races, no WaitGroup. Progress updates are pushed to a channel from this single goroutine.
- **Trivial shutdown.** One goroutine to cancel. It runs through active orders, DELETEs them, sends final result, exits.
- **All retry logic in one place.** The poll handler sees a failure, checks the retry map, acts.
- **Easy to explain in an interview.** "It's a single event loop with two timers."

### 2b-i. What "Asynchronous" Means in This Design

The rubric says "TWAP executes asynchronously." This refers to the **caller-executor boundary**: `Execute()` spawns the coordinator goroutine and returns a channel immediately. The caller is never blocked — it reads progress concurrently while the TWAP is actively placing and polling orders in the background. The caller can cancel mid-flight via context. This is the async execution the rubric requires.

**Within** the coordinator goroutine itself, operations are sequential — HTTP calls block until a response returns. This is a **deliberate design choice**, not an oversight. A single-threaded event loop with blocking I/O gives us zero shared state, no mutexes, trivial shutdown, and code that's simple to explain. The async property lives at the caller boundary, not inside the coordinator.

**Tradeoff acknowledged:** When the coordinator calls `exchange.GetOrder(...)` for each active order during a poll tick, those are regular function calls that block until the HTTP response returns. While blocked, the select loop cannot process other events (e.g., a place tick that fires during the I/O wait).

On localhost this is sub-millisecond per call, so even with 5 active orders the total blocking time is ~5ms — a place tick delayed by 5ms is irrelevant for a TWAP running over seconds or minutes.

**This is one of the most important changes for production.** With real network latency (50ms per call × 10 active orders = 500ms blocking), the delay would be significant. The fix: spawn HTTP calls as goroutines that send results back on a channel, so the select loop stays responsive. This reintroduces coordination complexity (shared state, result channels, error handling across goroutines), which is why it's a deliberate simplification for the localhost mock. Listed under "What I'd Do Next" as a high-priority production change.

### 2c. Context Flow

`context.Context` with a cancel function is the single shutdown mechanism. The caller creates a context, passes it to `Execute(ctx, exchange)`. The coordinator's select includes `ctx.Done()`. The same context is threaded into every exchange HTTP call via the adapter, so calls fail fast on cancellation.

No separate done channels, no additional signaling. One mechanism.

---

## Decision 3 — Progress/Result Interface

### Design

`Execute()` returns `<-chan ProgressUpdate`. The caller reads with `for range`. The loop exits when the channel closes. The last update sent before closing is the final result.

```go
progressCh := twap.Execute(ctx, exchange)
for update := range progressCh {
    // print progress
}
// loop exited — last update has terminal status and final numbers
```

### ProgressUpdate Fields

```go
type ProgressUpdate struct {
    TotalQty     float64           // what was requested (constant)
    FilledQty    float64           // aggregate filled across all children
    AvgPrice     float64           // weighted average fill price
    SlicesTotal  int               // how many slices the order was split into
    SlicesPlaced int               // how many POSTed to exchange
    SlicesFilled int               // how many fully filled
    Status       Status            // running | completed | failed | cancelled | partial
    Errors       []string          // slice-level failure descriptions
    Timestamp    time.Time
}
```

`IsFinal` is not needed — the `for range` loop exits on channel close, and the terminal statuses (completed/failed/cancelled/partial) already indicate finality. The last update received before the channel closes is the final result.

**Status values:**

- `running` — execution in progress
- `completed` — all slices filled successfully
- `failed` — execution could not complete (all slices failed)
- `cancelled` — context was cancelled, active orders were DELETEd
- `partial` — some slices succeeded, some failed after retries. FilledQty < TotalQty but > 0.

### How Failure is Reported

Failure surfaces through two fields working together:

- **`Status`** — distinguishes total failure (`failed`) from partial failure (`partial`) from cancellation (`cancelled`). The caller immediately knows the category.
- **`Errors`** — a `[]string` carrying slice-level descriptions, e.g., `"slice 3: failed after 3 retries: exchange returned 500"`. The caller knows which slices failed and why.
- **`FilledQty` vs `TotalQty`** — the shortfall is `TotalQty - FilledQty`. On `partial`, the caller sees exactly how much was left unfilled.

Example: TWAP for 10 units in 5 slices. Slices 1-3 fill. Slice 4 fails after retries. Slice 5 fills. Final update: `Status: partial`, `FilledQty: 8.0`, `TotalQty: 10.0`, `Errors: ["slice 4: failed after 3 retries"]`.

### Justification: Why Channel Over Alternatives

**Polling (caller calls `GetStatus()` on a loop):**
- Pushes complexity to the caller — caller manages its own loop, interval, and stop condition.
- Requires a mutex on shared state since the coordinator writes and the caller reads concurrently.
- Works, but the caller does more work.

**Callbacks (caller passes a function to Execute):**
- Tight coupling. The callback runs inside the coordinator's goroutine.
- Harder to reason about, harder to test.
- Caller's code executing inside the coordinator's context is a footgun.

**Channel (chosen):**
- Coordinator pushes, caller reads. Fully decoupled.
- No mutex needed — the channel IS the synchronization primitive.
- `for range` naturally terminates on channel close — "progress done" and "final result available" are the same event.
- Idiomatic Go.
- Testable — inject a fake exchange, call Execute, drain the channel, assert on what came out.

### Stretch Goal Alignment

The rubric's stretch goal says: "a richer progress channel (e.g. streaming updates instead of polling)." Our channel design IS push-based streaming — updates arrive at the caller without the caller polling. We hit the stretch goal naturally because Go channels are push-based. This should be called out in the submission README.

### Deduplication

Before sending an update on the channel, the coordinator hashes the current state and compares to the last-sent hash. If identical, the update is skipped. This prevents flooding the caller with identical updates on every poll tick when nothing has changed.

### Logging

All progress updates printed to stdout by the caller are also written to a log file. This serves two purposes:

1. **Debugging** — if something goes wrong during execution, the full history of progress updates is available for post-mortem analysis without needing to reproduce the run.
2. **Auditability** — every state transition (slice placed, partial fill, retry, failure, shutdown) is recorded with timestamps.

The log file is plain text, written by the caller (not the executor). This keeps logging out of the executor package (no I/O side effects in the library) and avoids conflating persistence with the non-goal of databases — a log file is ephemeral debugging output, not durable storage.

---

## Decision 4 — Exchange Adapter

### Purpose

The adapter wraps the HTTP boundary between the executor and the mock exchange. It keeps all HTTP, JSON marshaling, and URL construction out of the TWAP logic. The TWAP coordinator calls `exchange.PlaceOrder(...)` without knowing whether it's talking to a real HTTP server or a test fake.

It serves two purposes:

1. **Clean code** (rubric criterion 3) — TWAP logic is pure algorithm, no I/O.
2. **Testability** (rubric criterion 5) — swap in a fake exchange for deterministic unit tests.

### Interface

```go
type Exchange interface {
    PlaceOrder(ctx context.Context, req PlaceOrderRequest) (*Order, error)
    GetOrder(ctx context.Context, orderID string) (*Order, error)
    CancelOrder(ctx context.Context, orderID string) error
    GetBBO(ctx context.Context, symbol string) (*BBO, error)
}
```

The interface is **strategy-agnostic**. Any future strategy (VWAP, Iceberg) uses the same four methods. No strategy needs to know the transport layer.

### Implementations

**`HTTPExchange`** — real HTTP calls to the mock exchange. Used by the caller. Handles JSON marshaling/unmarshaling, URL construction, error mapping. Respects context for cancellation and timeouts.

**`FakeExchange`** — scripted deterministic responses. Used in tests. The test author controls exactly which orders fill, how they partial-fill, which ones fail, and when.

### Why Not Test Against the Real Mock Exchange

- **Non-deterministic** — fills happen randomly at random prices with random chunk sizes. Can't assert exact values.
- **Timing-dependent** — fills take real wall-clock time. Test suite would take minutes.
- **Flaky** — sometimes fast, sometimes slow. Tests break intermittently.

FakeExchange gives instant, deterministic, reliable tests.

### Who Calls What

Two interfaces exist in the system. The caller touches one, the strategy touches the other:

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
         │   exchange.GetBBO(...)       ← not used for market TWAP, available for limit
         │
         └──→ HTTPExchange             ← implements Exchange
                │
                │ Translates to HTTP:
                │   POST /orders
                │   GET /orders/<id>
                │   DELETE /orders/<id>
                │   GET /bbo?symbol=...
                │
                └──→ Mock Exchange (Python server, port 9101)
```

The **caller** only sees the Strategy interface. It never calls PlaceOrder/GetOrder/CancelOrder directly. It creates both objects, wires them together, and reads the output channel.

The **strategy** (TWAPStrategy) is where the Exchange interface methods are called — inside its coordinator goroutine. This is the I/O separation: TWAP logic calls `exchange.PlaceOrder(...)`, and whether that's real HTTP or a test fake is invisible to it.

```go
// Caller
exchange := NewHTTPExchange("http://127.0.0.1:9101")
twap := &TWAPStrategy{Symbol: "BTC-USD", Side: "buy", TotalQty: 10, ...}
progressCh := twap.Execute(ctx, exchange)
```

---

## Decision 5 — TWAP Internals

### TWAPStrategy Struct

```go
type TWAPStrategy struct {
    Symbol    string        // e.g. "BTC-USD"
    Side      string        // "buy" or "sell"
    TotalQty  float64       // total quantity to execute
    Duration  time.Duration // spread execution over this window
    Slices    int           // number of child orders
    OrderType string        // "market" (default) or "limit" — future extension
}
```

### Slicing

`total_qty` is divided by `slices`. The last slice absorbs the remainder.

Example: 10 qty / 3 slices → 3.33, 3.33, 3.34.

### Child Order Type

Market orders for this assignment. The `TWAPStrategy` struct has an `OrderType` field (defaulting to `"market"`) that could be set to `"limit"` for a limit-based TWAP. The coordinator branches inside `Execute` based on this field. This is future extensibility axis #2 (see Decision 7).

### Idempotent Retries with `client_order_id`

Each child order is placed with a `client_order_id`. If POST succeeds on the exchange but the response is lost (network timeout), re-POSTing with the same `client_order_id` returns the existing order instead of creating a duplicate. The `PlaceOrderRequest` struct has an optional `ClientOrderID` field — any strategy can use it.

### Retry Logic

Retry count is configurable via environment variable or struct field. The coordinator tracks retry count per slice. On failure:

1. Retry up to N times with the same `client_order_id`.
2. If all retries fail, log the failure for that slice.
3. Continue with remaining slices.
4. Report shortfall in the final result (status = `partial` if some slices succeeded).

**Why retries matter even against a localhost mock that doesn't simulate errors:**

1. **Rubric alignment** — "sensible handling of exchange errors" is explicitly listed under the Should tier. Retry logic is the clearest demonstration of error handling.
2. **Enables test case #3** — FakeExchange simulates failures. Without retry logic, there's nothing to test. The FakeExchange returns an error, the coordinator retries, FakeExchange fails again, the coordinator drops the slice and reports the shortfall. This is one of the "tricky edge cases" the rubric calls out.
3. **`client_order_id` and retries are linked** — the case they protect against (POST succeeds on the exchange but the response is lost due to timeout) is a real failure mode even on localhost when HTTP calls have a timeout context. Without retry, the coordinator would think the slice failed when it was actually placed successfully, with no way to recover. Re-POSTing with the same `client_order_id` discovers the existing order instead of creating a duplicate.

Retries don't improve runtime behavior against the mock, but they improve the rubric score, enable a key test case, and demonstrate sound design thinking.

### Aggregate Price Calculation

The weighted average fill price across all children is:

```
avg_price = sum(child_filled_qty × child_avg_price) / total_filled_qty
```

This is easy to get wrong — it has a dedicated test case.

### Timeout on HTTP Calls

Each HTTP call to the exchange uses a context with a timeout to prevent a hung network call from blocking the coordinator's select loop indefinitely.

---

## Decision 6 — Shutdown Semantics

### Trigger

The caller cancels the context (e.g., on SIGINT/SIGTERM). The coordinator's `select` sees `ctx.Done()`.

### Sequence

1. **Stop placing new slices** — the placeTicker is effectively ignored.
2. **DELETE all open/partially-filled orders on the exchange** — prevents orphaned orders that would keep filling with no observer.
3. **Compute final state** — aggregate filled qty and avg price from what actually filled before cancellation.
4. **Send final ProgressUpdate** with `Status: cancelled` and accurate fill numbers.
5. **Close the channel** — the caller's `for range` exits cleanly.
6. **Return** — coordinator goroutine exits.

### Why DELETE Active Orders

Orphaned orders with no observer keep filling on the exchange and cause unexpected positions. In a real trading system this is how you get unintended risk. Cancelling active orders on shutdown is the safe default.

### Best-Effort Cancellation

DELETE calls use a short timeout. If the exchange is unresponsive, shutdown is not blocked — the DELETEs are best-effort. The final ProgressUpdate reports what was known at the time of cancellation.

**Responsibility boundary:** If a DELETE fails (exchange unresponsive), the coordinator still exits and reports its last-known state. It is then the **caller's responsibility** to verify with the exchange whether orphaned orders were actually cancelled. This is an explicit tradeoff — we prioritize responsive shutdown over guaranteed cancellation. The caller receives enough information (order IDs, fill state) to follow up manually if needed. This tradeoff should be documented in the submission README.

---

## Decision 7 — Extensibility

### Strategy Interface

```go
type Strategy interface {
    Execute(ctx context.Context, exchange Exchange) <-chan ProgressUpdate
}
```

### Three Extensibility Axes

**Axis 1 — New strategy types (e.g., VWAP, Iceberg):**
Create a new struct implementing `Strategy` with its own `Execute` method. No changes to the caller, the progress channel type, or the exchange adapter. Everything downstream is strategy-agnostic.

**Axis 2 — Child order type within a strategy (e.g., market vs limit TWAP):**
An `OrderType` field on the `TWAPStrategy` struct. The coordinator branches inside `Execute`. Not a new strategy — a parameter on the existing one.

**Axis 3 — Progress channel is strategy-agnostic:**
The caller reads `<-chan ProgressUpdate` regardless of which strategy is running. TWAP, VWAP, Iceberg — same channel type, same `for range` loop, same final result shape.

### What We're NOT Building

For this assignment, the caller creates the strategy directly. There is no `Executor` wrapper struct. The Strategy interface is the top-level API.

---

## Test Cases

All tests use `FakeExchange` for deterministic, instant execution. No real HTTP, no timing dependency.

| # | Test case | What it verifies | Rubric |
|---|---|---|---|
| 1 | Aggregate weighted avg price | `sum(child_filled_qty × child_avg_price) / total_filled_qty` computed correctly across multiple children | Criterion 1 |
| 2 | Partial fills | Child order fills in multiple chunks; progress updates reflect intermediate filled_qty and avg_price | Criterion 1 |
| 3 | Failed child order | Retry logic fires, fallthrough to failure, shortfall reported in final result with status `partial` | Criterion 2 |
| 4 | Clean shutdown mid-execution | Context cancelled; active orders DELETEd on exchange; final update has correct filled qty and status `cancelled` | Criterion 4 |
| 5 | Happy path — all slices fill | All slices placed and filled; final status `completed`; filled_qty equals total_qty | Criterion 1 |
| 6 | Slicing math with remainder | `total_qty / slices` produces correct per-slice quantities; last slice absorbs remainder | Criterion 1 |

---

## What I'd Do Next

If given more time beyond the ~5 hour cap:

1. **Async HTTP calls (highest priority)** — Spawn POST/GET calls to the exchange as goroutines so blocking I/O doesn't delay tickers in the coordinator's select loop. This is a deliberate simplification for the localhost mock (sub-millisecond latency) but is the single most important change for production — real network latencies would cause the coordinator's event loop to stall, delaying slice placement and progress updates. The current synchronous design is explicitly chosen for code clarity and simplicity within this assignment's scope.

2. **Executor manager** — A struct that tracks multiple concurrent strategy runs with unified shutdown. The caller submits strategies to the manager; it manages contexts, aggregates status, and provides a single shutdown point.

3. **Limit-order TWAP** — Use `GetBBO` to get the current market price, place limit orders at or near the bid/ask instead of market orders. Reduces slippage further but adds complexity around unfilled resting orders.

4. **Metrics and observability** — Structured logging, latency histograms for exchange calls, fill-rate tracking.

---

## Rubric Alignment Summary

| Criterion | Requirement | How our design addresses it |
|---|---|---|
| 1 | Correct async TWAP with accurate partial-fill progress | Single coordinator polls all active orders, updates aggregate on every change. Weighted avg price calculation has a dedicated test. Remainder handling tested. |
| 2 | Sound progress/result interface with failure reporting | `<-chan ProgressUpdate` — push-based streaming, decoupled, no mutex. Justified over polling and callbacks. Failure surfaces via `Status` field (failed/partial) and `Errors` slice. Hits stretch goal naturally. |
| 3 | Clean exchange adapter separating logic from I/O | `Exchange` interface with 4 methods. TWAP logic never sees HTTP/JSON. Two implementations: HTTPExchange for production, FakeExchange for tests. |
| 4 | Sensible shutdown | Context cancellation → stop new slices → DELETE active orders (prevent orphans) → compute final state → send cancelled update → close channel. One path, one mechanism. |
| 5 | Tests, code clarity, good scope judgment | 6 targeted test cases covering tricky edge cases. Single-goroutine design is simple to read and explain. In-process library, no unnecessary layers. No frontend, no persistence, no extra order types. |

---

## Non-Goals Verification

| Non-goal | Status |
|---|---|
| Real exchange auth | Not present — no auth anywhere |
| Persistence / DB | Not present — all in-memory, single run |
| Production frontend | Not present — terminal-only caller |
| Latency micro-optimisation | Not present — blocking HTTP in select loop, explicitly acknowledged as fine for localhost |
| Order types beyond TWAP | Not present — TWAP only, extensibility is structural not implemented |

---

## Component Summary

| Component | What it is | Key files |
|---|---|---|
| `Exchange` interface | 4-method adapter separating TWAP logic from exchange I/O | `exchange.go` |
| `HTTPExchange` | Real HTTP implementation of Exchange | `http_exchange.go` |
| `FakeExchange` | Deterministic test implementation of Exchange | `fake_exchange_test.go` |
| `Strategy` interface | Top-level API — `Execute(ctx, exchange) <-chan ProgressUpdate` | `strategy.go` |
| `TWAPStrategy` | TWAP implementation — coordinator goroutine with two tickers | `twap.go` |
| `ProgressUpdate` | Progress/result struct sent on the channel | `types.go` |
| `cmd/caller/main.go` | Caller script — submits one TWAP, prints progress, prints result, exits | `cmd/caller/main.go` |

---
## Go idioms briefly explaned


1. **Goroutine** — A lightweight concurrent function launched with the go keyword. Cheaper than OS threads — Go's runtime multiplexes thousands of goroutines onto a few threads. go doWork() starts it; it runs concurrently with the caller.

2. **Channel (chan T)** — A typed, thread-safe pipe for passing values between goroutines. The sender pushes with ch <- value, the receiver reads with value := <-ch. Channels are the primary synchronization mechanism in Go — "don't communicate by sharing memory; share memory by communicating." for update := range ch reads until the channel is closed.

3. **context.Context** — A standard library type that carries cancellation signals and deadlines across API boundaries. Functions accept a ctx parameter, and when the caller calls cancel(), every function holding that context can detect it via ctx.Done(). This is Go's idiomatic way to propagate "stop everything" through a call chain.

4. **select** — A control structure that listens on multiple channels simultaneously and executes whichever case fires first. It's the backbone of event-loop-style designs in Go — "wait for a timer tick, a cancellation signal, or a result, whichever comes first."

5. **Interface** — An implicitly satisfied contract. Any struct that implements the methods of an interface satisfies it — no implements keyword needed. This makes it trivial to swap implementations (e.g., real HTTP client vs test fake) without changing the consumer.