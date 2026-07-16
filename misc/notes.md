# Questions

1. what is bbo? in the get api
    stands for "Best Bid and Offer." It's the tightest prices currently available: the highest price someone is willing to buy at (bid) and the lowest price someone is willing to sell at (ask). The gap between them is the "spread." You'd call this endpoint to see the current market price before placing an order.

2. `

```python
client_order_id (optional): send the same one twice and you get the
          SAME order back instead of a second one. Useful for safe retries.
```
what is this? why is this useful? client might want to place order wth same configs one more time right. or am i misunderstanding
    client_order_id — this is an idempotency key, not about placing the same order twice intentionally. Imagine this scenario: you send a POST to place an order, the exchange receives it and creates it, but the HTTP response gets lost (network blip). You don't know if it went through. Without client_order_id, if you retry, you'd accidentally create a duplicate order. With it, the exchange says "I already have an order with that client_order_id, here's the same one back." It's a safety net for retries — the caller generates a unique ID per intended order (like a UUID), so retrying the same request is safe.

    **1. client_id vs client_order_id** — There is no `client_id` in this system. There's only `client_order_id`. And the crucial distinction: it is **not** derived from the order contents. You generate a unique ID (typically a UUID) per *intended* order. Two intentionally separate orders that happen to have the same symbol/side/qty would get *different* client_order_ids because they're different intentions.

    Think of it this way:
    - You want to buy 2 BTC. You generate `client_order_id = "abc-123"` and POST it.
    - Network timeout. Did it go through? You don't know.
    - You POST again with the *same* `client_order_id = "abc-123"`. Exchange says "already got that one, here's the existing order." No duplicate.
    - Later, you want to buy another 2 BTC (a separate slice). You generate a *new* `client_order_id = "def-456"`. Exchange creates a fresh order.

    The idempotency is tied to the *intent*, not the *content*.

3. do we have to modofy the server in the assignment?
    NO

4. give a brief about what each function in the server is doing
    - now_ms() — current time in milliseconds
    - bbo(symbol) — calculates bid/ask from the midpoint price, applying a 4 basis-point spread
    - _fill_plan(o, bid, ask) — decides whether and at what price an order could fill this tick, and with what probability. Market orders and aggressive limits fill quickly (probability 1.0); passive limits fill slowly (low probability)
    - market_engine() — runs in a background thread ~5x/sec. Randomly walks prices and attempts partial fills on open orders each tick. This is what makes orders fill gradually in chunks rather than instantly
    - Handler — the HTTP request handler. do_GET serves BBO and order status, do_POST creates orders (with idempotency via client_order_id), do_DELETE cancels orders

5. in `a TWAP needs something like {symbol, side, total_qty, duration, slices}. Design the input so a different order type could be added later without reworking everything.` what is a `side`
    - sell vs buy

6. `Shut down cleanly: on stop, in-flight executions should do something sane and defensible — say what you chose and why.` this happens the execution of the order sent by the caller is done? or running a ctrlC during the execution?
    - this is about Ctrl+C while the TWAP is still running. Say you start a TWAP that should take 60 seconds, and at second 30 you hit Ctrl+C. Three slices are placed and partially filled, two haven't been sent yet. What should happen? Cancel the open orders on the exchange? Let fills already in-flight complete? Just report the partial result? That's the design decision they want you to make and justify.x

7. since frontend ability is entirely optional. we will skip it. lets have the progress display in the terminal only somehow. 

8. what is a 4 basis point spread
    Basis points — a basis point (bp) is 0.01%. So 4 basis points = 0.04%. For BTC-USD at $43,000 mid price, the spread is $43,000 × 0.0004 = $17.20 total. The bid sits $8.60 below mid, the ask $8.60 above. It's just a way of expressing a tiny percentage that's common in finance.

9. why would the fill_plan function be used

    **3. Why fill_plan exists** — it's purely the mock exchange's internal simulation. You don't call it or interact with it. It's what makes the mock exchange behave realistically: when `market_engine()` ticks every 200ms, it calls `_fill_plan` on each open order to decide "should this order get a fill this tick, and at what price?" The probability varies — market orders fill almost instantly (p=1.0), limit orders near the market fill sometimes, far-away limits rarely fill. This is why your TWAP needs to *poll* order status over time rather than assuming instant fills.

    You don't need to understand the fill math — just know that after you POST an order, you need to GET its status repeatedly to track when and how much has filled.

-----
questions about the master doc

1. 
```
Is the progress/result interface a sound, well-justified design (including how it reports a failure)?
```

how does our progress/result interface report failure?

    Through two fields on ProgressUpdate:

    Status — set to failed (all slices failed) or partial (some failed, some succeeded)
    Errors — a []string carrying slice-level descriptions like "slice 3: failed after 3 retries"

    The caller sees: status is partial, FilledQty is 7.2, TotalQty is 10, Errors has details on which slices failed. They can compute the shortfall (2.8 unfilled) and see exactly why. This is already in the doc but I'll make it more explicit.

2. 
```
On localhost this is sub-millisecond, so a place tick firing slightly late because a poll is in progress doesn't matter. In production, you'd spawn HTTP calls as goroutines — this is listed under "What I'd Do Next" (see below).


