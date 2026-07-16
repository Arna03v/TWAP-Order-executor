// Written with assistance from Claude Sonnet 4.6
package executor

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// drainUpdates collects all ProgressUpdates until the channel closes.
func drainUpdates(ch <-chan ProgressUpdate) []ProgressUpdate {
	var updates []ProgressUpdate
	for u := range ch {
		updates = append(updates, u)
	}
	return updates
}

// absFloat(x) — absolute value for float comparison. Floats can't be compared with == due to precision, so we check absFloat(a - b) < 1e-9.
func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// =========== BUY ORDER TESTS =============

// Test 1: slicing math — sum equals totalQty, last slice absorbs remainder
// just the math
func TestComputeSliceQtys(t *testing.T) {
	cases := []struct {
		name     string
		totalQty float64
		n        int
	}{
		{"even split", 10, 2},
		{"remainder on last slice", 10, 3},
		{"single slice", 7.5, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qtys := computeSliceQtys(tc.totalQty, tc.n)

			if len(qtys) != tc.n {
				t.Fatalf("want %d slices, got %d", tc.n, len(qtys))
			}

			// sum must equal the total — no quantity lost or invented
			var sum float64
			for _, q := range qtys {
				sum += q
			}
			if absFloat(sum-tc.totalQty) > 1e-9 {
				t.Errorf("want sum=%.10f, got %.10f", tc.totalQty, sum)
			}

			// distribution must be correct, not just the sum. Assert the actual
			// shape: every slice is positive, the first n-1 are an even split
			// (totalQty/n), and the last absorbs the remainder. This rejects a
			// bug like [0, 0, 10] that would still pass the sum check above.
			base := tc.totalQty / float64(tc.n)
			for i, q := range qtys {
				if q <= 0 {
					t.Errorf("slice %d: want positive qty, got %.10f", i, q)
				}
			}
			for i := 0; i < tc.n-1; i++ {
				if absFloat(qtys[i]-base) > 1e-9 {
					t.Errorf("slice %d: want even split %.10f, got %.10f", i, base, qtys[i])
				}
			}
			wantLast := tc.totalQty - base*float64(tc.n-1)
			if absFloat(qtys[tc.n-1]-wantLast) > 1e-9 {
				t.Errorf("last slice: want remainder %.10f, got %.10f", wantLast, qtys[tc.n-1])
			}
		})
	}
}

// Test: schedule-fidelity metrics — drift and deviation from a synthetic
// placement timeline, so the math is checked without timing flakiness.
func TestComputeScheduleMetrics(t *testing.T) {
	interval := 100 * time.Millisecond
	start := time.Unix(0, 0)

	// perfect schedule: slice i placed exactly at start + (i+1)*interval
	perfect := []time.Time{
		start.Add(1 * interval),
		start.Add(2 * interval),
		start.Add(3 * interval),
	}
	drift, dev := computeScheduleMetrics(start, perfect, interval)
	if drift > 1e-9 {
		t.Errorf("perfect schedule: want 0 drift, got %.6fms", drift)
	}
	if dev > 1e-9 {
		t.Errorf("perfect schedule: want 0%% deviation, got %.6f%%", dev)
	}

	// one late placement: slice 1 lands 20ms late (its gap and the next gap
	// are both off by 20ms, so max interval drift = 20ms)
	late := []time.Time{
		start.Add(1 * interval),
		start.Add(2*interval + 20*time.Millisecond),
		start.Add(3 * interval),
	}
	drift, dev = computeScheduleMetrics(start, late, interval)
	if absFloat(drift-20) > 1e-6 {
		t.Errorf("late placement: want 20ms max interval drift, got %.6fms", drift)
	}
	// only slice 1 deviates, by 20ms = 20%% of the 100ms interval; mean over 3 = 6.666..%%
	wantDev := (20.0 / 100.0) / 3.0 * 100
	if absFloat(dev-wantDev) > 1e-6 {
		t.Errorf("late placement: want %.6f%% deviation, got %.6f%%", wantDev, dev)
	}

	// degenerate inputs must not panic or divide by zero
	if d, p := computeScheduleMetrics(start, nil, interval); d != 0 || p != 0 {
		t.Errorf("empty placeTimes: want 0/0, got %.6f/%.6f", d, p)
	}
	if d, p := computeScheduleMetrics(start, perfect, 0); d != 0 || p != 0 {
		t.Errorf("zero interval: want 0/0, got %.6f/%.6f", d, p)
	}
}

