# Order Executor — Coding Handover Brief

## Context

This is a reference implementation for an Order Executor take-home. The full architecture and design rationale lives in `updated_master_doc_final.md` in the project context — **read it first**, it is the single source of truth. This brief covers what to build, in what order, and how to write it.

The mock exchange (`mock_exchange.py`) is provided and must not be modified. It runs on `http://127.0.0.1:9101`.

---

## What to Build

Two things:

1. **Executor package** (`executor/`) — a Go library that accepts a TWAP order and executes it asynchronously against the mock exchange.
2. **Caller script** (`cmd/caller/main.go`) — imports the executor, submits one TWAP order, prints progress to stdout, prints final result, exits.

No frontend. No HTTP server between caller and executor. No persistence.

---

## File Structure

```
order-executor/
├── cmd/
│   └── caller/
│       └── main.go              # Caller script
├── executor/
│   ├── types.go                 # ProgressUpdate, Order, BBO, PlaceOrderRequest, Status constants
│   ├── exchange.go              # Exchange interface (4 methods)
│   ├── http_exchange.go         # HTTPExchange — real HTTP to mock exchange
│   ├── strategy.go              # Strategy interface
│   ├── twap.go                  # TWAPStrategy — coordinator goroutine with two tickers
│   ├── twap_test.go             # All test cases
│   └── fake_exchange_test.go    # FakeExchange for deterministic tests
├── mock_exchange.py             # Provided, do not modify
├── go.mod
└── README.md                    # Will be written after implementation
```

---

## Implementation Order

Build in this order — each layer is testable before moving to the next:

### Step 1: Types (`types.go`)

Define all shared types:

```go
type Status string

const (
    StatusRunning   Status = "running"
    StatusCompleted Status = "completed"
    StatusFailed    Status = "failed"
    StatusCancelled Status = "cancelled"
    StatusPartial   Status = "partial"
)

type ProgressUpdate struct {
    TotalQty     float64
    FilledQty    float64
    AvgPrice     float64
    SlicesTotal  int
    SlicesPlaced int
    SlicesFilled int
    Status       Status
    Errors       []string
    Timestamp    time.Time
}

type PlaceOrderRequest struct {
    Symbol        string  
    Side          string  // "buy" or "sell"
    Qty           float64
    Type          string  // "market" or "limit"
    LimitPrice    float64 // only for limit orders
    ClientOrderID string  // optional, for idempotent retries
}

type Order struct {
    ID        string
    Symbol    string
    Side      string
    Qty       float64
    Type      string
    FilledQty float64
    AvgPrice  float64
    Status    string  // "open", "partially_filled", "filled", "canceled"
}

type BBO struct {
    Symbol string
    Bid    float64
    Ask    float64
}
```

### Step 2: Exchange Interface (`exchange.go`)

```go
type Exchange interface {
    PlaceOrder(ctx context.Context, req PlaceOrderRequest) (*Order, error)
    GetOrder(ctx context.Context, orderID string) (*Order, error)
    CancelOrder(ctx context.Context, orderID string) error
    GetBBO(ctx context.Context, symbol string) (*BBO, error)
}
```

### Step 3: HTTPExchange (`http_exchange.go`)

Implements `Exchange` with real HTTP calls to the mock exchange. Handles:
- JSON marshal/unmarshal
- URL construction
- Context propagation (for cancellation and timeouts)
- Error mapping (non-2xx responses → Go errors)

Uses `http.Client` with a configurable base URL. Each method adds a timeout context on top of the passed-in context.

### Step 4: Strategy Interface (`strategy.go`)

```go
type Strategy interface {
    Execute(ctx context.Context, exchange Exchange) <-chan ProgressUpdate
}
```

### Step 5: TWAPStrategy (`twap.go`)

This is the core. Read Decision 2 and Decision 5 in the master doc carefully.

```go
type TWAPStrategy struct {
    Symbol    string
    Side      string
    TotalQty  float64
    Duration  time.Duration
    Slices    int
    OrderType string // "market" (default) or "limit"
}
```

`Execute()` spawns a single coordinator goroutine and returns `<-chan ProgressUpdate` immediately.

The coordinator runs a `select` loop with:
- `placeTicker` — fires every `Duration / Slices`, POSTs next slice to exchange
- `pollTicker` — fires every ~200ms, GETs status of all active orders
- `ctx.Done()` — shutdown path

Key internal state the coordinator tracks:
- `activeOrders` — map of order ID → slice index for orders still being polled
- `sliceResults` — per-slice filled qty and avg price
- `retryCount` — per-slice retry counter
- `slicesPlaced` — counter
- `nextSlice` — which slice to place next

On each poll tick: iterate active orders, GET each one. If filled → update aggregates, remove from active. If partially_filled → update aggregates, keep active. If failed → retry or drop.

Aggregate weighted avg price: `sum(childFilledQty × childAvgPrice) / totalFilledQty`

Dedup before sending to channel: hash current state, compare to last sent, skip if identical.

