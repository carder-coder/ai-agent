package api

import (
	"net/http"

	"github.com/carder-coder/ai-agent/gateway/internal/config"
	"github.com/carder-coder/ai-agent/gateway/internal/provider/doubao"
)

func NewRouter(cfg config.Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/v1/chat/completions", chatHandler(doubao.NewClient(cfg)))
	mux.HandleFunc("/metrics", notImplementedHandler)
	return mux
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("{\"status\":\"ok\"}\n")); err != nil {
		return
	}
}

func notImplementedHandler(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, "endpoint is not implemented yet", "server_error", "not_implemented")
}
