package executor

import "context"

// strategy interface implemented by TWAP and future strategies
type Strategy interface {
	// Execute starts the algorithm in a background goroutine and returns
	// a channel immediately. The caller reads progress updates via for-range.
	Execute(ctx context.Context, exchange Exchange) <-chan ProgressUpdate
}