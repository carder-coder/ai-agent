// 本文件验证 Provider 管理接口的鉴权、查询与手动切换行为。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/carder-coder/ai-agent/gateway/internal/provider"
	"github.com/carder-coder/ai-agent/gateway/internal/provider/failover"
	"github.com/carder-coder/ai-agent/gateway/internal/provider/mock"
)

const adminTestToken = "admin-test-token-0123456789abcdef"

func newAdminTestHandler(t *testing.T) (*failover.Manager, http.Handler) {
	t.Helper()
	manager, err := failover.New("primary", "backup", map[string]provider.Adapter{
		"primary": mock.New(),
		"backup":  mock.New(),
	}, 3)
	if err != nil {
		t.Fatalf("failover.New() error = %v", err)
	}
	return manager, adminProviderHandler(manager, adminTestToken, t.TempDir())
}

func doAdmin(handler http.Handler, method, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "/v1/admin/provider", strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestAdminProviderRequiresConfiguredToken(t *testing.T) {
	manager, _ := newAdminTestHandler(t)
	disabled := adminProviderHandler(manager, "", t.TempDir())
	response := doAdmin(disabled, http.MethodGet, "", "")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "provider_admin_disabled") {
		t.Fatalf("disabled admin returned %d body=%s", response.Code, response.Body)
	}
}

func TestAdminProviderRejectsWrongToken(t *testing.T) {
	_, handler := newAdminTestHandler(t)
	response := doAdmin(handler, http.MethodGet, "", "wrong-token")
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "invalid_admin_token") {
		t.Fatalf("wrong token returned %d body=%s", response.Code, response.Body)
	}
}

func TestAdminProviderSwitchAndSnapshot(t *testing.T) {
	manager, handler := newAdminTestHandler(t)
	snapshot := doAdmin(handler, http.MethodGet, "", adminTestToken)
	if snapshot.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d body=%s", snapshot.Code, snapshot.Body)
	}
	var initial failover.Snapshot
	if err := json.Unmarshal(snapshot.Body.Bytes(), &initial); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if initial.Active != "primary" || initial.Backup != "backup" {
		t.Fatalf("unexpected initial snapshot: %+v", initial)
	}

	switched := doAdmin(handler, http.MethodPost, `{"provider":"backup"}`, adminTestToken)
	if switched.Code != http.StatusOK {
		t.Fatalf("switch status = %d body=%s", switched.Code, switched.Body)
	}
	var result providerSwitchResponse
	if err := json.Unmarshal(switched.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode switch response: %v", err)
	}
	if result.Previous != "primary" || result.Snapshot.Active != "backup" {
		t.Fatalf("unexpected switch result: %+v", result)
	}
	if result.SwitchMS < 0 || result.SwitchMS >= 3000 {
		t.Fatalf("switch_ms = %d, want 0-2999", result.SwitchMS)
	}
	if manager.Active() != "backup" || result.Snapshot.SwitchCount != 1 || result.Snapshot.LastSwitchAt == "" {
		t.Fatalf("switch was not recorded: %+v", result.Snapshot)
	}

	repeat := doAdmin(handler, http.MethodPost, `{"provider":"backup"}`, adminTestToken)
	if repeat.Code != http.StatusConflict || !strings.Contains(repeat.Body.String(), "provider_already_active") {
		t.Fatalf("repeat switch returned %d body=%s", repeat.Code, repeat.Body)
	}
	unknown := doAdmin(handler, http.MethodPost, `{"provider":"missing"}`, adminTestToken)
	if unknown.Code != http.StatusNotFound || !strings.Contains(unknown.Body.String(), "provider_not_found") {
		t.Fatalf("unknown provider returned %d body=%s", unknown.Code, unknown.Body)
	}
}

func TestAdminProviderSwitchIsAudited(t *testing.T) {
	traceDir := t.TempDir()
	manager, err := failover.New("primary", "backup", map[string]provider.Adapter{
		"primary": mock.New(),
		"backup":  mock.New(),
	}, 3)
	if err != nil {
		t.Fatalf("failover.New() error = %v", err)
	}
	handler := adminProviderHandler(manager, adminTestToken, traceDir)
	if response := doAdmin(handler, http.MethodPost, `{"provider":"backup"}`, adminTestToken); response.Code != http.StatusOK {
		t.Fatalf("switch status = %d body=%s", response.Code, response.Body)
	}
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		t.Fatalf("read trace dir: %v", err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".jsonl") {
		t.Fatalf("unexpected trace files: %v", entries)
	}
	content, err := os.ReadFile(filepath.Join(traceDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read trace file: %v", err)
	}
	if !strings.Contains(string(content), `"kind":"provider_switch"`) ||
		!strings.Contains(string(content), `"active":"backup"`) {
		t.Fatalf("switch trace is missing audit fields: %s", content)
	}
}
