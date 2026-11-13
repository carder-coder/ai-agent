// 本文件提供 Provider 主备切换的管理接口；未配置管理令牌时接口关闭。
package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/carder-coder/ai-agent/gateway/internal/provider/failover"
	"github.com/carder-coder/ai-agent/gateway/internal/trace"
)

type providerSwitchRequest struct {
	Provider string `json:"provider"`
}

type providerSwitchResponse struct {
	Previous string            `json:"previous"`
	Snapshot failover.Snapshot `json:"snapshot"`
	SwitchMS int64             `json:"switch_ms"`
}

// adminProviderHandler 查询或手动切换当前生效 Provider，并把切换写入 trace。
func adminProviderHandler(manager *failover.Manager, token, traceDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			writeError(w, http.StatusNotFound, "provider admin is disabled", "invalid_request_error", "provider_admin_disabled")
			return
		}
		if !validBearerToken(r.Header.Get("Authorization"), token) {
			writeError(w, http.StatusUnauthorized, "admin authentication failed", "authentication_error", "invalid_admin_token")
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, manager.Snapshot())
		case http.MethodPost:
			var request providerSwitchRequest
			if !decodeStrictRequest(w, r, &request) {
				return
			}
			if request.Provider == "" {
				writeError(w, http.StatusBadRequest, "provider is required", "invalid_request_error", "invalid_request")
				return
			}
			snapshot, previous, elapsed, err := manager.SwitchTo(request.Provider, "manual: admin request")
			if err != nil {
				if errors.Is(err, failover.ErrUnknownProvider) {
					writeError(w, http.StatusNotFound, "provider is not registered", "invalid_request_error", "provider_not_found")
					return
				}
				if errors.Is(err, failover.ErrSameProvider) {
					writeError(w, http.StatusConflict, "provider is already active", "invalid_request_error", "provider_already_active")
					return
				}
				writeError(w, http.StatusServiceUnavailable, "provider switch failed", "server_error", "provider_switch_failed")
				return
			}
			log.Printf("provider switched previous=%s active=%s elapsed_ms=%d count=%d",
				previous, snapshot.Active, elapsed, snapshot.SwitchCount)
			recordProviderSwitch(r, traceDir, previous, snapshot, elapsed)
			writeJSON(w, providerSwitchResponse{Previous: previous, Snapshot: snapshot, SwitchMS: elapsed})
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		}
	}
}

// recordProviderSwitch 把切换事件写入本次请求的 trace；写入失败不影响切换结果。
func recordProviderSwitch(r *http.Request, traceDir, previous string, snapshot failover.Snapshot, elapsed int64) {
	recorder, err := trace.NewRecorder(traceDir, trace.IDFromContext(r.Context()), "POST /v1/admin/provider")
	if err != nil {
		log.Printf("创建切换审计 trace 失败：%v", err)
		return
	}
	if err := recorder.RecordProviderSwitch(previous, snapshot.Active, snapshot.LastReason, elapsed, snapshot.SwitchCount); err != nil {
		log.Printf("写入切换审计事件失败：%v", err)
	}
	if err := recorder.End("provider_switched", 0); err != nil {
		log.Printf("结束切换审计 trace 失败：%v", err)
	}
}

// writeJSON 输出 JSON 响应并记录编码错误。
func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("编码响应失败：%v", err)
	}
}
