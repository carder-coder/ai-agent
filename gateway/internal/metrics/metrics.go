// 本文件采集网关请求指标，并以 Prometheus 文本格式提供 /metrics。
package metrics

import (
	"fmt"
	"math"
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

type modelStats struct {
	requests      uint64
	errors        uint64
	inputTokens   uint64
	outputTokens  uint64
	cachedTokens  uint64
	costUnits     uint64
	requestTiming histogram
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
	models           map[string]*modelStats
	cacheLookups     uint64
	cacheHits        uint64
	cacheCorrect     uint64
	cacheIncorrect   uint64
}

// New 创建并初始化一组网关指标。
func New() *Metrics {
	now := time.Now()
	return &Metrics{
		startedAt:        now,
		requestDuration:  histogram{buckets: make([]uint64, len(histogramBounds))},
		firstByteLatency: histogram{buckets: make([]uint64, len(histogramBounds))},
		models:           make(map[string]*modelStats),
	}
}

// ObserveModel 记录模型级 latency、usage、cost 与成功/失败状态。
func (m *Metrics) ObserveModel(
	model string,
	duration time.Duration,
	inputTokens, outputTokens, cachedTokens int,
	costYuan float64,
	success bool,
) {
	if model == "" {
		model = "unknown"
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	stats, exists := m.models[model]
	if !exists {
		// 模型名可能来自请求体；限制标签基数，避免任意输入无限扩张指标内存。
		if len(m.models) >= 64 {
			model = "_other"
			stats = m.models[model]
		}
		if stats == nil {
			stats = &modelStats{requestTiming: histogram{buckets: make([]uint64, len(histogramBounds))}}
			m.models[model] = stats
		}
	}
	stats.requests++
	if !success {
		stats.errors++
	}
	stats.inputTokens += uint64(max(inputTokens, 0))
	stats.outputTokens += uint64(max(outputTokens, 0))
	stats.cachedTokens += uint64(max(cachedTokens, 0))
	if costYuan > 0 && !math.IsNaN(costYuan) && !math.IsInf(costYuan, 0) {
		stats.costUnits += uint64(math.Round(costYuan * 10_000))
	}
	stats.requestTiming.observe(duration.Seconds())
}

// ObserveCacheLookup 记录可缓存聊天请求及其中的语义命中数。
func (m *Metrics) ObserveCacheLookup(hit bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheLookups++
	if hit {
		m.cacheHits++
	}
}

// ObserveCacheFeedback 累计用户对语义缓存答案的正误反馈。
func (m *Metrics) ObserveCacheFeedback(correct bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if correct {
		m.cacheCorrect++
	} else {
		m.cacheIncorrect++
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
	_, _ = fmt.Fprintln(w, "# HELP gateway_semantic_cache_lookups_total Eligible semantic cache lookups.")
	_, _ = fmt.Fprintln(w, "# TYPE gateway_semantic_cache_lookups_total counter")
	_, _ = fmt.Fprintf(w, "gateway_semantic_cache_lookups_total %d\n", m.cacheLookups)
	_, _ = fmt.Fprintln(w, "# HELP gateway_semantic_cache_hits_total Semantic cache hits.")
	_, _ = fmt.Fprintln(w, "# TYPE gateway_semantic_cache_hits_total counter")
	_, _ = fmt.Fprintf(w, "gateway_semantic_cache_hits_total %d\n", m.cacheHits)
	_, _ = fmt.Fprintln(w, "# HELP gateway_semantic_cache_feedback_total User feedback on semantic cache answers.")
	_, _ = fmt.Fprintln(w, "# TYPE gateway_semantic_cache_feedback_total counter")
	_, _ = fmt.Fprintf(w, "gateway_semantic_cache_feedback_total{result=\"correct\"} %d\n", m.cacheCorrect)
	_, _ = fmt.Fprintf(w, "gateway_semantic_cache_feedback_total{result=\"incorrect\"} %d\n", m.cacheIncorrect)
	hitRatio := float64(0)
	if m.cacheLookups > 0 {
		hitRatio = float64(m.cacheHits) / float64(m.cacheLookups)
	}
	feedbackCount := m.cacheCorrect + m.cacheIncorrect
	incorrectRatio := float64(0)
	if feedbackCount > 0 {
		incorrectRatio = float64(m.cacheIncorrect) / float64(feedbackCount)
	}
	_, _ = fmt.Fprintln(w, "# HELP gateway_semantic_cache_hit_ratio Cache hits divided by eligible lookups.")
	_, _ = fmt.Fprintln(w, "# TYPE gateway_semantic_cache_hit_ratio gauge")
	_, _ = fmt.Fprintf(w, "gateway_semantic_cache_hit_ratio %s\n", strconv.FormatFloat(hitRatio, 'f', 6, 64))
	_, _ = fmt.Fprintln(w, "# HELP gateway_semantic_cache_incorrect_feedback_ratio Incorrect feedback divided by all feedback.")
	_, _ = fmt.Fprintln(w, "# TYPE gateway_semantic_cache_incorrect_feedback_ratio gauge")
	_, _ = fmt.Fprintf(w, "gateway_semantic_cache_incorrect_feedback_ratio %s\n",
		strconv.FormatFloat(incorrectRatio, 'f', 6, 64))
	m.writeModelMetrics(w)
}

func (m *Metrics) writeModelMetrics(w http.ResponseWriter) {
	_, _ = fmt.Fprintln(w, "# HELP gateway_model_requests_total Completed requests by model.")
	_, _ = fmt.Fprintln(w, "# TYPE gateway_model_requests_total counter")
	_, _ = fmt.Fprintln(w, "# HELP gateway_model_errors_total Failed requests by model.")
	_, _ = fmt.Fprintln(w, "# TYPE gateway_model_errors_total counter")
	_, _ = fmt.Fprintln(w, "# HELP gateway_model_input_tokens_total Input tokens by model.")
	_, _ = fmt.Fprintln(w, "# TYPE gateway_model_input_tokens_total counter")
	_, _ = fmt.Fprintln(w, "# HELP gateway_model_output_tokens_total Output tokens by model.")
	_, _ = fmt.Fprintln(w, "# TYPE gateway_model_output_tokens_total counter")
	_, _ = fmt.Fprintln(w, "# HELP gateway_model_cached_input_tokens_total Cached input tokens by model.")
	_, _ = fmt.Fprintln(w, "# TYPE gateway_model_cached_input_tokens_total counter")
	_, _ = fmt.Fprintln(w, "# HELP gateway_model_cost_yuan_total Estimated model cost in yuan.")
	_, _ = fmt.Fprintln(w, "# TYPE gateway_model_cost_yuan_total counter")
	for model, stats := range m.models {
		label := strconv.Quote(model)
		_, _ = fmt.Fprintf(w, "gateway_model_requests_total{model=%s} %d\n", label, stats.requests)
		_, _ = fmt.Fprintf(w, "gateway_model_errors_total{model=%s} %d\n", label, stats.errors)
		_, _ = fmt.Fprintf(w, "gateway_model_input_tokens_total{model=%s} %d\n", label, stats.inputTokens)
		_, _ = fmt.Fprintf(w, "gateway_model_output_tokens_total{model=%s} %d\n", label, stats.outputTokens)
		_, _ = fmt.Fprintf(w, "gateway_model_cached_input_tokens_total{model=%s} %d\n", label, stats.cachedTokens)
		_, _ = fmt.Fprintf(w, "gateway_model_cost_yuan_total{model=%s} %s\n",
			label, strconv.FormatFloat(float64(stats.costUnits)/10_000, 'f', 4, 64))
		writeLabeledHistogram(w, "gateway_model_request_duration_seconds",
			"Model request duration in seconds.", model, stats.requestTiming)
	}
}

func writeLabeledHistogram(w http.ResponseWriter, name, help, model string, h histogram) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	label := strconv.Quote(model)
	for i, bound := range histogramBounds {
		_, _ = fmt.Fprintf(w, "%s_bucket{model=%s,le=%q} %d\n",
			name, label, strconv.FormatFloat(bound, 'f', -1, 64), h.buckets[i])
	}
	_, _ = fmt.Fprintf(w, "%s_bucket{model=%s,le=\"+Inf\"} %d\n", name, label, h.count)
	_, _ = fmt.Fprintf(w, "%s_sum{model=%s} %s\n%s_count{model=%s} %d\n",
		name, label, strconv.FormatFloat(h.sum, 'f', 6, 64), name, label, h.count)
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
