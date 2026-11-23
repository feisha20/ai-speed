package latency

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ai-speed/internal/netutil"
)

func TestTargetsWithMockServers(t *testing.T) {
	var hits atomic.Int32
	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(15 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fastSrv.Close()

	flakyCount := atomic.Int32{}
	flakySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := flakyCount.Add(1)
		if n == 2 {
			// Force immediate connection close to simulate packet/socket drop
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer flakySrv.Close()

	// Closed port for deterministic failure
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close()

	targets := []Target{
		{ID: "fast", Name: "Fast", URL: fastSrv.URL, Category: "baseline"},
		{ID: "flaky", Name: "Flaky", URL: flakySrv.URL, Category: "ai"},
		{ID: "dead", Name: "Dead", URL: deadURL, Category: "media"},
	}

	results, err := TestTargetsWith(context.Background(), netutil.ProxyConfig{Mode: "direct"}, targets, 3)
	if err != nil {
		t.Fatalf("TestTargetsWith failed: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	fast := results[0]
	if fast.SuccessCount != 3 || fast.LossPercent != 0 {
		t.Fatalf("expected fast 3/3 success, got %d/%d (loss %.1f%%)", fast.SuccessCount, fast.TotalCount, fast.LossPercent)
	}
	if fast.AvgMs < 10 || fast.MinMs > fast.AvgMs || fast.MaxMs < fast.AvgMs {
		t.Fatalf("unexpected latency stats: %+v", fast)
	}
	if fast.Rating != "excellent" {
		t.Fatalf("expected excellent rating, got %s", fast.Rating)
	}

	flaky := results[1]
	if flaky.SuccessCount != 2 || flaky.LossPercent == 0 {
		t.Fatalf("expected flaky 2/3 success with >0 loss, got %d/%d (loss %.1f%%)", flaky.SuccessCount, flaky.TotalCount, flaky.LossPercent)
	}

	dead := results[2]
	if dead.SuccessCount != 0 || dead.Rating != "failed" || dead.LossPercent != 100 {
		t.Fatalf("expected dead failed with 100%% loss, got %+v", dead)
	}
}

func TestOneInvalidTarget(t *testing.T) {
	_, err := TestOne(context.Background(), netutil.ProxyConfig{Mode: "direct"}, "nonexistent", 1)
	if err == nil {
		t.Fatal("expected error for unknown target ID")
	}
}
