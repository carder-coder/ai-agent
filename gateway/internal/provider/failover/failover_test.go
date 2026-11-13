// 本文件验证主备切换的自动切换、手动切换、阈值与边界行为。
package failover

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/carder-coder/ai-agent/gateway/internal/provider"
)

type stubAdapter struct {
	mu     sync.Mutex
	calls  int
	err    error
	status int
}

func (s *stubAdapter) Chat(context.Context, []byte, bool) (provider.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return provider.Response{}, s.err
	}
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	return provider.Response{StatusCode: status, Body: []byte(`{}`)}, nil
}

func (s *stubAdapter) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func newManager(t *testing.T, primary, backup *stubAdapter, threshold int) *Manager {
	t.Helper()
	adapters := map[string]provider.Adapter{"primary": primary}
	backupName := ""
	if backup != nil {
		adapters["backup"] = backup
		backupName = "backup"
	}
	manager, err := New("primary", backupName, adapters, threshold)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}

func TestChatAutoSwitchesToBackupAndRetriesOnce(t *testing.T) {
	primary := &stubAdapter{err: errors.New("connection refused")}
	backup := &stubAdapter{}
	manager := newManager(t, primary, backup, 1)

	response, err := manager.Chat(context.Background(), []byte(`{}`), false)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("Chat() = %+v, %v; want backup success", response, err)
	}
	if manager.Active() != "backup" {
		t.Fatalf("active = %q, want backup", manager.Active())
	}
	if primary.callCount() != 1 || backup.callCount() != 1 {
		t.Fatalf("calls primary=%d backup=%d, want 1/1", primary.callCount(), backup.callCount())
	}
	snapshot := manager.Snapshot()
	if snapshot.SwitchCount != 1 || snapshot.LastReason == "" || snapshot.LastSwitchAt == "" {
		t.Fatalf("switch audit is incomplete: %+v", snapshot)
	}
}

func TestChatSwitchesOnlyAfterThreshold(t *testing.T) {
	primary := &stubAdapter{err: errors.New("connection refused")}
	backup := &stubAdapter{}
	manager := newManager(t, primary, backup, 3)

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := manager.Chat(context.Background(), []byte(`{}`), false); err == nil {
			t.Fatalf("attempt %d unexpectedly succeeded", attempt)
		}
		if manager.Active() != "primary" {
			t.Fatalf("attempt %d switched too early: active=%q", attempt, manager.Active())
		}
		if failures := manager.Snapshot().ConsecutiveFailures; failures != attempt {
			t.Fatalf("attempt %d failures=%d", attempt, failures)
		}
	}
	response, err := manager.Chat(context.Background(), []byte(`{}`), false)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("third failure did not fail over: %+v, %v", response, err)
	}
	if manager.Active() != "backup" || manager.Snapshot().SwitchCount != 1 {
		t.Fatalf("unexpected switch state: %+v", manager.Snapshot())
	}
}

func TestManualSwitchIsUnderThreeSeconds(t *testing.T) {
	primary := &stubAdapter{}
	backup := &stubAdapter{}
	manager := newManager(t, primary, backup, 3)

	snapshot, previous, elapsed, err := manager.SwitchTo("backup", "manual")
	if err != nil {
		t.Fatalf("SwitchTo() error = %v", err)
	}
	if previous != "primary" || snapshot.Active != "backup" {
		t.Fatalf("switch result = %+v previous=%q", snapshot, previous)
	}
	if elapsed < 0 || elapsed >= 3000 {
		t.Fatalf("switch elapsed = %d ms, want 0-2999", elapsed)
	}
	if _, err := manager.Chat(context.Background(), []byte(`{}`), false); err != nil {
		t.Fatalf("Chat() after switch error = %v", err)
	}
	if primary.callCount() != 0 || backup.callCount() != 1 {
		t.Fatalf("traffic did not move to backup: primary=%d backup=%d", primary.callCount(), backup.callCount())
	}
}

func TestManualSwitchRejectsUnknownAndSameProvider(t *testing.T) {
	manager := newManager(t, &stubAdapter{}, &stubAdapter{}, 3)
	if _, _, _, err := manager.SwitchTo("missing", "manual"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("unknown provider error = %v", err)
	}
	if _, _, _, err := manager.SwitchTo("primary", "manual"); !errors.Is(err, ErrSameProvider) {
		t.Fatalf("same provider error = %v", err)
	}
	if manager.Active() != "primary" {
		t.Fatalf("failed switches changed active provider: %q", manager.Active())
	}
}

func TestNoBackupKeepsFailingOnPrimary(t *testing.T) {
	primary := &stubAdapter{err: errors.New("connection refused")}
	manager := newManager(t, primary, nil, 1)
	if _, err := manager.Chat(context.Background(), []byte(`{}`), false); err == nil {
		t.Fatal("Chat() succeeded without a backup provider")
	}
	if manager.Active() != "primary" || manager.Snapshot().SwitchCount != 0 {
		t.Fatalf("unexpected state without backup: %+v", manager.Snapshot())
	}
}

func TestUpstreamFiveHundredCountsButDoesNotRetry(t *testing.T) {
	primary := &stubAdapter{status: http.StatusInternalServerError}
	backup := &stubAdapter{}
	manager := newManager(t, primary, backup, 1)

	response, err := manager.Chat(context.Background(), []byte(`{}`), false)
	if err != nil || response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Chat() = %+v, %v; want upstream 500 passthrough", response, err)
	}
	// 5xx 说明上游仍可应答，本次请求不重放；但后续请求应走备用。
	if manager.Active() != "backup" || backup.callCount() != 0 {
		t.Fatalf("active=%q backup calls=%d, want backup without replay", manager.Active(), backup.callCount())
	}
	if _, err := manager.Chat(context.Background(), []byte(`{}`), false); err != nil {
		t.Fatalf("next request on backup failed: %v", err)
	}
}

func TestContextCanceledDoesNotCountAsFailure(t *testing.T) {
	primary := &stubAdapter{err: context.Canceled}
	manager := newManager(t, primary, &stubAdapter{}, 1)
	if _, err := manager.Chat(context.Background(), []byte(`{}`), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat() error = %v, want context.Canceled", err)
	}
	if snapshot := manager.Snapshot(); snapshot.ConsecutiveFailures != 0 || snapshot.SwitchCount != 0 {
		t.Fatalf("canceled request changed switch state: %+v", snapshot)
	}
}

func TestNewValidatesProviderNames(t *testing.T) {
	adapters := map[string]provider.Adapter{"primary": &stubAdapter{}, "backup": &stubAdapter{}}
	if _, err := New("primary", "primary", adapters, 3); err == nil {
		t.Fatal("New() accepted identical primary and backup")
	}
	if _, err := New("primary", "missing", adapters, 3); err == nil {
		t.Fatal("New() accepted an unregistered backup")
	}
	if _, err := New("", "", adapters, 3); err == nil {
		t.Fatal("New() accepted an empty primary")
	}
	manager, err := New("primary", "", adapters, 0)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if manager.Snapshot().Threshold != 1 {
		t.Fatalf("threshold = %d, want normalized 1", manager.Snapshot().Threshold)
	}
}
