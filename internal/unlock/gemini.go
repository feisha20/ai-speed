package unlock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"ai-speed/internal/netutil"
)

var geminiRegionRe = regexp.MustCompile(`,2,1,200,"([A-Z]{3})"`)

type googleAPIError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// probeGeminiWeb queries gemini.google.com and extracts Google's perceived alpha-2 region & status.
func probeGeminiWeb(ctx context.Context, client *http.Client, targetURL string) (region string, ok bool, blocked bool, netErr error, statusCode int, ms int64) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", false, false, err, 0, 0
	}
	netutil.ApplyBrowserHeaders(req)

	resp, err := client.Do(req)
	ms = time.Since(start).Milliseconds()
	if err != nil {
		return "", false, false, err, 0, ms
	}
	defer resp.Body.Close()
	statusCode = resp.StatusCode

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return "", false, false, err, statusCode, ms
	}
	body := string(data)

	if statusCode == http.StatusForbidden || statusCode == http.StatusUnavailableForLegalReasons {
		return "", false, true, nil, statusCode, ms
	}

	if matches := geminiRegionRe.FindStringSubmatch(body); len(matches) > 1 {
		region = netutil.Alpha3ToAlpha2(matches[1])
	}

	hasPositiveMarker := strings.Contains(body, "45631641,null,true") || strings.Contains(body, "45617354,null,true")
	if region != "" {
		if netutil.IsBlockedAIRegion(region) {
			return region, false, true, nil, statusCode, ms
		}
		return region, statusCode >= 200 && statusCode < 400, false, nil, statusCode, ms
	}

	if statusCode >= 200 && statusCode < 300 && hasPositiveMarker {
		return "", true, false, nil, statusCode, ms
	}

	return "", false, false, nil, statusCode, ms
}

// probeGoogleAIAPI checks generativelanguage.googleapis.com with a user-supplied Gemini API key.
func probeGoogleAIAPI(ctx context.Context, client *http.Client, apiURL, apiKey string) (sub SubCheck, geoDenied bool, ok bool) {
	trimmedKey := strings.TrimSpace(apiKey)
	if trimmedKey == "" {
		return SubCheck{
			Label:  "Gemini API (可选)",
			Status: "skip",
			Detail: "未填写 API Key，仅完成网页端检测",
		}, false, false
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return SubCheck{Label: "Gemini API (实测)", Status: "fail", Detail: err.Error()}, false, false
	}
	// Send key via header rather than URL query so it never appears in logs or error text.
	req.Header.Set("x-goog-api-key", trimmedKey)
	resp, err := client.Do(req)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return SubCheck{Label: "Gemini API (实测)", Status: "fail", Detail: err.Error(), LatencyMs: ms}, false, false
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	bodyStr := string(data)
	lower := strings.ToLower(bodyStr)

	if resp.StatusCode == http.StatusOK {
		return SubCheck{
			Label:     "Gemini API (实测)",
			Status:    "ok",
			Detail:    "API Key 实测放行 (HTTP 200)",
			LatencyMs: ms,
		}, false, true
	}

	var apiErr googleAPIError
	_ = json.Unmarshal(data, &apiErr)
	msg := strings.TrimSpace(apiErr.Error.Message)

	if strings.Contains(lower, "user location is not supported") {
		return SubCheck{
			Label:     "Gemini API (实测)",
			Status:    "fail",
			Detail:    "API 明确拒绝当前出口地区 (400 User location is not supported)",
			LatencyMs: ms,
		}, true, false
	}

	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return SubCheck{
		Label:     "Gemini API (实测)",
		Status:    "warn",
		Detail:    fmt.Sprintf("返回 %d: %s", resp.StatusCode, msg),
		LatencyMs: ms,
	}, false, false
}

func checkGemini(ctx context.Context, opts Options, ep Endpoints) (ItemResult, error) {
	client, err := netutil.NewHTTPClient(opts.Proxy, 10*time.Second, false)
	if err != nil {
		return ItemResult{}, err
	}

	res := ItemResult{
		ID:        "gemini",
		Name:      "Google Gemini",
		CheckedAt: netutil.FormatNow(),
	}

	region, webOK, webBlocked, netErr, statusCode, ms := probeGeminiWeb(ctx, client, ep.GeminiWebURL)
	var webCheck SubCheck
	switch {
	case netErr != nil:
		webCheck = SubCheck{Label: "Gemini 网页端", Status: "fail", Detail: netErr.Error(), LatencyMs: ms}
	case webBlocked:
		detail := fmt.Sprintf("出口地区或 IP 被限制 (HTTP %d)", statusCode)
		if region != "" {
			detail = fmt.Sprintf("Google 识别地区为 %s（不在支持区）", region)
		}
		webCheck = SubCheck{Label: "Gemini 网页端", Status: "fail", Detail: detail, LatencyMs: ms}
	case webOK:
		detail := "网页端正常访问"
		if region != "" {
			detail = fmt.Sprintf("网页端支持（Google 识别地区: %s）", region)
		}
		webCheck = SubCheck{Label: "Gemini 网页端", Status: "ok", Detail: detail, LatencyMs: ms}
	default:
		webCheck = SubCheck{Label: "Gemini 网页端", Status: "warn", Detail: fmt.Sprintf("未解析到地区标记 (HTTP %d)", statusCode), LatencyMs: ms}
	}

	apiCheck, apiGeoDenied, apiOK := probeGoogleAIAPI(ctx, client, ep.GeminiAPIURL, opts.GeminiAPIKey)

	res.Region = region
	res.Emoji = netutil.CountryEmoji(region)
	res.SubChecks = []SubCheck{webCheck, apiCheck}

	switch {
	case netErr != nil && strings.TrimSpace(opts.GeminiAPIKey) == "":
		res.Status = "failed"
		res.StatusText = "测试失败"
		res.Summary = "无法访问 gemini.google.com，请检查代理连通性"
	case apiGeoDenied && webOK:
		res.Status = "partial"
		res.StatusText = "仅网页可用"
		res.Summary = "Gemini 网页端可用，但 API 明确拒绝当前出口地区 (User location is not supported)"
	case apiGeoDenied || webBlocked:
		res.Status = "no"
		res.StatusText = "不支持"
		res.Summary = "当前出口地区不支持使用 Gemini"
	case webOK && apiOK:
		res.Status = "yes"
		res.StatusText = "完整支持"
		res.Summary = "Gemini 网页端与 API 均实测可用"
	case webOK:
		res.Status = "yes"
		res.StatusText = "支持"
		res.Summary = "Gemini 网页端可用（建议填写 API Key 以同步验证 API 地域限制）"
	case apiOK:
		res.Status = "partial"
		res.StatusText = "API 可用"
		res.Summary = "Gemini API 可用，网页端未返回完整地区标记"
	default:
		res.Status = "no"
		res.StatusText = "不支持"
		res.Summary = "未通过 Gemini 可用性检测"
	}

	return res, nil
}
