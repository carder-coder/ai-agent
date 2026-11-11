package retry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/provider"
)

var ErrCircuitOpen = errors.New("provider circuit breaker is open")

var defaultDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

type Policy struct {
	Delays []time.Duration
}

func DefaultPolicy(maxRetries int) Policy {
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries > len(defaultDelays) {
		maxRetries = len(defaultDelays)
	}
	return Policy{Delays: append([]time.Duration(nil), defaultDelays[:maxRetries]...)}
}

func Do(
	ctx context.Context,
	policy Policy,
	call func(context.Context) (provider.Response, error),
) (provider.Response, error) {
	for attempt := 0; ; attempt++ {
		response, err := call(ctx)
		if err != nil {
			return provider.Response{Attempts: attempt + 1}, err
		}
		response.Attempts = attempt + 1
		if response.StatusCode != http.StatusTooManyRequests || response.Stream != nil ||
			attempt >= len(policy.Delays) {
			return response, nil
		}

		timer := time.NewTimer(policy.Delays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return provider.Response{Attempts: attempt + 1}, ctx.Err()
		case <-timer.C:
		}
	}
}

type CircuitBreaker struct {
	mu          sync.Mutex
	threshold   int
	openFor     time.Duration
	failures    int
	openUntil   time.Time
	probeActive bool
}

func NewCircuitBreaker(threshold int, openFor time.Duration) (*CircuitBreaker, error) {
	if threshold < 1 {
		return nil, errors.New("circuit breaker threshold must be positive")
	}
	if openFor <= 0 {
		return nil, errors.New("circuit breaker open duration must be positive")
	}
	return &CircuitBreaker{threshold: threshold, openFor: openFor}, nil
}

func (b *CircuitBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if time.Now().Before(b.openUntil) || b.probeActive {
		return false
	}
	b.probeActive = true
	return true
}

func (b *CircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.probeActive = false
}

func (b *CircuitBreaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.probeActive = false
	if b.failures >= b.threshold {
		b.openUntil = time.Now().Add(b.openFor)
	}
}

type Adapter struct {
	next    provider.Adapter
	policy  Policy
	breaker *CircuitBreaker
}

func NewAdapter(next provider.Adapter, maxRetries int) (*Adapter, error) {
	if next == nil {
		return nil, errors.New("provider adapter must not be nil")
	}
	breaker, err := NewCircuitBreaker(5, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("create circuit breaker: %w", err)
	}
	return &Adapter{next: next, policy: DefaultPolicy(maxRetries), breaker: breaker}, nil
}

func (a *Adapter) Chat(ctx context.Context, body []byte, stream bool) (provider.Response, error) {
	if !a.breaker.Allow() {
		return provider.Response{}, ErrCircuitOpen
	}
	response, err := Do(ctx, a.policy, func(ctx context.Context) (provider.Response, error) {
		return a.next.Chat(ctx, body, stream)
	})
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, provider.ErrAuthentication) {
			a.breaker.RecordFailure()
		}
		return response, err
	}
	if response.StatusCode >= http.StatusInternalServerError || response.StatusCode < http.StatusOK {
		a.breaker.RecordFailure()
	} else {
		a.breaker.RecordSuccess()
	}
	return response, nil
}
