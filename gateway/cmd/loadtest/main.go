// 本文件实现可配置并发压测命令，统计请求延迟分位数和错误率。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// sample 保存单个请求的耗时、错误及是否因压测截止而取消。
type sample struct {
	latency   time.Duration
	err       error
	cancelled bool
}

// loadStats 汇总持续压测计数和有界延迟直方图。
type loadStats struct {
	requests  int64
	successes int64
	failures  int64
	cancelled int64
	latencies []uint64
}

// 直方图以 0.1 毫秒为桶、在 60 秒封顶，避免持续压测按样本数增长内存。
const (
	maxLoadConcurrency = 2000
	// 持续压测仅面向本机，并限定在第 5 周的 200 并发、30 分钟验收范围。
	maxDurationConcurrency  = 200
	maxLoadDuration         = 30 * time.Minute
	durationHistogramBucket = 100 * time.Microsecond
	maxLatencyBucket        = 600_000
)

// main 解析固定请求数或持续时长压测参数，并输出延迟分位数及错误率。
func main() {
	requests := flag.Int("n", 200, "total number of requests")
	concurrency := flag.Int("c", 50, "maximum concurrent requests")
	target := flag.String("url", "http://127.0.0.1:8080/v1/chat/completions", "chat completions endpoint")
	method := flag.String("method", http.MethodPost, "HTTP method: GET or POST")
	timeout := flag.Duration("timeout", 30*time.Second, "per-request timeout")
	duration := flag.Duration("duration", 0, "fixed load-test duration; 0 uses --n requests")
	flag.Parse()

	parsed, err := url.ParseRequestURI(*target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		fmt.Fprintln(os.Stderr, "--url must be an absolute HTTP or HTTPS URL")
		os.Exit(2)
	}
	normalizedMethod := strings.ToUpper(*method)
	if normalizedMethod != http.MethodGet && normalizedMethod != http.MethodPost {
		fmt.Fprintln(os.Stderr, "--method must be GET or POST")
		os.Exit(2)
	}
	if *requests < 1 || *concurrency < 1 || *concurrency > maxLoadConcurrency ||
		*timeout <= 0 || *duration < 0 {
		fmt.Fprintf(os.Stderr, "--n and --timeout must be positive; --c must be 1-%d; --duration must not be negative\n", maxLoadConcurrency)
		os.Exit(2)
	}
	if *duration > 0 {
		if err := validateDurationLoad(parsed, *duration, *concurrency); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}

	transport := &http.Transport{
		MaxConnsPerHost:     *concurrency,
		MaxIdleConns:        *concurrency,
		MaxIdleConnsPerHost: *concurrency,
	}
	client := &http.Client{Timeout: *timeout, Transport: transport}
	if *duration > 0 {
		started := time.Now()
		stats := runLoadTestFor(context.Background(), client, *target, normalizedMethod, *duration, *concurrency)
		elapsed := time.Since(started)
		completed := stats.successes + stats.failures
		errorRate := 0.0
		if completed > 0 {
			errorRate = float64(stats.failures) / float64(completed) * 100
		}
		fmt.Printf(
			"duration=%s requests=%d concurrency=%d success=%d errors=%d cancelled=%d error_rate=%.2f%% rps=%.2f P50=%.2fms P95=%.2fms P99=%.2fms\n",
			*duration, stats.requests, *concurrency, stats.successes, stats.failures, stats.cancelled, errorRate,
			float64(stats.requests)/elapsed.Seconds(),
			stats.percentileMilliseconds(0.50),
			stats.percentileMilliseconds(0.95),
			stats.percentileMilliseconds(0.99),
		)
		if stats.failures > 0 {
			os.Exit(1)
		}
		return
	}

	effectiveConcurrency := *concurrency
	if effectiveConcurrency > *requests {
		effectiveConcurrency = *requests
	}
	results := runLoadTestWithMethod(context.Background(), client, *target, *requests, effectiveConcurrency, normalizedMethod)
	successes := 0
	latencies := make([]time.Duration, 0, len(results))
	for _, result := range results {
		latencies = append(latencies, result.latency)
		if result.err == nil {
			successes++
		}
	}
	failures := len(results) - successes
	errorRate := float64(failures) / float64(len(results)) * 100
	fmt.Printf(
		"requests=%d concurrency=%d success=%d errors=%d error_rate=%.2f%% P50=%.2fms P95=%.2fms P99=%.2fms\n",
		len(results), effectiveConcurrency, successes, failures, errorRate,
		percentileMilliseconds(latencies, 0.50),
		percentileMilliseconds(latencies, 0.95),
		percentileMilliseconds(latencies, 0.99),
	)
	if failures > 0 {
		os.Exit(1)
	}
}

