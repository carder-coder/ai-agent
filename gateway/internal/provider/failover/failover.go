// 本文件实现 Provider 主备切换：手动切换、连续故障自动切换与切换耗时统计。
package failover

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/carder-coder/ai-agent/gateway/internal/provider"
)

var (
	// ErrUnknownProvider 表示切换目标不在已注册的 Provider 清单内。
	ErrUnknownProvider = errors.New("failover target provider is not registered")
	// ErrSameProvider 表示目标 Provider 已经是当前生效的 Provider。
	ErrSameProvider = errors.New("provider is already active")
)

// Snapshot 保存当前生效 Provider 与最近一次切换的可核对信息。
type Snapshot struct {
	Active              string `json:"active"`
	Primary             string `json:"primary"`
	Backup              string `json:"backup"`
	Threshold           int    `json:"threshold"`
	SwitchCount         int    `json:"switch_count"`
	LastSwitchAt        string `json:"last_switch_at,omitempty"`
	LastSwitchMS        int64  `json:"last_switch_ms"`
	LastReason          string `json:"last_reason,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
}

// Manager 在主 Provider 与备用 Provider 之间切换，并对调用方保持单一 Adapter 语义。
type Manager struct {
	mu           sync.Mutex
	adapters     map[string]provider.Adapter
	primary      string
	backup       string
	active       string
	threshold    int
	failures     int
	switchCount  int
	lastSwitch   time.Time
	lastSwitchMS int64
	lastReason   string
	clock        func() time.Time
}

// New 校验主备名称与阈值，返回可并发使用的切换管理器。
func New(primary, backup string, adapters map[string]provider.Adapter, threshold int) (*Manager, error) {
	primary = strings.TrimSpace(primary)
	backup = strings.TrimSpace(backup)
	if primary == "" {
		return nil, errors.New("primary provider must not be empty")
	}
	if _, exists := adapters[primary]; !exists {
		return nil, fmt.Errorf("primary provider %q is not registered", primary)
	}
	if backup != "" {
		if backup == primary {
			return nil, errors.New("backup provider must differ from primary")
		}
		if _, exists := adapters[backup]; !exists {
			return nil, fmt.Errorf("backup provider %q is not registered", backup)
		}
	}
	if threshold < 1 {
		threshold = 1
	}
	return &Manager{
		adapters:  adapters,
		primary:   primary,
		backup:    backup,
		active:    primary,
		threshold: threshold,
		clock:     time.Now,
	}, nil
}

// Active 返回当前生效的 Provider 名称。
func (m *Manager) Active() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// Snapshot 返回切换状态快照，供管理接口与实验记录核对。
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

func (m *Manager) snapshotLocked() Snapshot {
	snapshot := Snapshot{
		Active:              m.active,
		Primary:             m.primary,
		Backup:              m.backup,
		Threshold:           m.threshold,
		SwitchCount:         m.switchCount,
		LastSwitchMS:        m.lastSwitchMS,
		LastReason:          m.lastReason,
		ConsecutiveFailures: m.failures,
	}
	if !m.lastSwitch.IsZero() {
		snapshot.LastSwitchAt = m.lastSwitch.Format(time.RFC3339Nano)
	}
	return snapshot
}

// SwitchTo 手动把入口切到指定 Provider；返回切换前后的名称与实际耗时。
func (m *Manager) SwitchTo(name, reason string) (Snapshot, string, int64, error) {
	name = strings.TrimSpace(name)
	started := m.clock()
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.active
	if _, exists := m.adapters[name]; !exists {
		return m.snapshotLocked(), previous, 0, ErrUnknownProvider
	}
	if name == m.active {
		return m.snapshotLocked(), previous, 0, ErrSameProvider
	}
	m.applySwitchLocked(name, reason)
	return m.snapshotLocked(), previous, m.clock().Sub(started).Milliseconds(), nil
}

// applySwitchLocked 更新生效 Provider 并记录切换次数、时间与原因。
func (m *Manager) applySwitchLocked(name, reason string) {
	m.active = name
	m.failures = 0
	m.switchCount++
	m.lastSwitch = m.clock()
	m.lastSwitchMS = 0
	m.lastReason = reason
}

// Chat 调用当前生效 Provider；连接级失败且达到阈值时自动切到备用并重试一次。
func (m *Manager) Chat(ctx context.Context, body []byte, stream bool) (provider.Response, error) {
	m.mu.Lock()
	active := m.active
	adapter := m.adapters[active]
	m.mu.Unlock()

	response, err := adapter.Chat(ctx, body, stream)
	if !providerUnavailable(response, err) {
		// 只有明确的 2xx 才清零连续失败；4xx/429 说明 Provider 仍在线。
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			m.recordSuccess()
		}
		return response, err
	}
	switched, nextAdapter := m.recordFailure(active)
	// 只有连接级错误才在同一请求内用备用重试；上游 5xx 交给调用方收敛，避免重复计费。
	if switched && err != nil && !errors.Is(err, context.Canceled) {
		retried, retryErr := nextAdapter.Chat(ctx, body, stream)
		if !providerUnavailable(retried, retryErr) {
			m.recordSuccess()
		}
		return retried, retryErr
	}
	return response, err
}

func (m *Manager) recordSuccess() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures = 0
}

// recordFailure 累加连续失败；达到阈值且有备用时自动切换，返回切换后的 Adapter。
func (m *Manager) recordFailure(observed string) (bool, provider.Adapter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 并发请求可能已被其他 goroutine 切换，只对仍在生效的 Provider 计数。
	if observed != m.active {
		return false, m.adapters[m.active]
	}
	m.failures++
	if m.failures < m.threshold || m.backup == "" || m.active != m.primary {
		return false, m.adapters[m.active]
	}
	m.applySwitchLocked(m.backup, "auto: consecutive upstream failures")
	return true, m.adapters[m.active]
}

// providerUnavailable 判断响应或错误是否应计入 Provider 不可用。
func providerUnavailable(response provider.Response, err error) bool {
	if err != nil {
		return !errors.Is(err, context.Canceled)
	}
	return response.StatusCode >= http.StatusInternalServerError
}
