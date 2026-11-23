package speed

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ai-speed/internal/netutil"
	"ai-speed/internal/unlock"
)

// Options configures a bandwidth and AI target speed test run.
type Options struct {
	Proxy        netutil.ProxyConfig `json:"proxy"`
	DownloadMB   int                 `json:"downloadMb"`
	UploadMB     int                 `json:"uploadMb"`
	TimeoutSec   int                 `json:"timeoutSec"`
	OpenAIAPIKey string              `json:"openaiApiKey,omitempty"`
	OpenAIModel  string              `json:"openaiModel,omitempty"`
	GeminiAPIKey string              `json:"geminiApiKey,omitempty"`
	GeminiModel  string              `json:"geminiModel,omitempty"`
}

// ProgressEvent is streamed over SSE (or handled in CLI) as the speedtest advances.
type ProgressEvent struct {
	Type        string  `json:"type"` // "stage" | "sample" | "link_done" | "target_done" | "done" | "error"
	Stage       string  `json:"stage,omitempty"`
	LinkID      string  `json:"linkId,omitempty"`
	Label       string  `json:"label,omitempty"`
	CurrentMbps float64 `json:"currentMbps,omitempty"`
	AvgMbps     float64 `json:"avgMbps,omitempty"`
	PeakMbps    float64 `json:"peakMbps,omitempty"`
	Downloaded  int64   `json:"downloaded,omitempty"`
	TotalBytes  int64   `json:"totalBytes,omitempty"`
	ElapsedMs   int64   `json:"elapsedMs,omitempty"`
	Message     string  `json:"message,omitempty"`
	Report      *Report `json:"report,omitempty"`
}

