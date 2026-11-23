package unlock

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ai-speed/internal/netutil"
)

func checkChatGPT(ctx context.Context, opts Options, ep Endpoints) (ItemResult, error) {
	client, err := netutil.NewHTTPClient(opts.Proxy, 10*time.Second, false)
	if err != nil {
		return ItemResult{}, err
	}

	res := ItemResult{
		ID:        "chatgpt",
		Name:      "ChatGPT / GPT",
		CheckedAt: netutil.FormatNow(),
	}

	var (
		wg          sync.WaitGroup
		region      string
		colo        string
		traceCheck  SubCheck
		webCheck    SubCheck
		appCheck    SubCheck
		apiCheck    SubCheck
		webOK       bool
		webNetErr   bool
		appOK       bool
		appNetErr   bool
		apiOK       bool
		apiBlocked  bool
		apiNetErr   bool
	)

	wg.Add(4)

	// 1. Cloudflare trace for exit country & POP
	go func() {
		defer wg.Done()
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.CFTraceURL, nil)
		if err != nil {
			traceCheck = SubCheck{Label: "Cloudflare 出口", Status: "fail", Detail: err.Error()}
			return
		}
		netutil.ApplyBrowserHeaders(req)
		resp, err := client.Do(req)
		ms := time.Since(start).Milliseconds()
		if err != nil {
			traceCheck = SubCheck{Label: "Cloudflare 出口", Status: "fail", Detail: err.Error(), LatencyMs: ms}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		kv := netutil.ParseCFTrace(string(body))
		region = strings.ToUpper(strings.TrimSpace(kv["loc"]))
		colo = strings.TrimSpace(kv["colo"])
		if region != "" {
			traceCheck = SubCheck{
				Label:     "Cloudflare 出口",
				Status:    "ok",
				Detail:    fmt.Sprintf("识别地区 %s (POP: %s)", region, colo),
				LatencyMs: ms,
			}
		} else {
			traceCheck = SubCheck{
				Label:     "Cloudflare 出口",
				Status:    "warn",
				Detail:    fmt.Sprintf("未解析到地区码 (HTTP %d)", resp.StatusCode),
				LatencyMs: ms,
			}
		}
	}()

	// 2. Web compliance check
	go func() {
		defer wg.Done()
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.OpenAICookieURL, nil)
		if err != nil {
			webNetErr = true
			webCheck = SubCheck{Label: "Web 网页合规", Status: "fail", Detail: err.Error()}
			return
		}
		netutil.ApplyBrowserHeaders(req)
		resp, err := client.Do(req)
		ms := time.Since(start).Milliseconds()
		if err != nil {
			webNetErr = true
			webCheck = SubCheck{Label: "Web 网页合规", Status: "fail", Detail: err.Error(), LatencyMs: ms}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		lower := strings.ToLower(string(body))
		if strings.Contains(lower, "unsupported_country") {
			webCheck = SubCheck{Label: "Web 网页合规", Status: "fail", Detail: "地区不支持 (unsupported_country)", LatencyMs: ms}
			return
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			webOK = true
			webCheck = SubCheck{Label: "Web 网页合规", Status: "ok", Detail: fmt.Sprintf("地区放行 (HTTP %d)", resp.StatusCode), LatencyMs: ms}
			return
		}
		webCheck = SubCheck{Label: "Web 网页合规", Status: "warn", Detail: fmt.Sprintf("响应异常 (HTTP %d)", resp.StatusCode), LatencyMs: ms}
	}()

	// 3. iOS App / ISP risk check
	go func() {
		defer wg.Done()
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.OpenAIIOSURL, nil)
		if err != nil {
			appNetErr = true
			appCheck = SubCheck{Label: "App 客户端风控", Status: "fail", Detail: err.Error()}
			return
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 16_6_0 like Mac OS X) AppleWebKit/537.36 (KHTML, like Gecko) Mobile/16G29 ChatGPT/3.0")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Requested-With", "com.openai.chatgpt")
		req.Header.Set("Referer", "https://chat.openai.com/")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		resp, err := client.Do(req)
		ms := time.Since(start).Milliseconds()
		if err != nil {
			appNetErr = true
			appCheck = SubCheck{Label: "App 客户端风控", Status: "fail", Detail: err.Error(), LatencyMs: ms}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
		lower := strings.ToLower(string(body))
		switch {
		case strings.Contains(lower, "unsupported_country"):
			appCheck = SubCheck{Label: "App 客户端风控", Status: "fail", Detail: "地区被拒绝 (unsupported_country)", LatencyMs: ms}
		case strings.Contains(lower, "disallowed isp"):
			appCheck = SubCheck{Label: "App 客户端风控", Status: "fail", Detail: "ISP 运营商被 OpenAI 拦截 (disallowed isp)", LatencyMs: ms}
		case strings.Contains(lower, "vpn"):
			appCheck = SubCheck{Label: "App 客户端风控", Status: "fail", Detail: "触发 VPN 代理拦截", LatencyMs: ms}
		case strings.Contains(lower, "sorry, you have been blocked") || strings.Contains(lower, "you have been blocked"):
			appCheck = SubCheck{Label: "App 客户端风控", Status: "fail", Detail: "IP 被 Cloudflare / OpenAI 封禁", LatencyMs: ms}
		case strings.Contains(lower, "request is not allowed") || strings.Contains(lower, `"type":"dc"`) || strings.Contains(lower, `"type": "dc"`):
			appCheck = SubCheck{Label: "App 客户端风控", Status: "fail", Detail: "App 客户端拦截：识别为机房/风控 IP (type: dc, HTTP 403)", LatencyMs: ms}
		case resp.StatusCode == http.StatusForbidden && (strings.Contains(lower, "just a moment") || strings.Contains(lower, "challenge")):
			appCheck = SubCheck{Label: "App 客户端风控", Status: "warn", Detail: "触发 Cloudflare 质询 (HTTP 403)", LatencyMs: ms}
		case resp.StatusCode == http.StatusForbidden:
			appCheck = SubCheck{Label: "App 客户端风控", Status: "warn", Detail: "App 客户端返回 HTTP 403（可能触发风控限制）", LatencyMs: ms}
		case resp.StatusCode >= 500:
			appCheck = SubCheck{Label: "App 客户端风控", Status: "warn", Detail: fmt.Sprintf("服务端错误 (HTTP %d)", resp.StatusCode), LatencyMs: ms}
		default:
			appOK = true
			appCheck = SubCheck{Label: "App 客户端风控", Status: "ok", Detail: fmt.Sprintf("客户端接口未拦截 (HTTP %d)", resp.StatusCode), LatencyMs: ms}
		}
	}()

	// 4. OpenAI API route probe
	go func() {
		defer wg.Done()
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.OpenAIModelsURL, nil)
		if err != nil {
			apiNetErr = true
			apiCheck = SubCheck{Label: "OpenAI API 路由", Status: "fail", Detail: err.Error()}
			return
		}
		netutil.ApplyBrowserHeaders(req)
		hasKey := strings.TrimSpace(opts.OpenAIAPIKey) != ""
		if hasKey {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(opts.OpenAIAPIKey))
		}
		resp, err := client.Do(req)
		ms := time.Since(start).Milliseconds()
		if err != nil {
			apiNetErr = true
			apiCheck = SubCheck{Label: "OpenAI API 路由", Status: "fail", Detail: err.Error(), LatencyMs: ms}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		lower := strings.ToLower(string(body))

		if strings.Contains(lower, "unsupported_country") || strings.Contains(lower, "country, region, or territory not supported") {
			apiBlocked = true
			apiCheck = SubCheck{Label: "OpenAI API 路由", Status: "fail", Detail: fmt.Sprintf("API 明确拒绝当前地区 (HTTP %d)", resp.StatusCode), LatencyMs: ms}
			return
		}
		switch resp.StatusCode {
		case http.StatusOK:
			apiOK = true
			detail := "API 接口可达"
			if hasKey {
				detail = "API Key 实测可用 (HTTP 200)"
			}
			apiCheck = SubCheck{Label: "OpenAI API 路由", Status: "ok", Detail: detail, LatencyMs: ms}
		case http.StatusUnauthorized:
			apiOK = true
			detail := "地区放行（未填 Key，正常返回 401 鉴权提示）"
			if hasKey {
				detail = "地区放行，但填写的 API Key 无效 (HTTP 401)"
				apiCheck = SubCheck{Label: "OpenAI API 路由", Status: "warn", Detail: detail, LatencyMs: ms}
				return
			}
			apiCheck = SubCheck{Label: "OpenAI API 路由", Status: "ok", Detail: detail, LatencyMs: ms}
		case http.StatusForbidden:
			apiCheck = SubCheck{Label: "OpenAI API 路由", Status: "warn", Detail: "API 返回 403（可能触发风控）", LatencyMs: ms}
		default:
			apiCheck = SubCheck{Label: "OpenAI API 路由", Status: "warn", Detail: fmt.Sprintf("返回 HTTP %d", resp.StatusCode), LatencyMs: ms}
		}
	}()

	wg.Wait()

	res.Region = region
	res.Emoji = netutil.CountryEmoji(region)
	res.SubChecks = []SubCheck{traceCheck, webCheck, appCheck, apiCheck}

	switch {
	case webNetErr && appNetErr && apiNetErr:
		res.Status = "failed"
		res.StatusText = "测试失败"
		res.Summary = "无法连接到 OpenAI 相关端点，请检查代理连通性"
	case apiBlocked && !webOK:
		res.Status = "no"
		res.StatusText = "不支持"
		res.Summary = "Web 与 API 均被判定为不支持地区"
	case !webOK && !appOK:
		res.Status = "no"
		res.StatusText = "不支持"
		res.Summary = "OpenAI 网页端与客户端均未放行"
	case apiBlocked && webOK:
		res.Status = "partial"
		res.StatusText = "部分支持"
		res.Summary = "网页端可访问，但 OpenAI API 返回地区限制"
	case webOK && appOK && apiOK:
		res.Status = "yes"
		res.StatusText = "完整支持"
		res.Summary = "Web、App 客户端风控与 API 路由全部放行"
	default:
		res.Status = "partial"
		res.StatusText = "部分支持"
		res.Summary = "网页端可用，但 App 客户端风控或 API 存在拦截提示"
	}

	return res, nil
}
