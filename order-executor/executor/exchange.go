package executor

import "context"

// all HTTP calls go through this
// logic does not see IO (jsons, urls, http related stuff)

// used to connect the logic and trading venue
	// can be the mock server : HTTPexchange
	// can be a fake exchange to be used for testing; this way we have control over what to test

// The logic (the TWAPStrategy coordinator). The coordinator calls exchange.PlaceOrder, exchange.GetOrder, and exchange.CancelOrder inside its goroutine — it never knows whether it's talking to HTTPExchange or FakeExchange.

// The caller only touches HTTPExchange to instantiate it, then passes it into Execute(). After that the caller never calls any exchange methods directly.

/*

so these are used to plce child orders correct? 
and once the request joins the servers queue, we wait for the execution to be completed or do we return immediately

if we keep waiting; then how much to wait before context expires? if we return immediately then context makes sense in order to process timeout

Return immediately. PlaceOrder fires the HTTP POST and returns as soon as the exchange responds with {id, status: "accepted"} — that just means the order is queued, not filled.
The coordinator then separately tracks fills via the pollTicker calling GetOrder every ~200ms until the order reaches "filled" or "canceled".
So the context in PlaceOrder (and GetOrder, CancelOrder) is purely for the HTTP call itself — it cancels if the network hangs and the request takes too long. Nothing to do with waiting for a fill. That's why a short timeout makes sense: you want to detect a hung HTTP call quickly so the coordinator's select loop isn't blocked, not wait for the business logic to complete.
*/

type Exchange interface{
	// need to place an order. exchance replies immediately with accepted (not completed)
		// returns the created order on success
	// need to get status of an order
		// returns state of the order on success
	// and need to delete an order
		// this is best effort; (mentioned in the design doc)

	// need context for the http call only

	PlaceOrder(ctx context.Context, req PlaceOrderRequest) (*Order, error)
	GetOrder(ctx context.Context, orderID string) (*Order, error)
	CancelOrder(ctx context.Context, orderID string) error // best effort, nothing is returned

	// can implement a GetBBO when required
}