// validateDurationLoad 将持续压测限制在本机目标、30 分钟内和最多 200 并发。
func validateDurationLoad(target *url.URL, duration time.Duration, concurrency int) error {
	if duration > maxLoadDuration {
		return fmt.Errorf("--duration must not exceed %s", maxLoadDuration)
	}
	if concurrency > maxDurationConcurrency {
		return fmt.Errorf("duration mode concurrency must not exceed %d", maxDurationConcurrency)
	}
	if !isLoopbackTarget(target) {
		return fmt.Errorf("duration mode only accepts localhost or loopback IP targets")
	}
	return nil
}

// isLoopbackTarget 只接受明确的 localhost 或 loopback IP，不进行 DNS 解析。
func isLoopbackTarget(target *url.URL) bool {
	host := target.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// runLoadTest 使用固定数量的 worker 执行指定请求数并收集每次结果。
func runLoadTest(ctx context.Context, client *http.Client, target string, requests, concurrency int) []sample {
	return runLoadTestWithMethod(ctx, client, target, requests, concurrency, http.MethodPost)
}

// runLoadTestWithMethod 用固定数量 worker 按指定 HTTP 方法执行有限请求。
func runLoadTestWithMethod(
	ctx context.Context,
	client *http.Client,
	target string,
	requests, concurrency int,
	method string,
) []sample {
	if concurrency > requests {
		concurrency = requests
	}
	// 固定 worker 数限制同时在途请求，缓冲结果通道则避免 worker 被汇总协程阻塞。
	jobs := make(chan struct{})
	results := make(chan sample, requests)
	for worker := 0; worker < concurrency; worker++ {
		go func() {
			for range jobs {
				results <- sendRequestWithMethod(ctx, client, target, method)
			}
		}()
	}
	for request := 0; request < requests; request++ {
		jobs <- struct{}{}
	}
	close(jobs)

	samples := make([]sample, 0, requests)
	for request := 0; request < requests; request++ {
		samples = append(samples, <-results)
	}
	return samples
}

// runLoadTestFor 用固定数量 worker 持续压测，并以固定直方图控制统计内存。
func runLoadTestFor(
	parent context.Context,
	client *http.Client,
	target, method string,
	duration time.Duration,
	concurrency int,
) loadStats {
	ctx, cancel := context.WithTimeout(parent, duration)
	defer cancel()
	results := make(chan sample, concurrency)
	var workers sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					results <- sendRequestWithMethod(ctx, client, target, method)
				}
			}
		}()
	}
	go func() {
		workers.Wait()
		close(results)
	}()

	stats := loadStats{latencies: make([]uint64, maxLatencyBucket+1)}
	for result := range results {
		stats.observe(result)
	}
	return stats
}

// sendRequest 执行一次聊天请求并记录完整响应耗时及非成功结果。
func sendRequest(ctx context.Context, client *http.Client, target string) sample {
	return sendRequestWithMethod(ctx, client, target, http.MethodPost)
}

// sendRequestWithMethod 发送一次 GET 或 POST 请求并校验响应状态及读取结果。
func sendRequestWithMethod(ctx context.Context, client *http.Client, target, method string) sample {
	started := time.Now()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"model":"loadtest","messages":[{"role":"user","content":"load test"}]}`)
	}
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		target,
		body,
	)
	if err != nil {
		return sample{latency: time.Since(started), err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return sample{latency: time.Since(started), err: err, cancelled: ctx.Err() != nil}
	}
	_, copyErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		err = fmt.Errorf("HTTP %d", response.StatusCode)
	} else if copyErr != nil {
		err = copyErr
	} else if closeErr != nil {
		err = closeErr
	}
	return sample{latency: time.Since(started), err: err}
}

// observe 累加单次结果；延迟直方图固定大小，不随请求总数增长。
func (s *loadStats) observe(result sample) {
	s.requests++
	if result.cancelled {
		s.cancelled++
		return
	}
	if result.err != nil {
		s.failures++
	} else {
		s.successes++
	}
	bucket := maxLatencyBucket
	if result.latency < time.Duration(maxLatencyBucket)*durationHistogramBucket {
		bucket = int(result.latency / durationHistogramBucket)
	}
	s.latencies[bucket]++
}

// percentileMilliseconds 根据已完成请求的直方图估算 nearest-rank 延迟分位数。
func (s loadStats) percentileMilliseconds(percentile float64) float64 {
	completed := s.successes + s.failures
	if completed == 0 {
		return 0
	}
	target := int64(float64(completed)*percentile + 0.999999)
	var count int64
	for bucket, samples := range s.latencies {
		count += int64(samples)
		if count >= target {
			return float64(bucket) * float64(durationHistogramBucket) / float64(time.Millisecond)
		}
	}
	return float64(maxLatencyBucket) * float64(durationHistogramBucket) / float64(time.Millisecond)
}

// percentileMilliseconds 按 nearest-rank 计算耗时分位数并转换为毫秒。
func percentileMilliseconds(values []time.Duration, percentile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	// 使用 nearest-rank 口径，确保小样本的高分位数不会落在未观测值之间。
	index := int(float64(len(sorted))*percentile+0.999999) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return float64(sorted[index]) / float64(time.Millisecond)
}
