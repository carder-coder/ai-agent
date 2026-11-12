// 本文件验证压测器的并发控制、错误统计和延迟分位数计算。
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunLoadTestHonorsConcurrencyAndCountsErrors(t *testing.T) {
	var active atomic.Int32
	var peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for observed := peak.Load(); current > observed && !peak.CompareAndSwap(observed, current); observed = peak.Load() {
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
		if current%3 == 0 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	results := runLoadTest(context.Background(), server.Client(), server.URL, 12, 3)
	failures := 0
	for _, result := range results {
		if result.err != nil {
			failures++
		}
		if result.latency <= 0 {
			t.Fatalf("request latency = %s", result.latency)
		}
	}
	if len(results) != 12 || peak.Load() > 3 || peak.Load() < 2 || failures == 0 {
		t.Fatalf("requests=%d peak concurrency=%d failures=%d", len(results), peak.Load(), failures)
	}
}

func TestPercentileMilliseconds(t *testing.T) {
	latencies := []time.Duration{5 * time.Millisecond, 1 * time.Millisecond, 4 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}
	if got := percentileMilliseconds(latencies, 0.95); got != 5 {
		t.Fatalf("P95 = %v ms, want 5", got)
	}
	if got := percentileMilliseconds(nil, 0.95); got != 0 {
		t.Fatalf("empty percentile = %v ms, want 0", got)
	}
}

func TestRunLoadTestForRunsWithBoundedConcurrencyAndStatistics(t *testing.T) {
	var active atomic.Int32
	var peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for observed := peak.Load(); current > observed && !peak.CompareAndSwap(observed, current); observed = peak.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	stats := runLoadTestFor(context.Background(), server.Client(), server.URL, http.MethodGet, 120*time.Millisecond, 4)
	if stats.requests == 0 || stats.successes == 0 || stats.requests != stats.successes+stats.failures+stats.cancelled {
		t.Fatalf("unexpected duration stats: requests=%d success=%d errors=%d cancelled=%d",
			stats.requests, stats.successes, stats.failures, stats.cancelled)
	}
	if peak.Load() < 2 || peak.Load() > 4 {
		t.Fatalf("observed concurrency = %d, want 2-4", peak.Load())
	}
	if len(stats.latencies) != maxLatencyBucket+1 {
		t.Fatalf("latency bucket count = %d, want fixed bounded size %d", len(stats.latencies), maxLatencyBucket+1)
	}
	if stats.percentileMilliseconds(0.95) <= 0 {
		t.Fatalf("P95 = %v ms, want a measured latency", stats.percentileMilliseconds(0.95))
	}
}

func TestRunLoadTestWithMethodSupportsHealthChecks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	results := runLoadTestWithMethod(context.Background(), server.Client(), server.URL, 3, 2, http.MethodGet)
	for _, result := range results {
		if result.err != nil {
			t.Fatalf("GET request failed: %v", result.err)
		}
	}
}

func TestValidateDurationLoadIsBoundedToLoopbackAndScheduleLimits(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		duration    time.Duration
		concurrency int
		wantErr     bool
	}{
		{name: "localhost", target: "http://localhost:8080/healthz", duration: time.Minute, concurrency: 200},
		{name: "ipv4 loopback", target: "http://127.0.0.1:8080/healthz", duration: 30 * time.Minute, concurrency: 200},
		{name: "ipv6 loopback", target: "http://[::1]:8080/healthz", duration: time.Second, concurrency: 1},
		{name: "external host", target: "https://example.com/healthz", duration: time.Second, concurrency: 1, wantErr: true},
		{name: "duration over cap", target: "http://localhost/healthz", duration: 31 * time.Minute, concurrency: 1, wantErr: true},
		{name: "concurrency over cap", target: "http://localhost/healthz", duration: time.Minute, concurrency: 201, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, err := url.Parse(test.target)
			if err != nil {
				t.Fatal(err)
			}
			err = validateDurationLoad(target, test.duration, test.concurrency)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateDurationLoad() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
