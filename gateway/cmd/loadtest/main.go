package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type sample struct {
	latency time.Duration
	err     error
}

func main() {
	requests := flag.Int("n", 200, "total number of requests")
	concurrency := flag.Int("c", 50, "maximum concurrent requests")
	target := flag.String("url", "http://127.0.0.1:8080/v1/chat/completions", "chat completions endpoint")
	timeout := flag.Duration("timeout", 30*time.Second, "per-request timeout")
	flag.Parse()

	parsed, err := url.ParseRequestURI(*target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		fmt.Fprintln(os.Stderr, "--url must be an absolute HTTP or HTTPS URL")
		os.Exit(2)
	}
	if *requests < 1 || *concurrency < 1 || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "--n, --c, and --timeout must be positive")
		os.Exit(2)
	}

	client := &http.Client{Timeout: *timeout}
	results := runLoadTest(context.Background(), client, *target, *requests, *concurrency)
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
		len(results), *concurrency, successes, failures, errorRate,
		percentileMilliseconds(latencies, 0.50),
		percentileMilliseconds(latencies, 0.95),
		percentileMilliseconds(latencies, 0.99),
	)
	if failures > 0 {
		os.Exit(1)
	}
}

func runLoadTest(ctx context.Context, client *http.Client, target string, requests, concurrency int) []sample {
	if concurrency > requests {
		concurrency = requests
	}
	jobs := make(chan struct{})
	results := make(chan sample, requests)
	for worker := 0; worker < concurrency; worker++ {
		go func() {
			for range jobs {
				results <- sendRequest(ctx, client, target)
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

func sendRequest(ctx context.Context, client *http.Client, target string) sample {
	started := time.Now()
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		target,
		strings.NewReader(`{"model":"loadtest","messages":[{"role":"user","content":"load test"}]}`),
	)
	if err != nil {
		return sample{latency: time.Since(started), err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return sample{latency: time.Since(started), err: err}
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

func percentileMilliseconds(values []time.Duration, percentile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(float64(len(sorted))*percentile+0.999999) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return float64(sorted[index]) / float64(time.Millisecond)
}