// Test 2: weighted avg price — sum(childFilledQty * childAvgPrice) / totalFilledQty
func TestWeightedAvgPrice(t *testing.T) {
	f := NewFakeExchange()
	f.PlaceResponses = []PlaceResponse{
		{Order: &Order{ID: "slice-0", Status: orderStatusOpen}},
		{Order: &Order{ID: "slice-1", Status: orderStatusOpen}},
	}
	f.GetResponses["slice-0"] = []GetResponse{
		{Order: &Order{ID: "slice-0", FilledQty: 3, AvgPrice: 100, Status: orderStatusFilled}},
	}
	f.GetResponses["slice-1"] = []GetResponse{
		{Order: &Order{ID: "slice-1", FilledQty: 7, AvgPrice: 200, Status: orderStatusFilled}},
	}

	twap := NewTWAPStrategy("BTC-USD", "buy", 10, 50*time.Millisecond, 2)
	twap.PollInterval = 10 * time.Millisecond

	updates := drainUpdates(twap.Execute(context.Background(), f))
	final := updates[len(updates)-1]

	if final.Status != StatusSuccess {
		t.Errorf("want StatusSuccess, got %s", final.Status)
	}

	// weighted: (3*100 + 7*200) / 10 = 1700/10 = 170
	// simple average would give (100+200)/2 = 150 — WRONG
	wantAvg := (3*100.0 + 7*200.0) / 10.0
	if absFloat(final.AvgPrice-wantAvg) > 1e-6 {
		t.Errorf("want AvgPrice=%.6f, got %.6f", wantAvg, final.AvgPrice)
	}
}

// Test 3: partial fills — order fills across multiple poll ticks,
// intermediate ProgressUpdates reflect the in-between state
/*
TestPartialFills — One slice, but the FakeExchange returns two different responses for GetOrder. First poll: filled_qty: 3, status: partially_filled. Second poll: filled_qty: 10, status: filled. The test verifies two things: (a) an intermediate ProgressUpdate was sent with FilledQty=3 and Status=running, and (b) the final update has FilledQty=10 and Status=success. This proves the coordinator correctly handles orders that fill in chunks over multiple poll ticks.
*/
func TestPartialFills(t *testing.T) {
	f := NewFakeExchange()
	f.PlaceResponses = []PlaceResponse{
		{Order: &Order{ID: "slice-0", Status: orderStatusOpen}},
	}
	// first poll: partial; second poll: fully filled
	f.GetResponses["slice-0"] = []GetResponse{
		{Order: &Order{ID: "slice-0", FilledQty: 3, AvgPrice: 100, Status: orderStatusPartiallyFilled}},
		{Order: &Order{ID: "slice-0", FilledQty: 10, AvgPrice: 105, Status: orderStatusFilled}},
	}

	twap := NewTWAPStrategy("BTC-USD", "buy", 10, 20*time.Millisecond, 1)
	twap.PollInterval = 10 * time.Millisecond

	updates := drainUpdates(twap.Execute(context.Background(), f))

	// verify an intermediate update captured the partial fill
	var sawPartial bool
	for _, u := range updates {
		if absFloat(u.FilledQty-3) < 1e-9 && u.Status == StatusRunning {
			sawPartial = true
		}
	}
	if !sawPartial {
		t.Error("want intermediate update with FilledQty=3, did not see one")
	}

	final := updates[len(updates)-1]
	if final.Status != StatusSuccess {
		t.Errorf("want StatusSuccess, got %s", final.Status)
	}
	if absFloat(final.FilledQty-10) > 1e-9 {
		t.Errorf("want FilledQty=10, got %f", final.FilledQty)
	}
}

