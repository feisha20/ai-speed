package unlock

import (
	"context"
	"fmt"
	"sync"

	"ai-speed/internal/netutil"
)

// SubCheck represents one concrete diagnostic probe within a service unlock card.
type SubCheck struct {
	Label     string `json:"label"`
	Status    string `json:"status"` // "ok" | "warn" | "fail" | "skip"
	Detail    string `json:"detail"`
	LatencyMs int64  `json:"latencyMs,omitempty"`
}

// ItemResult represents the unlock/availability verdict for one target service.
type ItemResult struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`     // "yes" | "partial" | "risk" | "no" | "failed"
	StatusText string     `json:"statusText"` // Human-readable Chinese label
	Region     string     `json:"region,omitempty"`
	Emoji      string     `json:"emoji,omitempty"`
	Summary    string     `json:"summary"`
	SubChecks  []SubCheck `json:"subChecks"`
	CheckedAt  string     `json:"checkedAt"`
}

// Options configures an unlock check run.
type Options struct {
	Proxy        netutil.ProxyConfig `json:"proxy"`
	GeminiAPIKey string              `json:"geminiApiKey,omitempty"`
	OpenAIAPIKey string              `json:"openaiApiKey,omitempty"`
}

// Endpoints holds target URLs so unit tests can inject httptest servers.
type Endpoints struct {
	CFTraceURL        string
	OpenAICookieURL   string
	OpenAIIOSURL      string
	OpenAIModelsURL   string
	GeminiWebURL      string
	GeminiAPIURL      string
	AntigravityWebURL string
	CloudCodeURL      string
	DailyCloudCodeURL string
	YouTubePremiumURL string
	YouTubeGGCURL     string
}

// DefaultEndpoints returns the production URLs for all unlock probes.
func DefaultEndpoints() Endpoints {
	return Endpoints{
		CFTraceURL:        "https://chat.openai.com/cdn-cgi/trace",
		OpenAICookieURL:   "https://api.openai.com/compliance/cookie_requirements",
		OpenAIIOSURL:      "https://ios.chat.openai.com/",
		OpenAIModelsURL:   "https://api.openai.com/v1/models",
		GeminiWebURL:      "https://gemini.google.com/",
		GeminiAPIURL:      "https://generativelanguage.googleapis.com/v1beta/models",
		AntigravityWebURL: "https://antigravity.google/",
		CloudCodeURL:      "https://cloudcode-pa.googleapis.com/",
		DailyCloudCodeURL: "https://daily-cloudcode-pa.googleapis.com/",
		YouTubePremiumURL: "https://www.youtube.com/premium?hl=en",
		YouTubeGGCURL:     "https://redirector.googlevideo.com/report_mapping",
	}
}

// CheckAll runs unlock checks concurrently for ChatGPT, Gemini, Antigravity, and YouTube.
func CheckAll(ctx context.Context, opts Options) ([]ItemResult, error) {
	egress := netutil.FetchEgress(ctx, opts.Proxy)
	return checkAllWith(ctx, opts, DefaultEndpoints(), &egress)
}

// CheckOne runs a single service unlock check by target ID ("chatgpt" | "gemini" | "antigravity" | "youtube").
func CheckOne(ctx context.Context, opts Options, targetID string) (ItemResult, error) {
	var egress *netutil.EgressInfo
	if targetID == "antigravity" {
		info := netutil.FetchEgress(ctx, opts.Proxy)
		egress = &info
	}
	return checkOneWith(ctx, opts, DefaultEndpoints(), egress, targetID)
}

func checkAllWith(ctx context.Context, opts Options, ep Endpoints, egress *netutil.EgressInfo) ([]ItemResult, error) {
	order := []string{"chatgpt", "gemini", "antigravity", "youtube"}
	results := make([]ItemResult, len(order))
	var (
		wg       sync.WaitGroup
		firstErr error
		errMu    sync.Mutex
	)
	for i, id := range order {
		wg.Add(1)
		go func(idx int, targetID string) {
			defer wg.Done()
			res, err := checkOneWith(ctx, opts, ep, egress, targetID)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				return
			}
			results[idx] = res
		}(i, id)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

func checkOneWith(ctx context.Context, opts Options, ep Endpoints, egress *netutil.EgressInfo, targetID string) (ItemResult, error) {
	switch targetID {
	case "chatgpt":
		return checkChatGPT(ctx, opts, ep)
	case "gemini":
		return checkGemini(ctx, opts, ep)
	case "antigravity":
		return checkAntigravity(ctx, opts, ep, egress)
	case "youtube":
		return checkYouTube(ctx, opts, ep)
	default:
		return ItemResult{}, fmt.Errorf("未知的解锁检测目标 %q", targetID)
	}
}
