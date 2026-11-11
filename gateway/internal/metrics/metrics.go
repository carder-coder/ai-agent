// 本文件采集网关请求指标，并以 Prometheus 文本格式提供 /metrics。
package metrics

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var histogramBounds = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 0.8, 1, 2, 5, 10}

type histogram struct {
	buckets []uint64
	count   uint64
	sum     float64
}

// Metrics 并发安全地累计 HTTP 请求、错误、延迟和首字节观测值。
type Metrics struct {
	mu               sync.RWMutex
	startedAt        time.Time
	requests         uint64
	errors           uint64
	recentRequests   []time.Time
	requestDuration  histogram
	firstByteLatency histogram
}

// New 创建并初始化一组网关指标。
func New() *Metrics {
	now := time.Now()
	return &Metrics{
		startedAt:        now,
		requestDuration:  histogram{buckets: make([]uint64, len(histogramBounds))},
		firstByteLatency: histogram{buckets: make([]uint64, len(histogramBounds))},
	}
}

// Wrap 记录被包装 handler 的状态码、总耗时和首个响应字节时间。
func (m *Metrics) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		tracked := &responseWriter{ResponseWriter: w, started: started, status: http.StatusOK}
		var writer http.ResponseWriter = tracked
		if flusher, ok := w.(http.Flusher); ok {
			writer = &flushResponseWriter{responseWriter: tracked, flusher: flusher}
		}
		next.ServeHTTP(writer, r)
		m.observe(time.Since(started), tracked.firstByte, tracked.hasFirstByte, tracked.status)
	})
}

// ServeHTTP 以 Prometheus 文本格式输出指标，仅允许 GET 请求。
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	window := time.Since(m.startedAt)
	if window > time.Minute {
		window = time.Minute
	}
	// QPS 使用最近一分钟内完成的请求数；服务启动未满一分钟时按实际运行时长计算。
	qps := float64(0)
	if window > 0 {
		qps = float64(len(m.recentRequests)) / window.Seconds()
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP gateway_requests_per_second Requests per second over the active rolling window.\n")
	_, _ = fmt.Fprintf(w, "# TYPE gateway_requests_per_second gauge\ngateway_requests_per_second %s\n", strconv.FormatFloat(qps, 'f', 6, 64))
	_, _ = fmt.Fprintf(w, "# HELP gateway_requests_total Completed HTTP requests.\n# TYPE gateway_requests_total counter\ngateway_requests_total %d\n", m.requests)
	_, _ = fmt.Fprintf(w, "# HELP gateway_errors_total HTTP requests with status 4xx or 5xx.\n# TYPE gateway_errors_total counter\ngateway_errors_total %d\n", m.errors)
	writeHistogram(w, "gateway_request_duration_seconds", "HTTP request duration in seconds.", m.requestDuration)
	writeHistogram(w, "gateway_first_byte_seconds", "Time to first response body byte in seconds.", m.firstByteLatency)
}

func (m *Metrics) observe(duration, firstByte time.Duration, hasFirstByte bool, status int) {
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests++
	if status >= http.StatusBadRequest {
		m.errors++
	}
	m.requestDuration.observe(duration.Seconds())
	if hasFirstByte {
		m.firstByteLatency.observe(firstByte.Seconds())
	}
	m.recentRequests = append(m.recentRequests, now)
	firstValid := 0
	for firstValid < len(m.recentRequests) && m.recentRequests[firstValid].Before(cutoff) {
		firstValid++
	}
	if firstValid > 0 {
		m.recentRequests = append([]time.Time(nil), m.recentRequests[firstValid:]...)
	}
}

func (h *histogram) observe(value float64) {
	h.count++
	h.sum += value
	// Prometheus histogram 的桶是累积计数，同一次观测会计入所有不小于它的边界。
	for i, bound := range histogramBounds {
		if value <= bound {
			h.buckets[i]++
		}
	}
}

func writeHistogram(w http.ResponseWriter, name, help string, h histogram) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for i, bound := range histogramBounds {
		_, _ = fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", name, strconv.FormatFloat(bound, 'f', -1, 64), h.buckets[i])
	}
	_, _ = fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, h.count)
	_, _ = fmt.Fprintf(w, "%s_sum %s\n%s_count %d\n", name, strconv.FormatFloat(h.sum, 'f', 6, 64), name, h.count)
}

type responseWriter struct {
	http.ResponseWriter
	started       time.Time
	status        int
	firstByte     time.Duration
	hasFirstByte  bool
	headerWritten bool
}

func (w *responseWriter) WriteHeader(status int) {
	if w.headerWritten {
		return
	}
	w.headerWritten = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(body []byte) (int, error) {
	if !w.headerWritten {
		w.WriteHeader(http.StatusOK)
	}
	if !w.hasFirstByte && len(body) > 0 {
		w.hasFirstByte = true
		w.firstByte = time.Since(w.started)
	}
	return w.ResponseWriter.Write(body)
}

type flushResponseWriter struct {
	*responseWriter
	flusher http.Flusher
}

func (w *flushResponseWriter) Flush() {
	if !w.headerWritten {
		w.WriteHeader(http.StatusOK)
	}
	w.flusher.Flush()
}

func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