// Test 4: failed child order — retries fire, slice dropped, shortfall in final result
/*
Test 4 — TestFailedChildOrder — FakeExchange returns errors for the first 4 PlaceOrder calls (1 initial + 3 retries for slice 0). Slice 1 places and fills successfully. Verifies: final status is partial (not failed, since one slice succeeded), the Errors slice has at least one entry describing what failed, and FilledQty is 5 (only slice 1's quantity). This tests the retry logic and shortfall reporting.
*/
func TestFailedChildOrder(t *testing.T) {
	f := NewFakeExchange()
	// slice 0: 1 initial attempt + 3 retries = 4 error responses
	f.PlaceResponses = []PlaceResponse{
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		// slice 1 succeeds
		{Order: &Order{ID: "slice-1", Status: orderStatusOpen}},
	}
	f.GetResponses["slice-1"] = []GetResponse{
		{Order: &Order{ID: "slice-1", FilledQty: 5, AvgPrice: 100, Status: orderStatusFilled}},
	}

	twap := NewTWAPStrategy("BTC-USD", "buy", 10, 50*time.Millisecond, 2)
	twap.PollInterval = 10 * time.Millisecond

	updates := drainUpdates(twap.Execute(context.Background(), f))
	final := updates[len(updates)-1]

	if final.Status != StatusPartial {
		t.Errorf("want StatusPartial, got %s", final.Status)
	}
	if len(final.Errors) == 0 {
		t.Error("want at least one error describing the failed slice")
	}
	if absFloat(final.FilledQty-5) > 1e-9 {
		t.Errorf("want FilledQty=5, got %f", final.FilledQty)
	}
}

// Test: total failure — every slice fails to place, final status is failed (not partial)
/*
TestAllSlicesFail — Both slices fail all their PlaceOrder attempts (1 initial + 3 retries
= 4 per slice, 8 errors total). No slice ever places, so nothing fills. Verifies the
distinct total-failure path: status is failed (not partial, since nothing succeeded),
FilledQty is 0, and Errors describes every dropped slice. This covers the
computeFinalStatus branch where totalFilledQty == 0.
*/
func TestAllSlicesFail(t *testing.T) {
	f := NewFakeExchange()
	// 2 slices × (1 initial + 3 retries) = 8 error responses — every attempt fails
	f.PlaceResponses = []PlaceResponse{
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
	}

	twap := NewTWAPStrategy("BTC-USD", "buy", 10, 50*time.Millisecond, 2)
	twap.PollInterval = 10 * time.Millisecond

	updates := drainUpdates(twap.Execute(context.Background(), f))
	final := updates[len(updates)-1]

	if final.Status != StatusFailed {
		t.Errorf("want StatusFailed, got %s", final.Status)
	}
	if absFloat(final.FilledQty-0) > 1e-9 {
		t.Errorf("want FilledQty=0, got %f", final.FilledQty)
	}
	if len(final.Errors) != 2 {
		t.Errorf("want 2 slice errors (one per failed slice), got %d: %v", len(final.Errors), final.Errors)
	}
}

// Test 5: clean shutdown — context cancelled, active orders DELETEd, final status cancelled
/*
Test 5 — TestCleanShutdown — One slice is placed. FakeExchange keeps returning status: open on every poll (the order never fills). After 50ms, the test cancels the context. Verifies: final status is cancelled, CancelOrder was called on the exchange for the active order, and the coordinator exited cleanly. This tests the entire shutdown sequence — context cancellation, DELETE active orders, final report.

*/
func TestCleanShutdown(t *testing.T) {
	f := NewFakeExchange()
	f.PlaceResponses = []PlaceResponse{
		{Order: &Order{ID: "slice-0", Status: orderStatusOpen}},
	}
	// order never fills — stays open indefinitely
	f.GetResponses["slice-0"] = []GetResponse{
		{Order: &Order{ID: "slice-0", FilledQty: 0, Status: orderStatusOpen}},
	}

	twap := NewTWAPStrategy("BTC-USD", "buy", 10, 10*time.Millisecond, 1)
	twap.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	ch := twap.Execute(ctx, f)

	// give the order time to be placed and polled before cancelling
	time.Sleep(50 * time.Millisecond)
	cancel()

	updates := drainUpdates(ch)
	final := updates[len(updates)-1]

	if final.Status != StatusCancelled {
		t.Errorf("want StatusCancelled, got %s", final.Status)
	}
	if len(f.CancelledOrders) == 0 {
		t.Error("want CancelOrder called for active order, got none")
	}
	if f.CancelledOrders[0] != "slice-0" {
		t.Errorf("want CancelOrder called for slice-0, got %s", f.CancelledOrders[0])
	}
}