```

i dont understand what you mean by place tick firing late doesnt matter, why would we do it as go routines in prod? arent we already doing it as go routines?

    This is about the current design: single coordinator goroutine, one select loop. When the pollTicker fires, the coordinator calls exchange.GetOrder() for each active order — these are blocking HTTP calls happening sequentially inside that one goroutine. While those calls are happening, the select loop is stuck. If placeTicker fires during that window, it waits.

    On localhost: each HTTP call is sub-millisecond, so even 5 active orders means ~5ms of blocking. The place tick being 5ms late is irrelevant.
    In production with real network latency: say 50ms per call, 10 active orders = 500ms of blocking. Now your place tick could be half a second late, which matters for a TWAP that should place precisely on schedule.

    The fix (for production, not for this assignment): spawn the HTTP calls as separate goroutines that send results back to the coordinator via a channel. That way the select loop stays responsive. But that reintroduces shared state and channels for collecting results — more complexity.

    We are NOT doing goroutines for HTTP calls in our current design. The coordinator is the only goroutine. It makes HTTP calls synchronously inside its select loop. That's the simplicity win. The "what I'd do next" item is about changing this for production.

3. 

in the progressUpdate fields
- why do we need the timestamp?
- we dont require the isFinal right? we already know the loop will keep printing until the final update which is decided via the channel close
- should we also log all the statuses being printed in a log file so that it is easier to review the order's execution later as well?

    Timestamp — you're right to question it. It's not strictly needed for the caller's terminal output. But it's cheap and useful for debugging. I'd keep it but it's not critical. (keep for debuggding)

    IsFinal — you're right, it's redundant. The for range loop exits on channel close, and the terminal statuses (completed/failed/cancelled/partial) already tell you it's done. Let's drop it. The status field is sufficient.

    Logging to file — this edges into persistence, which is a non-goal. Terminal output only for this assignment. But it's a good "what I'd do next" item (structured logging, observability). Want to add it there? (NO, keep the log file for debugging)

4. 
```
The caller creates the adapter and passes it to the strategy. The adapter is not exposed as a public API — it's internal to the executor package.


// Caller
exchange := NewHTTPExchange("http://127.0.0.1:9101")
twap := &TWAPStrategy{Symbol: "BTC-USD", Side: "buy", TotalQty: 10, ...}
progressCh := twap.Execute(ctx, exchange)
```
if t he caller is only doing execute; then where are the 4 functions in the exchange interface 

type Exchange interface {
    PlaceOrder(ctx context.Context, req PlaceOrderRequest) (*Order, error)
    GetOrder(ctx context.Context, orderID string) (*Order, error)
    CancelOrder(ctx context.Context, orderID string) error
    GetBBO(ctx context.Context, symbol string) (*BBO, error)
}

being used? i did not understand this. i see 2 interfaces, strategy (which has the execute function) and the Exchange Interface which has the 4 functions.  between caller and the internal library, who has access to what interface, and calls what via the interface. where is the interface implemented

    Caller (main.go)
  │
  │ creates HTTPExchange
  │ creates TWAPStrategy
  │ calls twap.Execute(ctx, exchange) ← Strategy interface
  │ reads <-chan ProgressUpdate
  │
  └──→ TWAPStrategy.Execute()
         │
         │ This is where the Exchange interface is used:
         │   exchange.PlaceOrder(...)   ← inside coordinator, place tick
         │   exchange.GetOrder(...)     ← inside coordinator, poll tick
         │   exchange.CancelOrder(...)  ← inside coordinator, shutdown
         │   exchange.GetBBO(...)       ← not used for market TWAP, available for limit
         │
         └──→ HTTPExchange (implements Exchange)
                │
                │ Translates to HTTP:
                │   POST /orders
                │   GET /orders/<id>
                │   DELETE /orders/<id>
                │   GET /bbo?symbol=...
                │
                └──→ Mock Exchange (Python server)


    The caller only sees the Strategy interface. It never calls PlaceOrder, GetOrder, etc directly. It just passes the exchange adapter into the strategy.

    The strategy (TWAPStrategy) is the one calling the 4 Exchange methods inside its coordinator goroutine. That's where the interface is consumed.

    The caller creates both objects, wires them together, and reads the output channel. Clean separation.

5. the PlaceOrderRequest for idempotent POSTS is supported by the current mock server? are you sure?
    YES

6. 
```
Best-Effort Cancellation
DELETE calls use a short timeout. If the exchange is unresponsive, shutdown is not blocked — the DELETEs are best-effort. The final ProgressUpdate reports what was known at the time of cancellation.


