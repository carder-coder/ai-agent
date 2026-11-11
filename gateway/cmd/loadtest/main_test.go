package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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