// Test 6: happy path — all slices fill, final status success, FilledQty == TotalQty
func TestHappyPath(t *testing.T) {
	f := NewFakeExchange()
	f.PlaceResponses = []PlaceResponse{
		{Order: &Order{ID: "slice-0", Status: orderStatusOpen}},
		{Order: &Order{ID: "slice-1", Status: orderStatusOpen}},
		{Order: &Order{ID: "slice-2", Status: orderStatusOpen}},
	}
	f.GetResponses["slice-0"] = []GetResponse{
		{Order: &Order{ID: "slice-0", FilledQty: 4, AvgPrice: 100, Status: orderStatusFilled}},
	}
	f.GetResponses["slice-1"] = []GetResponse{
		{Order: &Order{ID: "slice-1", FilledQty: 4, AvgPrice: 100, Status: orderStatusFilled}},
	}
	f.GetResponses["slice-2"] = []GetResponse{
		{Order: &Order{ID: "slice-2", FilledQty: 4, AvgPrice: 100, Status: orderStatusFilled}},
	}

	twap := NewTWAPStrategy("BTC-USD", "buy", 12, 60*time.Millisecond, 3)
	twap.PollInterval = 10 * time.Millisecond

	updates := drainUpdates(twap.Execute(context.Background(), f))
	final := updates[len(updates)-1]

	if final.Status != StatusSuccess {
		t.Errorf("want StatusSuccess, got %s", final.Status)
	}
	if absFloat(final.FilledQty-12) > 1e-9 {
		t.Errorf("want FilledQty=12, got %f", final.FilledQty)
	}
	if final.SlicesFilled != 3 {
		t.Errorf("want SlicesFilled=3, got %d", final.SlicesFilled)
	}
}

// =========== SELL ORDER TESTS =============
// Sell order tests — same scenarios as buy, verifying side="sell" flows through correctly

func TestWeightedAvgPriceSell(t *testing.T) {
	f := NewFakeExchange()
	f.PlaceResponses = []PlaceResponse{
		{Order: &Order{ID: "slice-0", Status: orderStatusOpen}},
		{Order: &Order{ID: "slice-1", Status: orderStatusOpen}},
	}
	f.GetResponses["slice-0"] = []GetResponse{
		{Order: &Order{ID: "slice-0", FilledQty: 3, AvgPrice: 100, Status: orderStatusFilled}},
	}
	f.GetResponses["slice-1"] = []GetResponse{
		{Order: &Order{ID: "slice-1", FilledQty: 7, AvgPrice: 200, Status: orderStatusFilled}},
	}

	twap := NewTWAPStrategy("BTC-USD", "sell", 10, 50*time.Millisecond, 2)
	twap.PollInterval = 10 * time.Millisecond

	updates := drainUpdates(twap.Execute(context.Background(), f))
	final := updates[len(updates)-1]

	if final.Status != StatusSuccess {
		t.Errorf("want StatusSuccess, got %s", final.Status)
	}
	wantAvg := (3*100.0 + 7*200.0) / 10.0
	if absFloat(final.AvgPrice-wantAvg) > 1e-6 {
		t.Errorf("want AvgPrice=%.6f, got %.6f", wantAvg, final.AvgPrice)
	}
}

func TestPartialFillsSell(t *testing.T) {
	f := NewFakeExchange()
	f.PlaceResponses = []PlaceResponse{
		{Order: &Order{ID: "slice-0", Status: orderStatusOpen}},
	}
	f.GetResponses["slice-0"] = []GetResponse{
		{Order: &Order{ID: "slice-0", FilledQty: 3, AvgPrice: 100, Status: orderStatusPartiallyFilled}},
		{Order: &Order{ID: "slice-0", FilledQty: 10, AvgPrice: 105, Status: orderStatusFilled}},
	}

	twap := NewTWAPStrategy("BTC-USD", "sell", 10, 20*time.Millisecond, 1)
	twap.PollInterval = 10 * time.Millisecond

	updates := drainUpdates(twap.Execute(context.Background(), f))

	var sawPartial bool
	for _, u := range updates {
		if absFloat(u.FilledQty-3) < 1e-9 && u.Status == StatusRunning {
			sawPartial = true
		}
	}
	if !sawPartial {
		t.Error("want intermediate update with FilledQty=3, did not see one")
	}

	final := updates[len(updates)-1]
	if final.Status != StatusSuccess {
		t.Errorf("want StatusSuccess, got %s", final.Status)
	}
	if absFloat(final.FilledQty-10) > 1e-9 {
		t.Errorf("want FilledQty=10, got %f", final.FilledQty)
	}
}

