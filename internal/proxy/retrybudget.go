package proxy

import (
	"sync/atomic"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

// retryBudget caps the retries in flight at a share of the requests in flight.
// Without it every request may be retried max_retries times, so a pool that
// starts failing receives up to 1+max_retries times its traffic at the moment
// it can least absorb it. With it, retries add at most the configured share to
// the load, and a minimum keeps them possible when traffic is too light for the
// share to allow any.
//
// This is Envoy's retry budget: the limit follows the load at every moment, so
// there is no window or refill rate to tune.
type retryBudget struct {
	percent float64
	minimum int64

	requests atomic.Int64
	retries  atomic.Int64
}

// newRetryBudget returns an empty budget with the configured limits.
func newRetryBudget(cfg config.Retry) *retryBudget {
	return &retryBudget{percent: cfg.BudgetPercent, minimum: int64(cfg.MinRetryConcurrency)}
}

// requestStarted counts a request into the load the budget is a share of.
func (b *retryBudget) requestStarted() { b.requests.Add(1) }

// requestFinished counts a request out of the load.
func (b *retryBudget) requestFinished() { b.requests.Add(-1) }

// tryRetry reserves a retry if the budget allows one now. A reserved retry must
// be given back with retryFinished.
func (b *retryBudget) tryRetry() bool {
	if b.retries.Add(1) > b.limit() {
		b.retries.Add(-1)
		return false
	}
	return true
}

// retryFinished gives back a retry reserved by tryRetry.
func (b *retryBudget) retryFinished() { b.retries.Add(-1) }

// limit is how many retries may be in flight at the current load.
func (b *retryBudget) limit() int64 {
	return max(b.minimum, int64(b.percent/100*float64(b.requests.Load())))
}
