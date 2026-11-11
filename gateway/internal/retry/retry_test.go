package retry

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/provider"
)

func TestDoRetries429ThenReturnsSuccess(t *testing.T) {
	calls := 0
	response, err := Do(context.Background(), Policy{Delays: []time.Duration{time.Millisecond, time.Millisecond}},
		func(context.Context) (provider.Response, error) {
			calls++
			if calls < 3 {
				return provider.Response{StatusCode: http.StatusTooManyRequests}, nil
			}
			return provider.Response{StatusCode: http.StatusOK}, nil
		})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if calls != 3 || response.Attempts != 3 || response.StatusCode != http.StatusOK {
		t.Fatalf("calls = %d, response = %+v", calls, response)
	}
}

func TestDoDoesNotRetryAuthenticationFailures(t *testing.T) {
	calls := 0
	response, err := Do(context.Background(), Policy{Delays: []time.Duration{time.Millisecond}},
		func(context.Context) (provider.Response, error) {
			calls++
			return provider.Response{StatusCode: http.StatusUnauthorized}, nil
		})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if calls != 1 || response.Attempts != 1 {
		t.Fatalf("calls = %d, response = %+v", calls, response)
	}
}

func TestDoDoesNotRetry429AfterStreamHasStarted(t *testing.T) {
	calls := 0
	response, err := Do(context.Background(), Policy{Delays: []time.Duration{time.Millisecond}},
		func(context.Context) (provider.Response, error) {
			calls++
			return provider.Response{StatusCode: http.StatusTooManyRequests, Stream: testStream{}}, nil
		})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if calls != 1 || response.Attempts != 1 {
		t.Fatalf("calls = %d, response = %+v", calls, response)
	}
}

func TestDoCancelsDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	response, err := Do(ctx, Policy{Delays: []time.Duration{time.Second}}, func(context.Context) (provider.Response, error) {
		calls++
		cancel()
		return provider.Response{StatusCode: http.StatusTooManyRequests}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do() error = %v, want context.Canceled", err)
	}
	if calls != 1 || response.Attempts != 1 {
		t.Fatalf("calls = %d, response = %+v", calls, response)
	}
}

func TestCircuitBreakerAllowsOneHalfOpenProbe(t *testing.T) {
	breaker, err := NewCircuitBreaker(2, 15*time.Millisecond)
	if err != nil {
		t.Fatalf("NewCircuitBreaker() error = %v", err)
	}
	breaker.RecordFailure()
	breaker.RecordFailure()
	if breaker.Allow() {
		t.Fatal("circuit allowed a request while open")
	}
	time.Sleep(20 * time.Millisecond)
	if !breaker.Allow() {
		t.Fatal("circuit did not allow a half-open probe")
	}
	if breaker.Allow() {
		t.Fatal("circuit allowed more than one half-open probe")
	}
	breaker.RecordSuccess()
	if !breaker.Allow() {
		t.Fatal("successful probe did not close the circuit")
	}
}

func TestAdapterOpensCircuitAfterRepeatedUpstreamFailures(t *testing.T) {
	next := alwaysFailAdapter{}
	adapter, err := NewAdapter(next, 0)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	for range 5 {
		if _, err := adapter.Chat(context.Background(), nil, false); err != nil {
			t.Fatalf("Chat() error = %v", err)
		}
	}
	if _, err := adapter.Chat(context.Background(), nil, false); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Chat() error = %v, want ErrCircuitOpen", err)
	}
}

func TestAdapterDoesNotCountAuthenticationAsCircuitFailure(t *testing.T) {
	adapter, err := NewAdapter(authenticationAdapter{}, 0)
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	for range 6 {
		if _, err := adapter.Chat(context.Background(), nil, false); !errors.Is(err, provider.ErrAuthentication) {
			t.Fatalf("Chat() error = %v, want authentication error", err)
		}
	}
}

type alwaysFailAdapter struct{}

func (alwaysFailAdapter) Chat(context.Context, []byte, bool) (provider.Response, error) {
	return provider.Response{StatusCode: http.StatusBadGateway}, nil
}

type authenticationAdapter struct{}

func (authenticationAdapter) Chat(context.Context, []byte, bool) (provider.Response, error) {
	return provider.Response{}, provider.ErrAuthentication
}

type testStream struct{}

func (testStream) Next() (provider.Chunk, error) {
	return provider.Chunk{}, nil
}

func (testStream) Usage() provider.Usage {
	return provider.Usage{}
}

func (testStream) Close() error {
	return nil
}
