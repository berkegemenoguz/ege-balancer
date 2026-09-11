package proxy

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/berkegemenoguz/ege-balancer/internal/config"
)

func TestRetryBudgetAllowsTheMinimumAtAnyLoad(t *testing.T) {
	budget := newRetryBudget(config.Retry{BudgetPercent: 20, MinRetryConcurrency: 3})
	// A fifth of one request is no retry at all; the minimum still allows three.
	budget.requestStarted()

	for i := range 3 {
		if !budget.tryRetry() {
			t.Fatalf("retry %d was refused, want the minimum of 3 allowed", i+1)
		}
	}
	if budget.tryRetry() {
		t.Error("a fourth retry was allowed, want the minimum to be the limit at this load")
	}
}

func TestRetryBudgetFollowsTheRequestsInFlight(t *testing.T) {
	budget := newRetryBudget(config.Retry{BudgetPercent: 20, MinRetryConcurrency: 3})
	for range 100 {
		budget.requestStarted()
	}

	allowed := 0
	for budget.tryRetry() {
		allowed++
	}
	if allowed != 20 {
		t.Fatalf("allowed %d retries with 100 requests in flight, want 20", allowed)
	}

	// Half the requests finish, so the limit falls to 10, and 10 of the 20
	// retries finish with them: the budget is full again.
	for range 50 {
		budget.requestFinished()
	}
	for range 10 {
		budget.retryFinished()
	}
	if budget.tryRetry() {
		t.Error("allowed an 11th retry with a limit of 10")
	}

	budget.retryFinished()
	if !budget.tryRetry() {
		t.Error("refused a retry after one of the 10 in flight finished")
	}
}

func TestRetryBudgetHoldsUnderConcurrency(t *testing.T) {
	budget := newRetryBudget(config.Retry{BudgetPercent: 20, MinRetryConcurrency: 3})
	for range 100 {
		budget.requestStarted()
	}

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			if budget.tryRetry() {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()

	if got := allowed.Load(); got != 20 {
		t.Errorf("200 concurrent retries were allowed %d times, want exactly 20", got)
	}
}

func BenchmarkRetryBudget(b *testing.B) {
	budget := newRetryBudget(config.Retry{BudgetPercent: 20, MinRetryConcurrency: 3})
	for b.Loop() {
		budget.requestStarted()
		if budget.tryRetry() {
			budget.retryFinished()
		}
		budget.requestFinished()
	}
}

// BenchmarkRetryBudgetParallel is the cost every request pays: the budget is
// shared by the whole balancer, so its counters are contended.
func BenchmarkRetryBudgetParallel(b *testing.B) {
	budget := newRetryBudget(config.Retry{BudgetPercent: 20, MinRetryConcurrency: 3})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			budget.requestStarted()
			if budget.tryRetry() {
				budget.retryFinished()
			}
			budget.requestFinished()
		}
	})
}