func TestFailedChildOrderSell(t *testing.T) {
	f := NewFakeExchange()
	f.PlaceResponses = []PlaceResponse{
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		{Err: fmt.Errorf("exchange error")},
		{Order: &Order{ID: "slice-1", Status: orderStatusOpen}},
	}
	f.GetResponses["slice-1"] = []GetResponse{
		{Order: &Order{ID: "slice-1", FilledQty: 5, AvgPrice: 100, Status: orderStatusFilled}},
	}

	twap := NewTWAPStrategy("BTC-USD", "sell", 10, 50*time.Millisecond, 2)
	twap.PollInterval = 10 * time.Millisecond

	updates := drainUpdates(twap.Execute(context.Background(), f))
	final := updates[len(updates)-1]

	if final.Status != StatusPartial {
		t.Errorf("want StatusPartial, got %s", final.Status)
	}
	if len(final.Errors) == 0 {
		t.Error("want at least one error describing the failed slice")
	}
	if absFloat(final.FilledQty-5) > 1e-9 {
		t.Errorf("want FilledQty=5, got %f", final.FilledQty)
	}
}

func TestCleanShutdownSell(t *testing.T) {
	f := NewFakeExchange()
	f.PlaceResponses = []PlaceResponse{
		{Order: &Order{ID: "slice-0", Status: orderStatusOpen}},
	}
	f.GetResponses["slice-0"] = []GetResponse{
		{Order: &Order{ID: "slice-0", FilledQty: 0, Status: orderStatusOpen}},
	}

	twap := NewTWAPStrategy("BTC-USD", "sell", 10, 10*time.Millisecond, 1)
	twap.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	ch := twap.Execute(ctx, f)

	time.Sleep(50 * time.Millisecond)
	cancel()

	updates := drainUpdates(ch)
	final := updates[len(updates)-1]

	if final.Status != StatusCancelled {
		t.Errorf("want StatusCancelled, got %s", final.Status)
	}
	if len(f.CancelledOrders) == 0 {
		t.Error("want CancelOrder called for active order, got none")
	}
	if f.CancelledOrders[0] != "slice-0" {
		t.Errorf("want CancelOrder called for slice-0, got %s", f.CancelledOrders[0])
	}
}

func TestHappyPathSell(t *testing.T) {
	f := NewFakeExchange()
	f.PlaceResponses = []PlaceResponse{
		{Order: &Order{ID: "slice-0", Status: orderStatusOpen}},
		{Order: &Order{ID: "slice-1", Status: orderStatusOpen}},
		{Order: &Order{ID: "slice-2", Status: orderStatusOpen}},
	}
	f.GetResponses["slice-0"] = []GetResponse{
		{Order: &Order{ID: "slice-0", FilledQty: 4, AvgPrice: 100, Status: orderStatusFilled}},
	}
	f.GetResponses["slice-1"] = []GetResponse{
		{Order: &Order{ID: "slice-1", FilledQty: 4, AvgPrice: 100, Status: orderStatusFilled}},
	}
	f.GetResponses["slice-2"] = []GetResponse{
		{Order: &Order{ID: "slice-2", FilledQty: 4, AvgPrice: 100, Status: orderStatusFilled}},
	}

	twap := NewTWAPStrategy("BTC-USD", "sell", 12, 60*time.Millisecond, 3)
	twap.PollInterval = 10 * time.Millisecond

	updates := drainUpdates(twap.Execute(context.Background(), f))
	final := updates[len(updates)-1]

	if final.Status != StatusSuccess {
		t.Errorf("want StatusSuccess, got %s", final.Status)
	}
	if absFloat(final.FilledQty-12) > 1e-9 {
		t.Errorf("want FilledQty=12, got %f", final.FilledQty)
	}
	if final.SlicesFilled != 3 {
		t.Errorf("want SlicesFilled=3, got %d", final.SlicesFilled)
	}
}