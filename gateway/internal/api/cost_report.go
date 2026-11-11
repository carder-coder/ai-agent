package api

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/carder-coder/ai-agent/gateway/internal/metrics"
)

func costReportHandler(collector *metrics.Metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(collector.CostReport()); err != nil {
			log.Printf("写入成本报告失败：%v", err)
		}
	}
}
