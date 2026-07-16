// Written with assistance from Claude Sonnet 4.6
package executor

import "time"

// defining status types
type Status string
const(
	StatusRunning Status   = "running"   // exec in progresss, slices are still being placed

	// terminal states; not followed by anything else
	StatusSuccess Status   = "success"   // all slices were placed and filled, yay
	StatusFailed Status    = "failed"    // every slice failed, :(
	StatusCancelled Status = "cancelled" // caller canceled mid exec
	StatusPartial Status   = "partial"   // some slices failed and some slices filled after retries
)

// defnining progressUpdate type
// fed to caller and printed on the screen (and logged!)
type ProgressUpdate struct{
	TotalQty     float64  // original request
	FilledQty    float64  // different from filled slices. slices can also be filled partially due to the server impl
	AvgPrice     float64  // weighted average: sum(childFilledQty * childAvgPrice) / FilledQty
	SlicesTotal  int      // no. of slices the order was split into
	SlicesPlaced int      // how many child orders have been POSTed to the exchange
	SlicesFilled int      // how many child orders have reached status "filled"
	Status       Status   // running | success | failed | cancelled | partial
	Errors       []string // final per-slice failure descriptions, e.g. "slice 3: failed after 3 retries"
	Logs         []string // prints the retries (delta only), debugging
	Timestamp    time.Time

	// Schedule-fidelity metrics — populated on terminal updates only (0 while running).
	// They measure how faithfully child orders tracked the ideal uniform TWAP schedule.
	MaxIntervalDriftMs   float64 // largest deviation of an actual inter-placement gap from the target interval (ms)
	ScheduleDeviationPct float64 // mean deviation of placements from their ideal scheduled time, as % of the target interval
}

// defning type for the orderRequest tp be placed
type OrderType string
const (
	OrderTypeMarket OrderType = "market"
	// OrderTypeLimit  OrderType = "limit"       can be used to easily extend the implementation to limit order types
)

// adding json tags since the server accepts it like that

type PlaceOrderRequest struct{
	Symbol        string    `json:"symbol"`
	Side          string    `json:"side"`                           // buy or sell
	Qty           float64   `json:"qty"`                            // amount of shares
	Type          OrderType `json:"type"`   
	ClientOrderId string    `json:"client_order_id,omitempty"`      // to use the server's idempotency feature

	// can add the field for limit; to extend limitOrders
}

// type for child order as returned by the exchange (GET /orders/<id>).
type Order struct {
	ID        string  `json:"id"`         // orderId of the child order
	Symbol    string  `json:"symbol"`
	Side      string  `json:"side"`
	Qty       float64 `json:"qty"`
	Type      string  `json:"type"`
	FilledQty float64 `json:"filled_qty"`
	AvgPrice  float64 `json:"avg_price"`  // avg price of the child order 
	Status    string  `json:"status"`     // "open", "partially_filled", "filled", "canceled"
}

// implement for BBO as and when required!

// exchange-side order statuses, as returned by GET /orders/<id>
const (
	orderStatusOpen            = "open"
	orderStatusPartiallyFilled = "partially_filled"
	orderStatusFilled          = "filled"
	orderStatusCanceled        = "canceled"
)