// 本文件实现 Provider 调用的 429 退避重试、熔断器和防护适配器。
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

// ErrCircuitOpen 表示熔断器处于打开状态，当前请求未发送到上游。
var ErrCircuitOpen = errors.New("provider circuit breaker is open")

// 仅对明确返回的 429 退避重试；网络错误可能已被上游受理，不能安全重放。
var defaultDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// Policy 定义收到可重试响应后各次重试之间的等待时长。
type Policy struct {
	Delays []time.Duration
}

// DefaultPolicy 返回最多三次、按 1/2/4 秒退避的默认策略。
func DefaultPolicy(maxRetries int) Policy {
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries > len(defaultDelays) {
		maxRetries = len(defaultDelays)
	}
	return Policy{Delays: append([]time.Duration(nil), defaultDelays[:maxRetries]...)}
}

// Do 调用 Provider，并仅对尚未开始流式响应的 429 按策略退避重试。
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

// CircuitBreaker 在连续上游故障后暂时阻止请求，并限制半开探测并发数。
type CircuitBreaker struct {
	mu          sync.Mutex
	threshold   int
	openFor     time.Duration
	failures    int
	openUntil   time.Time
	probeActive bool
}

// NewCircuitBreaker 创建指定失败阈值和打开时长的熔断器。
func NewCircuitBreaker(threshold int, openFor time.Duration) (*CircuitBreaker, error) {
	if threshold < 1 {
		return nil, errors.New("circuit breaker threshold must be positive")
	}
	if openFor <= 0 {
		return nil, errors.New("circuit breaker open duration must be positive")
	}
	return &CircuitBreaker{threshold: threshold, openFor: openFor}, nil
}

// Allow 判断当前请求是否可执行；半开状态最多放行一个探测请求。
func (b *CircuitBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if time.Now().Before(b.openUntil) || b.probeActive {
		return false
	}
	// 熔断到期后只放行一个探测请求，避免故障上游被并发流量瞬间打满。
	b.probeActive = true
	return true
}

// RecordSuccess 清空连续失败数并关闭熔断器。
func (b *CircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
	b.probeActive = false
}

// RecordFailure 累加失败数，并在达到阈值时重新打开熔断器。
func (b *CircuitBreaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.probeActive = false
	if b.failures >= b.threshold {
		b.openUntil = time.Now().Add(b.openFor)
	}
}

// Adapter 为 Provider 调用统一增加 429 重试和熔断保护。
type Adapter struct {
	next    provider.Adapter
	policy  Policy
	breaker *CircuitBreaker
}

// NewAdapter 创建带重试和熔断保护的 Provider Adapter。
func NewAdapter(next provider.Adapter, maxRetries int) (*Adapter, error) {
	if next == nil {
		return nil, errors.New("provider adapter must not be nil")
	}
	// 五次连续上游故障后熔断 30 秒；认证与限流响应由 Chat 单独排除。
	breaker, err := NewCircuitBreaker(5, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("create circuit breaker: %w", err)
	}
	return &Adapter{next: next, policy: DefaultPolicy(maxRetries), breaker: breaker}, nil
}

// Chat 在熔断器允许时调用上游，并根据结果更新熔断状态。
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