// LinkResult holds real measured throughput for a backbone or edge link.
type LinkResult struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	SourceURL  string    `json:"sourceUrl"`
	OK         bool      `json:"ok"`
	AvgMbps    float64   `json:"avgMbps"`
	PeakMbps   float64   `json:"peakMbps"`
	TTFBMs     int64     `json:"ttfbMs"`
	Bytes      int64     `json:"bytes"`
	DurationMs int64     `json:"durationMs"`
	Samples    []float64 `json:"samples"`
	IsFallback bool      `json:"isFallback,omitempty"`
	Note       string    `json:"note,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// TokenSpeedResult stores optional LLM streaming token generation throughput.
type TokenSpeedResult struct {
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	OK           bool    `json:"ok"`
	TTFTMs       int64   `json:"ttftMs"`
	TokensPerSec float64 `json:"tokensPerSec"`
	TotalMs      int64   `json:"totalMs"`
	OutputChars  int     `json:"outputChars"`
	Error        string  `json:"error,omitempty"`
}

// TargetSpeedSummary translates link speed + target TTFB into experience metrics for the 4 core targets.
type TargetSpeedSummary struct {
	ID            string            `json:"id"` // "chatgpt" | "gemini" | "antigravity" | "youtube"
	Name          string            `json:"name"`
	DownloadMbps  float64           `json:"downloadMbps"`
	UploadMbps    float64           `json:"uploadMbps,omitempty"`
	TTFBMs        int64             `json:"ttfbMs"`
	JitterMs      int64             `json:"jitterMs"`
	BackboneLabel string            `json:"backboneLabel"`
	QualityTag    string            `json:"qualityTag"`
	TokenSpeed    *TokenSpeedResult `json:"tokenSpeed,omitempty"`
	GGCNode       string            `json:"ggcNode,omitempty"`
	Note          string            `json:"note,omitempty"`
}

// Report aggregates all link and target speed results.
type Report struct {
	DownloadMB int                  `json:"downloadMb"`
	UploadMB   int                  `json:"uploadMb"`
	Links      []LinkResult         `json:"links"`
	Targets    []TargetSpeedSummary `json:"targets"`
	CheckedAt  string               `json:"checkedAt"`
}

// Mirror describes a named download speed mirror.
type Mirror struct {
	Name string
	URL  string
}

// Endpoints configures all external speedtest & probe URLs (overridable for testing).
type Endpoints struct {
	GoogleCDNURL      string
	YouTubeGGCURL     string
	CloudflareDownURL string // expects %d byte count or plain URL
	CloudflareUpURL   string
	FallbackMirrors   []Mirror
	OpenAIChatURL     string
	GeminiStreamURL   string // expects %s model name
	TargetProbeURLs   map[string]string
	MinValidBytes     int64
}

// DefaultEndpoints returns production speedtest endpoints.
func DefaultEndpoints() Endpoints {
	return Endpoints{
		GoogleCDNURL:      "https://dl.google.com/chrome/mac/universal/stable/GGRO/googlechrome.dmg",
		YouTubeGGCURL:     "https://redirector.googlevideo.com/report_mapping",
		CloudflareDownURL: "https://speed.cloudflare.com/__down?bytes=%d",
		CloudflareUpURL:   "https://speed.cloudflare.com/__up",
		FallbackMirrors: []Mirror{
			{Name: "Hetzner 新加坡链路", URL: "https://sin-speed.hetzner.com/100MB.bin"},
			{Name: "Hetzner 美东链路", URL: "https://ash-speed.hetzner.com/100MB.bin"},
			{Name: "OVH 国际测试链路", URL: "https://proof.ovh.net/files/10Mb.dat"},
		},
		OpenAIChatURL:   "https://api.openai.com/v1/chat/completions",
		GeminiStreamURL: "https://generativelanguage.googleapis.com/v1beta/models/%s:streamGenerateContent?alt=sse",
		TargetProbeURLs: map[string]string{
			"chatgpt":     "https://api.openai.com/v1/models",
			"gemini":      "https://gemini.google.com/",
			"antigravity": "https://cloudcode-pa.googleapis.com/",
			"youtube":     "https://www.youtube.com/generate_204",
		},
		MinValidBytes: 16 * 1024,
	}
}

func normalizeOptions(opts Options) Options {
	if opts.DownloadMB <= 0 {
		opts.DownloadMB = 10
	}
	if opts.DownloadMB > 50 {
		opts.DownloadMB = 50
	}
	if opts.UploadMB < 0 {
		opts.UploadMB = 0
	}
	if opts.UploadMB > 10 {
		opts.UploadMB = 10
	}
	if opts.TimeoutSec <= 0 {
		opts.TimeoutSec = 15
	}
	if opts.TimeoutSec < 5 {
		opts.TimeoutSec = 5
	}
	if opts.TimeoutSec > 45 {
		opts.TimeoutSec = 45
	}
	if strings.TrimSpace(opts.OpenAIModel) == "" {
		opts.OpenAIModel = "gpt-4o-mini"
	}
	if strings.TrimSpace(opts.GeminiModel) == "" {
		opts.GeminiModel = "gemini-2.0-flash"
	}
	return opts
}

// Run executes the complete speedtest suite against production endpoints.
func Run(ctx context.Context, opts Options, onEvent func(ProgressEvent)) (*Report, error) {
	return RunWithEndpoints(ctx, opts, DefaultEndpoints(), onEvent)
}

// RunWithEndpoints executes the speedtest suite using supplied endpoints (used for tests too).
func RunWithEndpoints(ctx context.Context, opts Options, ep Endpoints, onEvent func(ProgressEvent)) (*Report, error) {
	opts = normalizeOptions(opts)
	emit := func(ev ProgressEvent) {
		if onEvent != nil {
			onEvent(ev)
		}
	}

	dlClient, err := netutil.NewHTTPClient(opts.Proxy, time.Duration(opts.TimeoutSec)*time.Second, false)
	if err != nil {
		return nil, err
	}
	probeClient, err := netutil.NewHTTPClient(opts.Proxy, 8*time.Second, true)
	if err != nil {
		return nil, err
	}

	maxDownBytes := int64(opts.DownloadMB) * 1024 * 1024
	var links []LinkResult

	// 1. Google CDN backbone speed (Gemini / Antigravity / YouTube)
	emit(ProgressEvent{
		Type:    "stage",
		Stage:   "google_cdn",
		LinkID:  "google_cdn",
		Label:   "Google 全球 CDN 下行测速（对应 Gemini / 反重力 / YouTube）",
		Message: fmt.Sprintf("正在拉取最多 %d MB 测试流...", opts.DownloadMB),
	})
	googleRes := measureDownload(ctx, dlClient, "google_cdn", "Google 全球 CDN", ep.GoogleCDNURL, true, maxDownBytes, ep.MinValidBytes, func(cur, avg, peak float64, dl, total, elapsed int64) {
		emit(ProgressEvent{
			Type:        "sample",
			Stage:       "google_cdn",
			LinkID:      "google_cdn",
			Label:       "Google 全球 CDN",
			CurrentMbps: cur,
			AvgMbps:     avg,
			PeakMbps:    peak,
			Downloaded:  dl,
			TotalBytes:  total,
			ElapsedMs:   elapsed,
		})
	})
	links = append(links, googleRes)
	emit(ProgressEvent{
		Type:        "link_done",
		Stage:       "google_cdn",
		LinkID:      "google_cdn",
		Label:       googleRes.Name,
		AvgMbps:     googleRes.AvgMbps,
		PeakMbps:    googleRes.PeakMbps,
		Downloaded:  googleRes.Bytes,
		TotalBytes:  maxDownBytes,
		ElapsedMs:   googleRes.DurationMs,
		Message:     googleRes.Error,
	})

	// 2. Cloudflare Edge download (ChatGPT/OpenAI edge) + honest fallback
	cfURL := ep.CloudflareDownURL
	if strings.Contains(cfURL, "%d") {
		cfURL = fmt.Sprintf(cfURL, maxDownBytes)
	}
	emit(ProgressEvent{
		Type:    "stage",
		Stage:   "cloudflare_edge",
		LinkID:  "cloudflare_edge",
		Label:   "Cloudflare 边缘下行测速（对应 ChatGPT / GPT）",
		Message: fmt.Sprintf("正在测试 Cloudflare 边缘节点（上限 %d MB）...", opts.DownloadMB),
	})
	cfRes := measureDownload(ctx, dlClient, "cloudflare_edge", "Cloudflare 边缘 (GPT)", cfURL, false, maxDownBytes, ep.MinValidBytes, func(cur, avg, peak float64, dl, total, elapsed int64) {
		emit(ProgressEvent{
			Type:        "sample",
			Stage:       "cloudflare_edge",
			LinkID:      "cloudflare_edge",
			Label:       "Cloudflare 边缘 (GPT)",
			CurrentMbps: cur,
			AvgMbps:     avg,
			PeakMbps:    peak,
			Downloaded:  dl,
			TotalBytes:  total,
			ElapsedMs:   elapsed,
		})
	})
	links = append(links, cfRes)
	emit(ProgressEvent{
		Type:       "link_done",
		Stage:      "cloudflare_edge",
		LinkID:     "cloudflare_edge",
		Label:      cfRes.Name,
		AvgMbps:    cfRes.AvgMbps,
		PeakMbps:   cfRes.PeakMbps,
		Downloaded: cfRes.Bytes,
		TotalBytes: maxDownBytes,
		ElapsedMs:  cfRes.DurationMs,
		Message:    cfRes.Error,
	})

	var fallbackRes *LinkResult
	if !cfRes.OK && ctx.Err() == nil && len(ep.FallbackMirrors) > 0 {
		emit(ProgressEvent{
			Type:    "stage",
			Stage:   "fallback_cdn",
			LinkID:  "fallback_cdn",
			Label:   "Cloudflare 握手/请求失败，自动切换国际备用节点测速",
			Message: fmt.Sprintf("Cloudflare 失败原因: %s", cfRes.Error),
		})
		for _, m := range ep.FallbackMirrors {
			if ctx.Err() != nil {
				break
			}
			r := measureDownload(ctx, dlClient, "fallback_cdn", m.Name, m.URL, true, maxDownBytes, ep.MinValidBytes, func(cur, avg, peak float64, dl, total, elapsed int64) {
				emit(ProgressEvent{
					Type:        "sample",
					Stage:       "fallback_cdn",
					LinkID:      "fallback_cdn",
					Label:       m.Name,
					CurrentMbps: cur,
					AvgMbps:     avg,
					PeakMbps:    peak,
					Downloaded:  dl,
					TotalBytes:  total,
					ElapsedMs:   elapsed,
				})
			})
			r.IsFallback = true
			if r.OK {
				r.Note = fmt.Sprintf("因 Cloudflare 测速失败 (%s)，已切换至 %s", cfRes.Error, m.Name)
				fallbackRes = &r
				links = append(links, r)
				emit(ProgressEvent{
					Type:       "link_done",
					Stage:      "fallback_cdn",
					LinkID:     "fallback_cdn",
					Label:      r.Name,
					AvgMbps:    r.AvgMbps,
					PeakMbps:   r.PeakMbps,
					Downloaded: r.Bytes,
					TotalBytes: maxDownBytes,
					ElapsedMs:  r.DurationMs,
					Message:    r.Note,
				})
				break
			}
		}
	}

	// 3. Optional Upload speed (when UploadMB > 0)
	var uploadRes *LinkResult
	if opts.UploadMB > 0 && ctx.Err() == nil && ep.CloudflareUpURL != "" {
		upBytes := int64(opts.UploadMB) * 1024 * 1024
		emit(ProgressEvent{
			Type:    "stage",
			Stage:   "upload",
			LinkID:  "upload",
			Label:   fmt.Sprintf("上行带宽测试 (%d MB)", opts.UploadMB),
			Message: "正在测量请求与文件上行吞吐...",
		})
		r := measureUpload(ctx, dlClient, "upload", "Cloudflare 上行", ep.CloudflareUpURL, upBytes, func(cur, avg, peak float64, sent, total, elapsed int64) {
			emit(ProgressEvent{
				Type:        "sample",
				Stage:       "upload",
				LinkID:      "upload",
				Label:       "上行带宽",
				CurrentMbps: cur,
				AvgMbps:     avg,
				PeakMbps:    peak,
				Downloaded:  sent,
				TotalBytes:  total,
				ElapsedMs:   elapsed,
			})
		})
		uploadRes = &r
		links = append(links, r)
		emit(ProgressEvent{
			Type:       "link_done",
			Stage:      "upload",
			LinkID:     "upload",
			Label:      r.Name,
			AvgMbps:    r.AvgMbps,
			PeakMbps:   r.PeakMbps,
			Downloaded: r.Bytes,
			TotalBytes: upBytes,
			ElapsedMs:  r.DurationMs,
			Message:    r.Error,
		})
	}

	// 4. Target TTFB + jitter + GGC node + optional Token/s
	emit(ProgressEvent{
		Type:    "stage",
		Stage:   "targets",
		Label:   "测试 4 大目标响应延迟与实感速率",
		Message: "正在采样 ChatGPT / Gemini / 反重力 / YouTube...",
	})

	type ttfbStat struct {
		ttfb   int64
		jitter int64
	}
	targetIDs := []string{"chatgpt", "gemini", "antigravity", "youtube"}
	stats := make(map[string]ttfbStat, len(targetIDs))
	var statsMu sync.Mutex
	var ggcNode string
	var openAITokenRes, geminiTokenRes *TokenSpeedResult

	var wg sync.WaitGroup
	for _, id := range targetIDs {
		probeURL := ep.TargetProbeURLs[id]
		if probeURL == "" {
			continue
		}
		wg.Add(1)
		go func(tid, u string) {
			defer wg.Done()
			ttfb, jitter := measureTargetTTFB(ctx, probeClient, u, 2)
			statsMu.Lock()
			stats[tid] = ttfbStat{ttfb: ttfb, jitter: jitter}
			statsMu.Unlock()
		}(id, probeURL)
	}

	if ep.YouTubeGGCURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 7*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(c, http.MethodGet, ep.YouTubeGGCURL, nil)
			if err != nil {
				return
			}
			netutil.ApplyBrowserHeaders(req)
			resp, err := probeClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			ggcNode = unlock.ParseGGCNode(string(data))
		}()
	}

	if strings.TrimSpace(opts.OpenAIAPIKey) != "" && ep.OpenAIChatURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			openAITokenRes = measureOpenAITokenSpeed(ctx, dlClient, ep.OpenAIChatURL, opts.OpenAIAPIKey, opts.OpenAIModel)
		}()
	}

	if strings.TrimSpace(opts.GeminiAPIKey) != "" && ep.GeminiStreamURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			geminiTokenRes = measureGeminiTokenSpeed(ctx, dlClient, ep.GeminiStreamURL, opts.GeminiAPIKey, opts.GeminiModel)
		}()
	}

	wg.Wait()

	// Build 4 target summaries honestly
	gptDownload := cfRes.AvgMbps
	gptBackbone := "Cloudflare 边缘链路"
	gptNote := ""
	if !cfRes.OK {
		if fallbackRes != nil && fallbackRes.OK {
			gptDownload = fallbackRes.AvgMbps
			gptBackbone = fallbackRes.Name + "（备用）"
			gptNote = fallbackRes.Note
		} else {
			gptBackbone = "Cloudflare 测速失败"
			gptNote = cfRes.Error
		}
	}
	var upMbps float64
	if uploadRes != nil && uploadRes.OK {
		upMbps = uploadRes.AvgMbps
	}

	googleMbps := googleRes.AvgMbps
	googleBackbone := "Google 全球 CDN"
	if !googleRes.OK {
		googleBackbone = "Google CDN 测速失败"
	}

	targets := []TargetSpeedSummary{
		{
			ID:            "chatgpt",
			Name:          "ChatGPT / GPT",
			DownloadMbps:  round2(gptDownload),
			UploadMbps:    round2(upMbps),
			TTFBMs:        stats["chatgpt"].ttfb,
			JitterMs:      stats["chatgpt"].jitter,
			BackboneLabel: gptBackbone,
			QualityTag:    classifyAIQuality(gptDownload, stats["chatgpt"].ttfb, stats["chatgpt"].jitter),
			TokenSpeed:    openAITokenRes,
			Note:          gptNote,
		},
		{
			ID:            "gemini",
			Name:          "Google Gemini",
			DownloadMbps:  round2(googleMbps),
			TTFBMs:        stats["gemini"].ttfb,
			JitterMs:      stats["gemini"].jitter,
			BackboneLabel: googleBackbone,
			QualityTag:    classifyAIQuality(googleMbps, stats["gemini"].ttfb, stats["gemini"].jitter),
			TokenSpeed:    geminiTokenRes,
			Note:          googleRes.Error,
		},
		{
			ID:            "antigravity",
			Name:          "Antigravity (反重力)",
			DownloadMbps:  round2(googleMbps),
			TTFBMs:        stats["antigravity"].ttfb,
			JitterMs:      stats["antigravity"].jitter,
			BackboneLabel: googleBackbone,
			QualityTag:    classifyAntigravityQuality(googleMbps, stats["antigravity"].ttfb, stats["antigravity"].jitter),
			Note:          googleRes.Error,
		},
		{
			ID:            "youtube",
			Name:          "YouTube",
			DownloadMbps:  round2(googleMbps),
			TTFBMs:        stats["youtube"].ttfb,
			JitterMs:      stats["youtube"].jitter,
			BackboneLabel: googleBackbone,
			QualityTag:    classifyYouTubeQuality(googleMbps, googleRes.OK),
			GGCNode:       ggcNode,
			Note:          googleRes.Error,
		},
	}

	report := &Report{
		DownloadMB: opts.DownloadMB,
		UploadMB:   opts.UploadMB,
		Links:      links,
		Targets:    targets,
		CheckedAt:  netutil.FormatNow(),
	}
	emit(ProgressEvent{
		Type:    "done",
		Stage:   "done",
		Message: "测速完成",
		Report:  report,
	})
	return report, nil
}

func measureDownload(
	ctx context.Context,
	client *http.Client,
	id, name, targetURL string,
	useRange bool,
	maxBytes int64,
	minValidBytes int64,
	onSample func(cur, avg, peak float64, dl, total, elapsed int64),
) LinkResult {
	res := LinkResult{
		ID:        id,
		Name:      name,
		SourceURL: targetURL,
	}
	if strings.TrimSpace(targetURL) == "" {
		res.Error = "测速地址为空"
		return res
	}

	var ttfbMs int64
	start := time.Now()
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			ttfbMs = time.Since(start).Milliseconds()
		},
	}

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, targetURL, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	netutil.ApplyBrowserHeaders(req)
	if useRange && maxBytes > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", maxBytes-1))
	}

	resp, err := client.Do(req)
	if err != nil {
		res.Error = err.Error()
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		res.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}

	transferStart := time.Now()
	if ttfbMs <= 0 {
		ttfbMs = time.Since(start).Milliseconds()
	}
	res.TTFBMs = max(1, ttfbMs)

	reader := io.LimitReader(resp.Body, maxBytes)
	buf := make([]byte, 32*1024)
	var (
		totalRead    int64
		windowBytes  int64
		lastTick     = time.Now()
		peakMbps     float64
		samples      []float64
	)

	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			totalRead += int64(n)
			windowBytes += int64(n)
			now := time.Now()
			winElapsed := now.Sub(lastTick).Seconds()
			if winElapsed >= 0.25 {
				instMbps := (float64(windowBytes) * 8.0) / winElapsed / 1_000_000.0
				totalElapsed := maxFloat(now.Sub(transferStart).Seconds(), 0.001)
				avgMbps := (float64(totalRead) * 8.0) / totalElapsed / 1_000_000.0
				if instMbps > peakMbps {
					peakMbps = instMbps
				}
				samples = append(samples, round2(instMbps))
				if onSample != nil {
					onSample(round2(instMbps), round2(avgMbps), round2(peakMbps), totalRead, maxBytes, now.Sub(transferStart).Milliseconds())
				}
				windowBytes = 0
				lastTick = now
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if totalRead > minValidBytes {
				break
			}
			res.Error = readErr.Error()
			res.Bytes = totalRead
			res.DurationMs = time.Since(transferStart).Milliseconds()
			return res
		}
	}

	elapsed := maxFloat(time.Since(transferStart).Seconds(), 0.001)
	avgMbps := (float64(totalRead) * 8.0) / elapsed / 1_000_000.0
	if peakMbps < avgMbps {
		peakMbps = avgMbps
	}
	if len(samples) == 0 && totalRead > 0 {
		samples = append(samples, round2(avgMbps))
	}

	if totalRead < minValidBytes {
		res.Error = fmt.Sprintf("返回数据过小 (%d B)，可能被网关拦截", totalRead)
		res.Bytes = totalRead
		res.DurationMs = time.Since(transferStart).Milliseconds()
		return res
	}

	res.OK = true
	res.Bytes = totalRead
	res.DurationMs = max(1, time.Since(transferStart).Milliseconds())
	res.AvgMbps = round2(avgMbps)
	res.PeakMbps = round2(peakMbps)
	res.Samples = samples
	return res
}

type progressZeroReader struct {
	remaining int64
	sent      int64
	total     int64
	start     time.Time
	lastTick  time.Time
	winBytes  int64
	peakMbps  float64
	samples   []float64
	onSample  func(cur, avg, peak float64, sent, total, elapsed int64)
}

func (r *progressZeroReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}
	clear(p[:n])
	r.remaining -= int64(n)
	r.sent += int64(n)
	r.winBytes += int64(n)

	now := time.Now()
	winElapsed := now.Sub(r.lastTick).Seconds()
	if winElapsed >= 0.25 {
		instMbps := (float64(r.winBytes) * 8.0) / winElapsed / 1_000_000.0
		totalElapsed := maxFloat(now.Sub(r.start).Seconds(), 0.001)
		avgMbps := (float64(r.sent) * 8.0) / totalElapsed / 1_000_000.0
		if instMbps > r.peakMbps {
			r.peakMbps = instMbps
		}
		r.samples = append(r.samples, round2(instMbps))
		if r.onSample != nil {
			r.onSample(round2(instMbps), round2(avgMbps), round2(r.peakMbps), r.sent, r.total, now.Sub(r.start).Milliseconds())
		}
		r.winBytes = 0
		r.lastTick = now
	}
	return n, nil
}

func measureUpload(
	ctx context.Context,
	client *http.Client,
	id, name, targetURL string,
	totalBytes int64,
	onSample func(cur, avg, peak float64, sent, total, elapsed int64),
) LinkResult {
	res := LinkResult{
		ID:        id,
		Name:      name,
		SourceURL: targetURL,
	}
	now := time.Now()
	zr := &progressZeroReader{
		remaining: totalBytes,
		total:     totalBytes,
		start:     now,
		lastTick:  now,
		onSample:  onSample,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, zr)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.ContentLength = totalBytes
	req.Header.Set("Content-Type", "application/octet-stream")
	netutil.ApplyBrowserHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		res.Error = err.Error()
		res.DurationMs = time.Since(now).Milliseconds()
		return res
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		res.DurationMs = time.Since(now).Milliseconds()
		return res
	}

	elapsed := maxFloat(time.Since(now).Seconds(), 0.001)
	avgMbps := (float64(zr.sent) * 8.0) / elapsed / 1_000_000.0
	peak := maxFloat(zr.peakMbps, avgMbps)
	samples := zr.samples
	if len(samples) == 0 && zr.sent > 0 {
		samples = append(samples, round2(avgMbps))
	}

	res.OK = true
	res.Bytes = zr.sent
	res.DurationMs = max(1, time.Since(now).Milliseconds())
	res.AvgMbps = round2(avgMbps)
	res.PeakMbps = round2(peak)
	res.Samples = samples
	return res
}

func measureTargetTTFB(ctx context.Context, client *http.Client, targetURL string, count int) (int64, int64) {
	var samples []int64
	for range count {
		if ctx.Err() != nil {
			break
		}
		c, cancel := context.WithTimeout(ctx, 6*time.Second)
		start := time.Now()
		var ttfb int64
		trace := &httptrace.ClientTrace{
			GotFirstResponseByte: func() {
				ttfb = time.Since(start).Milliseconds()
			},
		}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(c, trace), http.MethodGet, targetURL, nil)
		if err != nil {
			cancel()
			continue
		}
		netutil.ApplyBrowserHeaders(req)
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.CopyN(io.Discard, resp.Body, 2048)
			resp.Body.Close()
			if ttfb <= 0 {
				ttfb = time.Since(start).Milliseconds()
			}
			samples = append(samples, max(1, ttfb))
		}
		cancel()
	}
	if len(samples) == 0 {
		return 0, 0
	}
	var sum int64
	for _, s := range samples {
		sum += s
	}
	avg := sum / int64(len(samples))
	var jitter int64
	if len(samples) >= 2 {
		d := samples[1] - samples[0]
		if d < 0 {
			d = -d
		}
		jitter = d
	}
	return avg, jitter
}

func measureOpenAITokenSpeed(ctx context.Context, client *http.Client, chatURL, apiKey, model string) *TokenSpeedResult {
	res := &TokenSpeedResult{Provider: "OpenAI", Model: model}
	payload, _ := json.Marshal(map[string]any{
		"model":          model,
		"stream":         true,
		"max_tokens":     48,
		"stream_options": map[string]bool{"include_usage": true},
		"messages": []map[string]string{
			{"role": "user", "content": "Count from 1 to 20 separated by spaces."},
		},
	})

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL, bytes.NewReader(payload))
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))

	resp, err := client.Do(req)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		return res
	}

	var (
		firstTokenAt     time.Time
		completionTokens int
		chunkCount       int
		textBuf          strings.Builder
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(raw), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
			completionTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
			chunkCount++
			textBuf.WriteString(chunk.Choices[0].Delta.Content)
		}
	}

	totalDur := time.Since(start)
	res.TotalMs = max(1, totalDur.Milliseconds())
	res.OutputChars = utf8.RuneCountInString(textBuf.String())
	if !firstTokenAt.IsZero() {
		res.TTFTMs = max(1, firstTokenAt.Sub(start).Milliseconds())
	} else {
		res.TTFTMs = res.TotalMs
	}

	tokens := completionTokens
	if tokens <= 0 {
		tokens = chunkCount
	}
	if tokens <= 0 && res.OutputChars > 0 {
		tokens = max(1, res.OutputChars/3)
	}
	genSec := time.Since(firstTokenAt).Seconds()
	if firstTokenAt.IsZero() || genSec < 0.05 {
		genSec = totalDur.Seconds()
	}
	if tokens > 0 && genSec > 0 {
		res.OK = true
		res.TokensPerSec = round2(float64(tokens) / genSec)
	} else {
		res.Error = "未收到有效的流式输出内容"
	}
	return res
}

func measureGeminiTokenSpeed(ctx context.Context, client *http.Client, streamURLTemplate, apiKey, model string) *TokenSpeedResult {
	res := &TokenSpeedResult{Provider: "Gemini", Model: model}
	endpoint := streamURLTemplate
	if strings.Contains(endpoint, "%s") {
		endpoint = fmt.Sprintf(endpoint, model)
	}

	payload, _ := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{"parts": []map[string]string{{"text": "Count from 1 to 20 separated by spaces."}}},
		},
		"generationConfig": map[string]int{"maxOutputTokens": 48},
	})

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", strings.TrimSpace(apiKey))

	resp, err := client.Do(req)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		return res
	}

	var (
		firstTokenAt     time.Time
		completionTokens int
		textBuf          strings.Builder
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" {
			continue
		}
		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			UsageMetadata *struct {
				CandidatesTokenCount int `json:"candidatesTokenCount"`
			} `json:"usageMetadata"`
		}
		if json.Unmarshal([]byte(raw), &chunk) != nil {
			continue
		}
		if chunk.UsageMetadata != nil && chunk.UsageMetadata.CandidatesTokenCount > 0 {
			completionTokens = chunk.UsageMetadata.CandidatesTokenCount
		}
		for _, cand := range chunk.Candidates {
			for _, part := range cand.Content.Parts {
				if part.Text != "" {
					if firstTokenAt.IsZero() {
						firstTokenAt = time.Now()
					}
					textBuf.WriteString(part.Text)
				}
			}
		}
	}

	totalDur := time.Since(start)
	res.TotalMs = max(1, totalDur.Milliseconds())
	res.OutputChars = utf8.RuneCountInString(textBuf.String())
	if !firstTokenAt.IsZero() {
		res.TTFTMs = max(1, firstTokenAt.Sub(start).Milliseconds())
	} else {
		res.TTFTMs = res.TotalMs
	}

	tokens := completionTokens
	if tokens <= 0 && res.OutputChars > 0 {
		tokens = max(1, res.OutputChars/3)
	}
	genSec := time.Since(firstTokenAt).Seconds()
	if firstTokenAt.IsZero() || genSec < 0.05 {
		genSec = totalDur.Seconds()
	}
	if tokens > 0 && genSec > 0 {
		res.OK = true
		res.TokensPerSec = round2(float64(tokens) / genSec)
	} else {
		res.Error = "未收到有效的流式输出内容"
	}
	return res
}

func classifyYouTubeQuality(mbps float64, ok bool) string {
	if !ok || mbps <= 0 {
		return "测速失败"
	}
	switch {
	case mbps >= 80:
		return "8K HDR 极速流畅"
	case mbps >= 30:
		return "4K 60fps 流畅"
	case mbps >= 12:
		return "2K / 1080P60 流畅"
	case mbps >= 5:
		return "1080P 流畅"
	default:
		return "720P 及以下（偏慢）"
	}
}

func classifyAIQuality(mbps float64, ttfb, jitter int64) string {
	if mbps <= 0 && ttfb <= 0 {
		return "连接失败"
	}
	if ttfb > 0 && ttfb < 400 && mbps >= 20 && jitter < 120 {
		return "极速低延迟"
	}
	if mbps >= 8 && (ttfb == 0 || ttfb < 900) {
		return "流畅可用"
	}
	return "可用但偏慢"
}

func classifyAntigravityQuality(mbps float64, ttfb, jitter int64) string {
	if mbps <= 0 && ttfb <= 0 {
		return "连接失败"
	}
	if ttfb > 0 && ttfb < 350 && jitter < 100 && mbps >= 15 {
		return "低抖动 / IDE 流畅"
	}
	if mbps >= 8 && (ttfb == 0 || ttfb < 800) {
		return "可用 / 偶有首包延迟"
	}
	return "抖动或带宽偏低"
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
