// Written with assistance from Claude Sonnet 4.6
package executor

import (
	"context"
	"fmt"
	"math"
	"time"
)


// defining the TWAPStrategy
type TWAPStrategy struct {
	Symbol     string
	Side       string        // "buy" or "sell"
	TotalQty   float64       // total quantity to execute
	Duration   time.Duration // spread execution across this window
	Slices     int           // number of child orders
	OrderType  OrderType     // defaults to OrderTypeMarket
	MaxRetries int           // PlaceOrder retry limit per slice (default 3)
	PollInterval time.Duration // defaults to 200ms, overridable in tests

}

// initialise
func NewTWAPStrategy(symbol string, side string, totalQty float64, duration time.Duration, slices int) *TWAPStrategy {
	t := &TWAPStrategy{}
	t.Symbol = symbol
	t.Side = side
	t.TotalQty = totalQty
	t.Duration = duration
	t.Slices = slices
	t.OrderType = OrderTypeMarket // default, caller can override for limit orders
	t.MaxRetries = 3              // default, caller can override
	t.PollInterval = 200 * time.Millisecond // how often the coordinator polls the exchange

	return t
}

// implement the execute() for this strategy. spawns the coordinator goroutine and returns a progress channel immediately.
func (t *TWAPStrategy) Execute(ctx context.Context, exchange Exchange) <-chan ProgressUpdate {
	ch := make(chan ProgressUpdate, 1)
	go t.coordinator(ctx, exchange, ch)
	return ch
}

// divided quantity over the n slices
func computeSliceQtys(totalQty float64, n int) []float64 {
	qtys := make([]float64, n)
	base := totalQty / float64(n)
	for i := 0; i < n-1; i++ {
		qtys[i] = base
	}
	// the last one needs to fill the entire order
	// e.g. 10 / 3 → [3.333..., 3.333..., 3.334...]
	qtys[n-1] = totalQty - base*float64(n-1)
	return qtys
}

// finalise the terminal status once all slices are resolved.
func computeFinalStatus(totalFilledQty float64, errors []string) Status {
	if len(errors) == 0 {
		return StatusSuccess
	}
	if totalFilledQty == 0 {
		return StatusFailed
	}
	return StatusPartial // the last remaning case

}

// computeScheduleMetrics measures how faithfully the child orders tracked the
// ideal uniform TWAP schedule, using only the wall-clock times at which each
// slice was placed.
//
//   - maxIntervalDriftMs: the largest deviation of an actual gap between two
//     consecutive placements from the target inter-slice interval, in ms.
//     Answers "in the worst case, how far off-cadence was a placement?"
//   - scheduleDeviationPct: the mean absolute deviation of each placement from
//     its ideal scheduled time, expressed as a percentage of the target
//     interval. Answers "on average, how tightly did we track the schedule?"
//
// The i-th placement (0-indexed) ideally lands at execStart + (i+1)*interval,
// because a time.Ticker first fires one full interval after it is created.
func computeScheduleMetrics(execStart time.Time, placeTimes []time.Time, targetInterval time.Duration) (maxIntervalDriftMs, scheduleDeviationPct float64) {
	if len(placeTimes) == 0 || targetInterval <= 0 {
		return 0, 0
	}
	target := float64(targetInterval)

	// max interval drift — deviation of consecutive gaps from the target interval
	for i := 1; i < len(placeTimes); i++ {
		gap := float64(placeTimes[i].Sub(placeTimes[i-1]))
		driftMs := math.Abs(gap-target) / float64(time.Millisecond)
		if driftMs > maxIntervalDriftMs {
			maxIntervalDriftMs = driftMs
		}
	}

	// mean cumulative deviation from the ideal schedule, as a fraction of the interval
	sumDevFrac := 0.0
	for i, ts := range placeTimes {
		ideal := execStart.Add(time.Duration(i+1) * targetInterval)
		sumDevFrac += math.Abs(float64(ts.Sub(ideal))) / target
	}
	scheduleDeviationPct = (sumDevFrac / float64(len(placeTimes))) * 100

	return maxIntervalDriftMs, scheduleDeviationPct
}