```
this i am not sure i agree with. if the server is hanging should we keep retrying until server is back for correctness? or should we pass on the responsibility of confirming that the delete has suceeded bcak to the caller. the current one passes it bcak to the caller. which i think is fine. but mention this explicitly in the master doc

    Fair point. Let me clarify what happens: the coordinator DELETEs active orders with a short timeout. If the exchange doesn't respond, the coordinator proceeds with shutdown anyway and reports the final state based on what it knew. The caller sees status cancelled and the fill numbers as of that moment. Whether those orphaned orders continue filling on an unresponsive exchange is now outside the program's control.

    You're right that this should be explicit. The caller receives the final state and it's their responsibility to verify with the exchange later if needed. I'll update the doc.

7. 
```
 An OrderType field on the TWAPStrategy struct. The coordinator branches inside Execute. Not a new strategy — a parameter on the existing one.
```
but we have not discussed what the struct will be? 

    type TWAPStrategy struct {
        Symbol    string
        Side      string        // "buy" or "sell"
        TotalQty  float64
        Duration  time.Duration
        Slices    int
        OrderType string        // "market" (default) or "limit" — future extension
    }

8. 
```
Async HTTP calls — Spawn POST/GET calls to the exchange as goroutines so blocking I/O doesn't delay tickers in the coordinator's select loop. Not needed for the localhost mock (sub-millisecond latency) but critical at real network latencies.
```
isnt this what we are alrady doing? calling the srver via http calls?

    - same as 2.

----
questions in updated master doc

1. do we need retries if the internal impl is synchronous?
    Connection model: Go's http.Client uses connection pooling by default (HTTP keep-alive). So the TCP connection is reused across requests, but each POST /orders or GET /orders/<id> is still an independent request-response. A persistent connection doesn't prevent a request from failing — the server could return a 500, the request could time out, or the connection could drop between requests.

    Does retry improve our impl? For the mock exchange on localhost — practically no. The mock doesn't simulate server errors. It returns 400 for bad input (which you shouldn't retry — that's a client bug) and always responds to valid requests. Retries would never fire in a real run.

    But there are three reasons to keep retries:
        First, the rubric explicitly says "sensible handling of exchange errors" under the Should tier. Retries are the clearest demonstration of that.

        Second, test case #3 (failed child order) uses FakeExchange to simulate failures. Without retry logic in the coordinator, there's nothing to test. The FakeExchange returns an error, the coordinator retries, FakeExchange fails again, the coordinator drops the slice and reports the shortfall. That test scenario is one of the "tricky edge cases" the rubric calls out.

        Third, the client_order_id and retries are specifically linked. The case they protect against — POST succeeds on the exchange but the response is lost (timeout) — is a real failure mode even on localhost if we have a timeout context on HTTP calls. Without retry, you'd think the slice failed when it actually placed successfully, and you'd have no way to recover.

So retries don't improve runtime behavior against the mock, but they improve the rubric score, enable a key test case, and demonstrate sound design thinking. Worth keeping.


---- 
notes after starting coding

- add in to the to-do next (already added in the comments)
1. limit orders
    - ordertype can be easily extended
    - add another field for limit in the plceOrderRequest
    - add a type for BBO

- read the comments and clean up; contains information for myself as well


- using json tags for whatever is sent to the server
    - make sure to string()the inout before sending to server

- dropping httpcall timeouts since local in httpexchange
    - make sure to add in to-do for production

- okay, since there are a lot of context to pass, we will just use the lambda. for the hash dedup

- why cant we compare floats with ==
    Computers store floats in binary (base 2). Some decimal numbers can't be represented exactly in binary — just like 1/3 can't be represented exactly in decimal (0.3333... forever).

    For example, 0.1 in binary is a repeating fraction. The computer stores the closest approximation, which is something like 0.1000000000000000055511151231257827021181583404541015625.

    result := 0.1 + 0.2
    fmt.Println(result == 0.3)  // false

    Because 0.1 + 0.2 evaluates to 0.30000000000000004, not 0.3.

    In our case, we're dividing quantities and multiplying prices. 10.0 / 3.0 gives 3.3333333333333335, not exactly 3.333.... After several multiplications and divisions across slices, these tiny errors accumulate. So instead of checking final.AvgPrice == 150.0, we check absFloat(final.AvgPrice - 150.0) < 1e-9 — "is it within a billionth of the expected value?" Close enough to be correct, tolerant of binary representation errors.

How testing works
    The mechanics:Any file ending in _test.go is a test file. Go's go test command finds these automatically. Any function that starts with Test and takes *testing.T as its only argument is a test case. You run them with go test ./executor/ from the project root.

```Go
func TestSomething(t *testing.T) {
    // if something is wrong:
    t.Errorf("expected %d, got %d", want, got)   // logs failure, keeps running
    t.Fatalf("expected %d, got %d", want, got)   // logs failure, stops this test immediately
}
```

Table-driven tests are a pattern (not a language feature) where you define a slice of test cases and loop over them:

```Go
cases := []struct {
    name     string
    input    int
    expected int
}{
    {"small", 2, 4},
    {"zero", 0, 0},
}

