package latency

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"ai-speed/internal/netutil"
)

// Target defines a latency test endpoint.
type Target struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Category string `json:"category"` // "baseline" | "ai" | "media"
}

// Sample captures timing breakdown for a single HTTP probe.
type Sample struct {
	Seq        int    `json:"seq"`
	OK         bool   `json:"ok"`
	StatusCode int    `json:"statusCode,omitempty"`
	ConnectMs  int64  `json:"connectMs"`
	TLSMs      int64  `json:"tlsMs"`
	TTFBMs     int64  `json:"ttfbMs"`
	Error      string `json:"error,omitempty"`
}

// TargetResult summarizes latency, jitter, and packet/request loss across samples.
type TargetResult struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	Category     string   `json:"category"`
	AvgMs        int64    `json:"avgMs"`
	MinMs        int64    `json:"minMs"`
	MaxMs        int64    `json:"maxMs"`
	AvgConnectMs int64    `json:"avgConnectMs"`
	AvgTLSMs     int64    `json:"avgTlsMs"`
	JitterMs     int64    `json:"jitterMs"`
	SuccessCount int      `json:"successCount"`
	TotalCount   int      `json:"totalCount"`
	LossPercent  float64  `json:"lossPercent"`
	Rating       string   `json:"rating"` // "excellent" | "good" | "fair" | "poor" | "failed"
	Samples      []Sample `json:"samples"`
	LastError    string   `json:"lastError,omitempty"`
	CheckedAt    string   `json:"checkedAt"`
}

// DefaultTargets returns the 5 canonical latency targets.
func DefaultTargets() []Target {
	return []Target{
		{
			ID:       "baseline",
			Name:     "基准连通 (Google 204)",
			URL:      "https://www.gstatic.com/generate_204",
			Category: "baseline",
		},
		{
			ID:       "chatgpt",
			Name:     "ChatGPT / OpenAI",
			URL:      "https://api.openai.com/v1/models",
			Category: "ai",
		},
		{
			ID:       "gemini",
			Name:     "Google Gemini",
			URL:      "https://gemini.google.com/",
			Category: "ai",
		},
		{
			ID:       "antigravity",
			Name:     "Antigravity (反重力)",
			URL:      "https://cloudcode-pa.googleapis.com/",
			Category: "ai",
		},
		{
			ID:       "youtube",
			Name:     "YouTube",
			URL:      "https://www.youtube.com/generate_204",
			Category: "media",
		},
	}
}

// TestAll measures latency across all default targets concurrently.
func TestAll(ctx context.Context, cfg netutil.ProxyConfig, samples int) ([]TargetResult, error) {
	return TestTargetsWith(ctx, cfg, DefaultTargets(), samples)
}

// TestOne measures latency for a single target ID from DefaultTargets.
func TestOne(ctx context.Context, cfg netutil.ProxyConfig, targetID string, samples int) (TargetResult, error) {
	for _, t := range DefaultTargets() {
		if t.ID == targetID {
			res, err := TestTargetsWith(ctx, cfg, []Target{t}, samples)
			if err != nil {
				return TargetResult{}, err
			}
			return res[0], nil
		}
	}
	return TargetResult{}, fmt.Errorf("未知的延迟测试目标 %q", targetID)
}

// TestTargetsWith measures latency against the supplied target list.
func TestTargetsWith(ctx context.Context, cfg netutil.ProxyConfig, targets []Target, samples int) ([]TargetResult, error) {
	if samples <= 0 {
		samples = 3
	}
	if samples > 10 {
		samples = 10
	}
	// Disable keep-alive so every sample measures a genuine proxy handshake & TTFB.
	client, err := netutil.NewHTTPClient(cfg, 8*time.Second, true)
	if err != nil {
		return nil, err
	}

	results := make([]TargetResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(idx int, target Target) {
			defer wg.Done()
			results[idx] = measureTarget(ctx, client, target, samples)
		}(i, t)
	}
	wg.Wait()
	return results, nil
}