// central coordinator
/*
coordinator is the single goroutine that owns all TWAP state.

Design: one goroutine -> select loop on two tickers.
  - placeTicker: fires every Duration/Slices, POSTs the next child order
  - pollTicker:  fires every ~200ms(PollInterval), GETs fill status of all active orders

No shared state — the channel IS the synchronization between coordinator and caller.
HTTP calls are synchronous (blocking). 
Deliberate choice: on localhost latency is sub-millisecond. Production fix: perform HTTP calls as goroutines (see README).
*/

func (t *TWAPStrategy) coordinator(ctx context.Context, exchange Exchange, ch chan<- ProgressUpdate) {
	defer close(ch)

	// if(t.OrderType == "limit") {} easily branchable on orderType

	// compute the number of slices
	sliceQtys := computeSliceQtys(t.TotalQty, t.Slices)

	// execStart anchors the ideal schedule; placeTimes records when each slice
	// was actually placed. Together they feed computeScheduleMetrics at the end.
	execStart := time.Now()
	var placeTimes []time.Time

	// dfine the tickers for the select statements to loop over
	// how often to place order
	placeInterval := t.Duration / time.Duration(t.Slices)
	placeTicker := time.NewTicker(placeInterval)
	defer placeTicker.Stop()

	// how often to poll
	pollTicker := time.NewTicker(t.PollInterval)
	defer pollTicker.Stop()

	// need to know whcih orders to poll.
	// every plced order goes here, every order complete (with any status) leaves
	// so it also keep rack of what to cancel

	// sliceOrderId : slicdIdx in the sliceQty
	activeOrders := make(map[string]int)

	// per-slice fill state — needed to compute weighted avg price incrementally
	sliceFilledQty := make([]float64, t.Slices)
	sliceAvgPrice := make([]float64, t.Slices)

	// global stats to return
	totalFilledQty := 0.0
	weightedPriceSum := 0.0 // sum(childFilledQty * childAvgPrice); divide by totalFilledQty for avgPrice
	slicesPlaced := 0
	slicesFilled := 0


	// indexes
	var sliceErrors []string
	nextSliceIdx := 0 // slice index to be fired next


	// storing hash for dedup check
	lastSentHash := ""

	// schedule-fidelity metrics — computed once before the terminal update,
	// captured here so sendUpdate can attach them (0 on running updates)
	maxIntervalDriftMs := 0.0
	scheduleDeviationPct := 0.0

	// definint the fucntion for sending the update based on hashing
	// lambda due to modification of lastSenthash and all the other context it needs
	// pass status and logs, rest are captured from the context
	sendUpdate := func(status Status, logs []string) {
		avgPrice := 0.0
		if totalFilledQty > 0 {
			avgPrice = weightedPriceSum / totalFilledQty
		}

		// hash covers all non-constant, non-timestamp fields
		hash := fmt.Sprintf("%f:%f:%d:%d:%s:%v:%v",
			totalFilledQty, avgPrice, slicesPlaced, slicesFilled, status, sliceErrors, logs)
		if hash == lastSentHash {
			return
		}
		lastSentHash = hash

		ch <- ProgressUpdate{
			TotalQty:     t.TotalQty,
			FilledQty:    totalFilledQty,
			AvgPrice:     avgPrice,
			SlicesTotal:  t.Slices,
			SlicesPlaced: slicesPlaced,
			SlicesFilled: slicesFilled,
			Status:       status,
			Errors:       sliceErrors,
			Logs:         logs,
			Timestamp:    time.Now(),

			MaxIntervalDriftMs:   maxIntervalDriftMs,
			ScheduleDeviationPct: scheduleDeviationPct,
		}
	}

	// now the coordination
	// loops forever
	for { 
		select {
		case <-ctx.Done():
			// placeTicker.Stop()
			// pollTicker.Stop()

			// cancel active orders 
			// fresh context — caller's ctx is already cancelled, DELETE calls need their own deadline
			cancelCtx, cancelFunc := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancelFunc()
			for orderID := range activeOrders {
				exchange.CancelOrder(cancelCtx, orderID) // best-effort, ignore error
			}

			maxIntervalDriftMs, scheduleDeviationPct = computeScheduleMetrics(execStart, placeTimes, placeInterval)
			sendUpdate(StatusCancelled, nil) // status of the entire order, not the slice.
			return

		case <-placeTicker.C:
			// no more slices to place
			if nextSliceIdx >= len(sliceQtys) {
				placeTicker.Stop()
				break
			}

			// we have a slice to place

			sliceIdx := nextSliceIdx
			nextSliceIdx++

			// record when this slice's placement was actually handled — the
			// gap from its ideal slot is what the drift/deviation metrics measure
			placeTimes = append(placeTimes, time.Now())

			// same client_order_id across all retries for this slice —
			// if POST succeeds but response is lost, retry returns the existing order
			// instead of creating a duplicate
			req := PlaceOrderRequest{
				Symbol:        t.Symbol,
				Side:          t.Side,
				Qty:           sliceQtys[sliceIdx],
				Type:          t.OrderType,
				ClientOrderId: fmt.Sprintf("slice-%d", sliceIdx),
			}

			var placed *Order
			var placeErr error
			var logs []string

			for attempt := 0; attempt <= t.MaxRetries; attempt++ { // 1 trye + 3 retries
				placed, placeErr = exchange.PlaceOrder(ctx, req)
				// no error -> success -> breakout
				if placeErr == nil { 
					break
				}
				if attempt < t.MaxRetries {
					logs = append(logs, fmt.Sprintf("slice %d: retry %d/%d — %s", sliceIdx+1, attempt+1, t.MaxRetries, placeErr))
				}
			}

			// all retries exhausted — drop this slice, continue with remaining
			if placeErr != nil {

				sliceErrors = append(sliceErrors, fmt.Sprintf("slice %d: failed after %d retries: %s", sliceIdx+1, t.MaxRetries, placeErr))
				sendUpdate(StatusRunning, logs) // other slices are still running
				break
			}

			// order is placed, no we need to keep polling
			activeOrders[placed.ID] = sliceIdx
			slicesPlaced++
			sendUpdate(StatusRunning, logs)

		case <-pollTicker.C:
			for orderID, sliceIdx := range activeOrders {
				order, err := exchange.GetOrder(ctx, orderID)
				if err != nil {
					// transient failure — skip this tick, retry next poll (~200ms)
					// the order still exists on the exchange; don't count as a failure
					continue
				}

				// update weighted avg price incrementally:
				// subtract old contribution, add new contribution
				weightedPriceSum += (order.FilledQty * order.AvgPrice) - (sliceFilledQty[sliceIdx] * sliceAvgPrice[sliceIdx])
				totalFilledQty += order.FilledQty - sliceFilledQty[sliceIdx]
				sliceFilledQty[sliceIdx] = order.FilledQty
				sliceAvgPrice[sliceIdx] = order.AvgPrice

				switch order.Status {
					case orderStatusFilled:
						slicesFilled++
						delete(activeOrders, orderID)
					case orderStatusCanceled:
						// canceled by exchange (not by us) — treat as a dropped slice
						sliceErrors = append(sliceErrors, fmt.Sprintf("slice %d: order %s canceled by exchange", sliceIdx+1, orderID))
						delete(activeOrders, orderID)
				}
				// open or partially_filled: keep in activeOrders, poll again next tick
			}

			sendUpdate(StatusRunning, nil)

			// all slices placed and none still active → execution complete
			if nextSliceIdx >= len(sliceQtys) && len(activeOrders) == 0 {
				finalStatus := computeFinalStatus(totalFilledQty, sliceErrors)
				maxIntervalDriftMs, scheduleDeviationPct = computeScheduleMetrics(execStart, placeTimes, placeInterval)
				sendUpdate(finalStatus, nil)
				return
			}
		}
	}



}