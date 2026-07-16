// Written with assistance from Claude Sonnet 4.6
package executor

import "context"


// the fake exchange for tests
// doesnt go to the live server to give us contorl over scenarios

// PlaceResponse is a scripted PlaceOrder response.
type PlaceResponse struct {
	Order *Order
	Err   error
}

// GetResponse is a scripted GetOrder response.
type GetResponse struct {
	Order *Order
	Err   error
}

// FakeExchange implements Exchange with scripted deterministic responses.
// Not goroutine-safe — the coordinator calls it from a single goroutine, and tests read results only after the for-range exits (channel closed, coordinator done).
/*
PlaceResponses []PlaceResponse — a queue of scripted responses for PlaceOrder. Each time the coordinator calls PlaceOrder, it consumes the next item in this list. So if you put 4 errors then 1 success, the first 4 calls fail and the 5th succeeds.
placeIdx int — tracks which PlaceResponse to serve next.
GetResponses map[string][]GetResponse — per order ID, a queue of scripted responses for GetOrder. So "slice-0" can return partially_filled on the first poll, then filled on the second. Different order IDs can have completely different response sequences.
getIdx map[string]int — per order ID, tracks which response to serve next.
CancelledOrders []string — records every order ID that CancelOrder was called with. Tests assert against this to verify shutdown actually fired the DELETEs.
CancelErr error — scripted error to return from CancelOrder. Lets you test the best-effort cancellation path.
*/

/*

How it works end-to-end in the failed slice test:

The coordinator calls PlaceOrder for slice 0 → gets error → retries → gets error → retries → gets error → retries → gets error (4 total, 1 initial + 3 retries) → exhausted, drops slice 0, adds to Errors.

Then placeTicker fires for slice 1 → coordinator calls PlaceOrder → gets the 5th response which is a success → order "slice-1" enters activeOrders.

Next pollTicker tick → coordinator calls GetOrder("slice-1") → gets filled → execution completes with StatusPartial because Errors is non-empty but FilledQty > 0.
*/

type FakeExchange struct {
	// PlaceOrder: responses consumed in order across all calls
	PlaceResponses []PlaceResponse
	placeIdx       int

	// GetOrder: per orderID, responses consumed in order per ID.
	// When responses run out, the last one is repeated — so tests only need to
	// script state transitions, not infinite steady-state responses.
	GetResponses map[string][]GetResponse
	getIdx       map[string]int

	// CancelOrder
	CancelledOrders []string
	CancelErr       error
}

func NewFakeExchange() *FakeExchange {
	f := &FakeExchange{}
	f.GetResponses = make(map[string][]GetResponse)
	f.getIdx = make(map[string]int)
	return f
}

func (f *FakeExchange) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (*Order, error) {
	if f.placeIdx >= len(f.PlaceResponses) {
		// default: open order whose ID matches the client order ID
		o := &Order{}
		o.ID = req.ClientOrderId
		o.Status = orderStatusOpen
		return o, nil
	}
	resp := f.PlaceResponses[f.placeIdx]
	f.placeIdx++
	return resp.Order, resp.Err
}

func (f *FakeExchange) GetOrder(ctx context.Context, orderID string) (*Order, error) {
	responses := f.GetResponses[orderID]
	idx := f.getIdx[orderID]

	if idx >= len(responses) {
		// repeat last response — order stays in its last known state
		if len(responses) > 0 {
			last := responses[len(responses)-1]
			return last.Order, last.Err
		}
		// no responses configured at all — default to filled
		o := &Order{}
		o.ID = orderID
		o.Status = orderStatusFilled
		return o, nil
	}

	resp := responses[idx]
	f.getIdx[orderID] = idx + 1
	return resp.Order, resp.Err
}

func (f *FakeExchange) CancelOrder(ctx context.Context, orderID string) error {
	f.CancelledOrders = append(f.CancelledOrders, orderID)
	return f.CancelErr
}