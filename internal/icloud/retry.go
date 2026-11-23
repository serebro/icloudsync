package icloud

import (
	"context"
	"time"
)

// RetryDelays is the backoff between attempts of a transient-failing request.
// Together they ride out short network outages (DNS timeouts, Wi-Fi reconnects)
// of about two and a half minutes. A package var so tests can shorten it.
var RetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 45 * time.Second, 75 * time.Second}

// Retry calls fn until it succeeds, fails with a non-transient error, ctx ends,
// or RetryDelays run out. onRetry (optional) is told about each retried error.
func Retry[T any](ctx context.Context, onRetry func(error, time.Duration), fn func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for attempt := 0; ; attempt++ {
		v, err = fn()
		if err == nil || !IsTransient(err) || ctx.Err() != nil || attempt >= len(RetryDelays) {
			return v, err
		}
		d := RetryDelays[attempt]
		if onRetry != nil {
			onRetry(err, d)
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-time.After(d):
		}
	}
}