for _, tc := range cases {
    t.Run(tc.name, func(t *testing.T) {
        // t.Run creates a named subtest — shows up as TestDouble/small, TestDouble/zero
        got := double(tc.input) // do the actual thing you want to test
        if got != tc.expected {
            t.Errorf("want %d, got %d", tc.expected, got)
        }
    })
}
```

--- entire cancel flow
**1. User presses Ctrl+C**

OS sends SIGINT to the process.

**2. Signal goroutine (`main.go`)**

`signal.Notify` routed SIGINT into `sigCh`. The goroutine unblocks on `<-sigCh`, calls `write("[caller] interrupt received")`, then calls `cancel()`. The `ctx` is now cancelled.

**3. Coordinator's select loop (`twap.go`)**

On the next iteration of the `for` loop, `ctx.Done()` fires — its channel is now closed so it's always selectable. The `ctx.Done()` case wins.

**4. Cleanup inside `ctx.Done()` case**

`placeTicker` and `pollTicker` defers are registered but haven't fired yet — they fire on `return`.

A fresh context is created:
```go
cancelCtx, cancelFunc := context.WithTimeout(context.Background(), 3*time.Second)
defer cancelFunc()
```

This is independent of the cancelled `ctx`. The coordinator loops over `activeOrders` and calls `exchange.CancelOrder(cancelCtx, orderID)` for each — best-effort, errors ignored. This fires real HTTP DELETE calls to the exchange, preventing orphaned orders from filling unobserved.

**5. Final update**

`sendUpdate(StatusCancelled, nil)` pushes one last `ProgressUpdate` onto `ch` with whatever `FilledQty` and `AvgPrice` were known at the time of cancellation.

**6. Coordinator returns**

`defer close(ch)` fires — the progress channel is closed.

**7. Back in `main.go`**

The caller's `for update := range ch` loop exits because the channel is closed. `final` holds the last update — `StatusCancelled` with accurate fill numbers.

The caller prints the final result and any slice errors.

**8. `main()` returns**

`defer cancel()` fires — no-op since `ctx` is already cancelled.

`defer logFile.Close()` fires — log file is flushed and closed cleanly.


----- NOTES from review

1. summary of the cancel flow from the user;s side

Summary of the flow

Ctrl+C / SIGTERM ─→ sigCh ─→ cancel()
                                 │
                                 ▼
                            ctx.Done() closes
                                 │
              ┌──────────────────┴───────────────────┐
              ▼                                       ▼
   coordinator's select fires            any in-flight PlaceOrder/GetOrder
   ctx.Done() case → shutdown            HTTP call aborts

So: ctx = the "are we still running?" signal read by the coordinator and every HTTP call; cancel = the trigger, pulled by the signal handler (real shutdown) and by defer (cleanup on normal exit).

2. `executor` is the package we have coded!
    - `Exchange` and `Strategy` are the interface that is exposed
    - implementations are the http and the fake exchange, and the twapStrategy

3. caller creates the endpoint to used for the httpexchange. then creates a twapstrategy and calls execute on the strategy. 

4. `coordinator(ctx context.Context, exchange Exchange, ch chan<- ProgressUpdate) ` is a write only channel. guarded by the compiler. 
    - <-chan ProgressUpdate   // receive-only (can only read)
    - chan<- ProgressUpdate   // send-only (can only write)
    - chan ProgressUpdate     // bidirectional

5. pollTicker flow
- get orderstatus for the slice (with the orderId for the slice)
    - returns the aggregatde price for the slice, we dont need to do the math for per slice
    -  order.FilledQty — cumulative filled so far for this slice (e.g. 1.0, then 2.5, then 3.33)
    - order.AvgPrice — the slice's own weighted-average price across all its chunks so far

What GetOrder actually returns for a slice

Look at the server (mock_exchange.py:114): every time a slice fills another chunk, the server itself updates that order's avg_price as a running weighted average of its own chunks. So when you GetOrder, you get the slice already aggregated:

- order.FilledQty — cumulative filled so far for this slice (e.g. 1.0, then 2.5, then 3.33)
- order.AvgPrice — the slice's own weighted-average price across all its chunks so far

So for your 3.33-unit slice filling 1 → 2.5 → 3.33, three polls return:

┌──────┬───────────┬───────────────────────────────────────────────────────────────┐
│ poll │ FilledQty │ AvgPrice (server-computed, weighted over that slice's chunks) │
├──────┼───────────┼───────────────────────────────────────────────────────────────┤
│ 1    │ 1.0       │ avg of the 1-unit chunk                                       │
├──────┼───────────┼───────────────────────────────────────────────────────────────┤
│ 2    │ 2.5       │ avg of (1 @ p1, 1.5 @ p2)                                     │
├──────┼───────────┼───────────────────────────────────────────────────────────────┤
│ 3    │ 3.33      │ avg of (1 @ p1, 1.5 @ p2, 0.83 @ p3)                          │
└──────┴───────────┴───────────────────────────────────────────────────────────────┘

You do not see p1/p2/p3 separately. The intra-slice averaging is the server's job.

So what is this code doing?

The coordinator's job is one level up: aggregate across all slices (multiple child orders) into a single global weighted average. It maintains two running totals:

- totalFilledQty — filled across all slices
- weightedPriceSum — Σ (slice's filledQty × slice's avgPrice) across all slices

and global AvgPrice = weightedPriceSum / totalFilledQty.

Why the subtract-then-add ("incremental") trick

Here's the problem: the same slice is polled over and over, and its numbers grow each time. If you just did weightedPriceSum += order.FilledQty * order.AvgPrice every poll, you'd count that slice 3 times and massively overcount.

So the code keeps the slice's last-seen contribution in sliceFilledQty[sliceIdx] / sliceAvgPrice[sliceIdx], and on each poll it replaces the old contribution with the new one:

weightedPriceSum += (new contribution) - (old contribution)
                 =  (order.FilledQty*order.AvgPrice) - (sliceFilledQty[i]*sliceAvgPrice[i])
totalFilledQty  += order.FilledQty - sliceFilledQty[i]      // same delta idea
sliceFilledQty[i] = order.FilledQty                         // remember new value
sliceAvgPrice[i]  = order.AvgPrice

Numeric walk-through (your slice, index i)

Say prices give slice avgPrice ≈ 100 throughout (kept flat for clarity). Start: sliceFilledQty[i]=0, sliceAvgPrice[i]=0.

- Poll 1 (FilledQty 1.0, avg 100): add 1*100 - 0 = 100; totalFilledQty += 1-0. Store (1.0, 100).
- Poll 2 (FilledQty 2.5, avg 100): add 2.5*100 - 1*100 = 150; total += 2.5-1 = 1.5. Store (2.5, 100).
- Poll 3 (FilledQty 3.33, avg 100): add 3.33*100 - 2.5*100 = 83; total += 0.83. Store (3.33, 100).

Notice the deltas — 100, 150, 83 on the price sum and 1, 1.5, 0.83 on qty — are exactly the new chunk each poll, which is what you intuited. The subtraction is just how the code isolates "what's new since last poll" from the server's cumulative numbers. Net: weightedPriceSum gains 333, totalFilledQty gains 3.33 — correct.

The err != nil { continue } at the top

If GetOrder fails this tick (transient network blip), it skips that order for this poll and tries again in ~200ms. Crucially it does not touch the aggregates and does not mark the slice failed — the order still lives on the exchange; you just couldn't read it this instant. This avoids corrupting the running totals on a read hiccup.

Summary

- Server aggregates chunks → one (FilledQty, AvgPrice) per slice.
- Coordinator aggregates slices → one global (totalFilledQty, AvgPrice).
- The subtract-old/add-new pattern exists purely because each slice is polled repeatedly with growing cumulative values, so you must replace its prior contribution rather than re-add it.

6. Fake exchange flow

PlaceOrder — single flat queue, one index

PlaceResponses []PlaceResponse   // {Order, Err}
placeIdx       int
Exactly as you said: one shared list, consumed in order across all calls, placeIdx++ each time.
- Call N returns PlaceResponses[N].
- When the list runs out, it doesn't error — it falls back to a default "open order whose ID = the request's ClientOrderId" (fake_exchange_test.go:68-74). This is why the failed-order test scripts exactly 4 errors then 1 success, and doesn't need to script the rest.

Why flat (not per-slice)? Because slices are placed sequentially, so a single ordered queue naturally lines up call 0→slice 0, call 1→slice 1, etc. — or, in the retry test, calls 0-3→slice 0's retries, call 4→slice 1.

GetOrder — per-order-ID queues, per-ID index, repeats last

GetResponses map[string][]GetResponse   // orderID → its own response sequence
getIdx       map[string]int             // orderID → its own index
Not one shared list — one queue per order ID, because different orders fill differently and are polled independently. On GetOrder(id):
- returns GetResponses[id][getIdx[id]], then getIdx[id]++.

The important twist (fake_exchange_test.go:84-95): when a slice's queue is exhausted, it repeats the last response instead of falling off the end.

Why? Polling is timing-dependent — the coordinator might poll a slice 2 times or 20 times depending on tick alignment, which a test can't predict. So you only script the state transitions you care about (e.g. partially_filled → filled), and the fake holds the final state forever:
f.GetResponses["slice-0"] = []GetResponse{
    {partially_filled, 3},   // first poll
    {filled, 10},            // second poll — and every poll after, repeated
}
Extra edge: if a queue is empty/unset, it defaults to filled — so tests that don't care about a slice's fill path get instant completion.

CancelOrder — records, doesn't consume

CancelledOrders []string
CancelErr       error
No queue, no index. It just appends the order ID to CancelledOrders and returns CancelErr (a single scripted error, or nil).
- The list is an assertion target, not a response source: the shutdown test checks f.CancelledOrders[0] == "slice-0" to prove the coordinator actually fired the DELETE.
- CancelErr lets a test exercise the best-effort path (DELETE fails but shutdown still proceeds).

Why these three shapes differ

┌─────────────┬────────────────────────────┬────────────────────────────────────────────────────────┐
│   Method    │         Structure          │                          Why                           │
├─────────────┼────────────────────────────┼────────────────────────────────────────────────────────┤
│ PlaceOrder  │ one flat queue             │ placements happen in a single known sequence           │
├─────────────┼────────────────────────────┼────────────────────────────────────────────────────────┤
│ GetOrder    │ queue per ID + repeat-last │ orders fill independently; poll count is unpredictable │
├─────────────┼────────────────────────────┼────────────────────────────────────────────────────────┤
│ CancelOrder │ record list + one error    │ you assert that it happened, not script a reply        │
└─────────────┴────────────────────────────┴────────────────────────────────────────────────────────┘

So the general "store responses, return one by one, increment index" model is exactly right for Place and Get — with Get keyed per order and sticky on its last state — while Cancel is a recorder, not a replayer.