Shutdown: stop placing → DELETE active orders (best-effort, short timeout) → compute final state → send cancelled update → close channel.

After all slices placed, stop the placeTicker. After all active orders resolved, send final update, close channel, return.

### Step 6: FakeExchange + Tests (`fake_exchange_test.go`, `twap_test.go`)

FakeExchange is a struct that implements `Exchange` with scripted responses. The test author controls exactly what each `PlaceOrder` and `GetOrder` call returns.

Design FakeExchange so tests can specify:
- Per-call responses for PlaceOrder (success, error, specific order ID)
- Per-call responses for GetOrder (partial fill amounts, prices, statuses)
- Whether CancelOrder succeeds

Six test cases (see master doc for details):
1. Aggregate weighted avg price calculation
2. Partial fills — multiple chunks, intermediate progress
3. Failed child order — retry, fallthrough, shortfall
4. Clean shutdown mid-execution — context cancel, DELETEs fired, final state correct
5. Happy path — all slices fill
6. Slicing math with remainder

### Step 7: Caller (`cmd/caller/main.go`)

Simple script:
1. Create `HTTPExchange` pointing at `http://127.0.0.1:9101`
2. Create `TWAPStrategy` with hardcoded params (e.g., buy 10 BTC-USD, 5 slices, 30 seconds)
3. Set up context with SIGINT/SIGTERM handling → cancel
4. Call `twap.Execute(ctx, exchange)`
5. `for range` over channel, print each update to stdout AND write to a log file
6. After loop exits, print final result, exit

Log file is written by the caller, not the executor. Plain text, one line per update with timestamp.

---

## Coding Style

Adapt to this style (derived from the candidate's existing Go code):

**Naming:**
- camelCase for fields and local variables: `maxCapacity`, `filledQty`, `activeOrders`
- Exported types and methods: `ProgressUpdate`, `Execute`, `PlaceOrder`
- Unexported: `coordinator`, `sendUpdate`, `computeAggregate`
- Receiver names: single letter matching type — `func (t *TWAPStrategy)`, `func (h *HTTPExchange)`

**Struct initialization:**
- Field-by-field assignment style for complex structs:
  ```go
  t := &TWAPStrategy{}
  t.Symbol = symbol
  t.Side = side
  ```
- Composite literals fine for simple/short structs

**Comments:**
- Inline comments explaining reasoning, not just what the code does
- Block comments (`/* */`) for design rationale at the top of complex functions
- Brief doc comments on exported types and methods

**Error handling:**
- Return errors, don't panic
- Wrap errors with context: `fmt.Errorf("placing order for slice %d: %w", i, err)`

**Context pattern:**
- Accept `ctx context.Context` as first parameter
- Check `ctx.Done()` via select before accepting work
- Thread context into all downstream calls

**Testing:**
- Table-driven tests with descriptive test case names
- Subtests with `t.Run(tc.name, ...)`
- Clear arrange/act/assert structure

**General:**
- No external dependencies beyond stdlib
- Keep functions short — extract helpers when a function exceeds ~40 lines
- Semicolons at end of lines are not used (standard Go)

---

## Key Gotchas for the Implementer

1. **The mock exchange fills asynchronously.** POST /orders returns `{id, status: "accepted"}` — NOT filled. You must poll GET /orders/<id> to track fills. This is the whole reason the coordinator needs a pollTicker.

2. **Weighted avg price across children is NOT a simple average.** It's `sum(childFilledQty × childAvgPrice) / totalFilledQty`. Easy to get wrong.

3. **Remainder handling in slicing.** `10 / 3` = 3.33, 3.33, 3.34. Last slice absorbs the remainder. Don't lose quantity to rounding.

4. **`client_order_id` for safe retries.** The mock exchange supports it (lines 173-176 of mock_exchange.py). Use it on every PlaceOrder call so retries don't create duplicates.

5. **Channel must be closed.** The caller does `for range`, which only exits on channel close. If you forget to close the channel, the caller hangs forever.

6. **DELETE doesn't undo fills.** It only stops an open/partially-filled order from filling further. Already-filled quantity stays filled.

7. **HTTP calls are blocking inside the coordinator.** This is a deliberate design choice — see Decision 2b-i in the master doc. Don't spawn goroutines for HTTP calls.

8. **Dedup updates before sending.** Without dedup, the caller gets flooded with identical updates every 200ms when nothing has changed.

---

## What NOT to Build

- No HTTP server between caller and executor
- No frontend / web UI
- No database / persistence (log file is fine)
- No additional order types (just TWAP)
- No latency optimisation
- No real exchange auth

---

## Reference Files in Context

- `updated_master_doc_final.md` — full architecture and design rationale
- `mock_exchange.py` — the exchange server, do not modify
- `README.md` — original task README with rubric
- `task_introduction.md` — task instructions
- `main.go`, `connection_pool.go`, `library.go` — candidate's Go style reference (concurrency LLD prep code)