func measureTarget(ctx context.Context, client *http.Client, target Target, count int) TargetResult {
	res := TargetResult{
		ID:         target.ID,
		Name:       target.Name,
		URL:        target.URL,
		Category:   target.Category,
		TotalCount: count,
		Samples:    make([]Sample, 0, count),
		CheckedAt:  netutil.FormatNow(),
	}

	var ttfbs []int64
	var sumConnect, sumTLS int64

	for seq := 1; seq <= count; seq++ {
		if seq > 1 {
			select {
			case <-ctx.Done():
				break
			case <-time.After(120 * time.Millisecond):
			}
		}
		if ctx.Err() != nil {
			res.Samples = append(res.Samples, Sample{
				Seq:   seq,
				OK:    false,
				Error: ctx.Err().Error(),
			})
			res.LastError = ctx.Err().Error()
			continue
		}

		sample := probeOnce(ctx, client, target.URL, seq)
		res.Samples = append(res.Samples, sample)
		if sample.OK {
			res.SuccessCount++
			ttfbs = append(ttfbs, sample.TTFBMs)
			sumConnect += sample.ConnectMs
			sumTLS += sample.TLSMs
		} else if sample.Error != "" {
			res.LastError = sample.Error
		}
	}

	if res.TotalCount > 0 {
		res.LossPercent = math.Round((float64(res.TotalCount-res.SuccessCount)/float64(res.TotalCount))*1000) / 10
	}

	if len(ttfbs) == 0 {
		res.Rating = "failed"
		return res
	}

	minVal := ttfbs[0]
	maxVal := ttfbs[0]
	var sumTTFB int64
	for _, v := range ttfbs {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
		sumTTFB += v
	}
	n := int64(len(ttfbs))
	res.AvgMs = sumTTFB / n
	res.MinMs = minVal
	res.MaxMs = maxVal
	res.AvgConnectMs = sumConnect / n
	res.AvgTLSMs = sumTLS / n

	if len(ttfbs) >= 2 {
		var diffSum int64
		for i := 1; i < len(ttfbs); i++ {
			d := ttfbs[i] - ttfbs[i-1]
			if d < 0 {
				d = -d
			}
			diffSum += d
		}
		res.JitterMs = diffSum / int64(len(ttfbs)-1)
	}

	res.Rating = classifyLatency(res.AvgMs, res.LossPercent)
	return res
}

func probeOnce(ctx context.Context, client *http.Client, targetURL string, seq int) Sample {
	reqCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()

	var (
		start        = time.Now()
		connectStart time.Time
		connectDone  time.Duration
		tlsStart     time.Time
		tlsDone      time.Duration
		ttfb         time.Duration
	)

	trace := &httptrace.ClientTrace{
		ConnectStart: func(_, _ string) {
			connectStart = time.Now()
		},
		ConnectDone: func(_, _ string, _ error) {
			if !connectStart.IsZero() {
				connectDone = time.Since(connectStart)
			}
		},
		TLSHandshakeStart: func() {
			tlsStart = time.Now()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			if !tlsStart.IsZero() {
				tlsDone = time.Since(tlsStart)
			}
		},
		GotFirstResponseByte: func() {
			ttfb = time.Since(start)
		},
	}

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(reqCtx, trace), http.MethodGet, targetURL, nil)
	if err != nil {
		return Sample{Seq: seq, OK: false, Error: err.Error()}
	}
	netutil.ApplyBrowserHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return Sample{Seq: seq, OK: false, Error: err.Error()}
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	resp.Body.Close()

	if ttfb <= 0 {
		ttfb = time.Since(start)
	}
	return Sample{
		Seq:        seq,
		OK:         true,
		StatusCode: resp.StatusCode,
		ConnectMs:  max(1, connectDone.Milliseconds()),
		TLSMs:      tlsDone.Milliseconds(),
		TTFBMs:     max(1, ttfb.Milliseconds()),
	}
}

func classifyLatency(avgMs int64, lossPct float64) string {
	if lossPct >= 100 || avgMs <= 0 {
		return "failed"
	}
	if lossPct > 35 || avgMs >= 650 {
		return "poor"
	}
	if avgMs < 180 && lossPct == 0 {
		return "excellent"
	}
	if avgMs < 380 && lossPct == 0 {
		return "good"
	}
	return "fair"
}
