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

var hostingKeywords = []string{
	"amazon", "aws", "google", "microsoft", "azure", "digitalocean",
	"linode", "akamai", "vultr", "hetzner", "ovh", "oracle",
	"alibaba", "aliyun", "tencent", "cloudflare", "datacenter", "hosting",
}

func isHostingISP(isp, org string) bool {
	combined := strings.ToLower(isp + " " + org)
	for _, kw := range hostingKeywords {
		if strings.Contains(combined, kw) {
			return true
		}
	}
	return false
}

func probeReachable(ctx context.Context, client *http.Client, label, targetURL string, requireSuccess bool) (SubCheck, bool) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return SubCheck{Label: label, Status: "fail", Detail: err.Error()}, false
	}
	netutil.ApplyBrowserHeaders(req)
	resp, err := client.Do(req)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return SubCheck{Label: label, Status: "fail", Detail: err.Error(), LatencyMs: ms}, false
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	resp.Body.Close()

	if requireSuccess && (resp.StatusCode < 200 || resp.StatusCode >= 400) {
		return SubCheck{
			Label:     label,
			Status:    "warn",
			Detail:    fmt.Sprintf("HTTP %d", resp.StatusCode),
			LatencyMs: ms,
		}, false
	}
	if resp.StatusCode >= 500 {
		return SubCheck{
			Label:     label,
			Status:    "warn",
			Detail:    fmt.Sprintf("服务端异常 HTTP %d", resp.StatusCode),
			LatencyMs: ms,
		}, false
	}
	return SubCheck{
		Label:     label,
		Status:    "ok",
		Detail:    fmt.Sprintf("TLS 与路由可达 (HTTP %d)", resp.StatusCode),
		LatencyMs: ms,
	}, true
}

func checkAntigravity(ctx context.Context, opts Options, ep Endpoints, egress *netutil.EgressInfo) (ItemResult, error) {
	client, err := netutil.NewHTTPClient(opts.Proxy, 10*time.Second, false)
	if err != nil {
		return ItemResult{}, err
	}

	res := ItemResult{
		ID:        "antigravity",
		Name:      "Antigravity (反重力)",
		CheckedAt: netutil.FormatNow(),
	}

	var (
		wg            sync.WaitGroup
		webCheck      SubCheck
		webOK         bool
		cloudCheck    SubCheck
		cloudOK       bool
		dailyCheck    SubCheck
		dailyOK       bool
		googleRegion  string
		geminiBlocked bool
		apiCheck      SubCheck
		apiGeoDenied  bool
		apiOK         bool
	)

	wg.Add(5)
	go func() {
		defer wg.Done()
		webCheck, webOK = probeReachable(ctx, client, "官网入口 (antigravity.google)", ep.AntigravityWebURL, true)
	}()
	go func() {
		defer wg.Done()
		cloudCheck, cloudOK = probeReachable(ctx, client, "控制面 (cloudcode-pa)", ep.CloudCodeURL, false)
	}()
	go func() {
		defer wg.Done()
		dailyCheck, dailyOK = probeReachable(ctx, client, "日常端 (daily-cloudcode-pa)", ep.DailyCloudCodeURL, false)
	}()
	go func() {
		defer wg.Done()
		region, _, blocked, _, _, _ := probeGeminiWeb(ctx, client, ep.GeminiWebURL)
		googleRegion = region
		geminiBlocked = blocked
	}()
	go func() {
		defer wg.Done()
		if strings.TrimSpace(opts.GeminiAPIKey) == "" {
			apiCheck = SubCheck{
				Label:  "Google AI API 交叉验证",
				Status: "skip",
				Detail: "未登录/未填 Key：仅验证网络链路与出口地区，账号资格仍受 Google 账号归属地影响",
			}
			return
		}
		apiCheck, apiGeoDenied, apiOK = probeGoogleAIAPI(ctx, client, ep.GeminiAPIURL, opts.GeminiAPIKey)
		apiCheck.Label = "Google AI API 交叉验证"
	}()
	wg.Wait()

	effectiveRegion := googleRegion
	if effectiveRegion == "" && egress != nil {
		effectiveRegion = egress.IPv4Country
	}
	res.Region = effectiveRegion
	res.Emoji = netutil.CountryEmoji(effectiveRegion)

	regionBlocked := geminiBlocked || (effectiveRegion != "" && netutil.IsBlockedAIRegion(effectiveRegion))
	hasIPv6Leak := egress != nil && egress.IPv6LeakWarning != ""
	hasHostingHint := egress != nil && isHostingISP(egress.ISP, egress.Org)

	var geoCheck SubCheck
	switch {
	case regionBlocked:
		geoCheck = SubCheck{
			Label:  "出口地区与 IPv6 合规",
			Status: "fail",
			Detail: fmt.Sprintf("Google 识别地区为 %s，属于 Antigravity 不支持区域", effectiveRegion),
		}
	case hasIPv6Leak:
		geoCheck = SubCheck{
			Label:  "出口地区与 IPv6 合规",
			Status: "warn",
			Detail: egress.IPv6LeakWarning,
		}
	case effectiveRegion != "":
		detail := fmt.Sprintf("Google 识别地区 %s 属于支持区", effectiveRegion)
		if egress != nil && !egress.IPv6Available {
			detail += "（未检测到 IPv6 泄露）"
		}
		if hasHostingHint {
			detail += "；当前为云厂商/机房 ASN，若遇资格报错建议切换住宅/原生节点"
		}
		geoCheck = SubCheck{
			Label:  "出口地区与 IPv6 合规",
			Status: "ok",
			Detail: detail,
		}
	default:
		geoCheck = SubCheck{
			Label:  "出口地区与 IPv6 合规",
			Status: "warn",
			Detail: "未能解析 Google 识别地区码",
		}
	}

	res.SubChecks = []SubCheck{webCheck, cloudCheck, dailyCheck, geoCheck, apiCheck}

	switch {
	case !webOK && !cloudOK && !dailyOK:
		res.Status = "failed"
		res.StatusText = "核心域名不可达"
		res.Summary = "antigravity.google 与 cloudcode-pa 均无法连通，请检查代理或开启 Clash TUN 模式"
	case regionBlocked || apiGeoDenied:
		res.Status = "no"
		res.StatusText = "出口地区不支持"
		res.Summary = "出口 IP 地区或 API 被 Google 拒绝，使用 Antigravity 会触发 User location is not supported"
	case !cloudOK:
		res.Status = "risk"
		res.StatusText = "控制面异常"
		res.Summary = "官网可达但 cloudcode-pa.googleapis.com 不通，Antigravity 会卡在加载中"
	case hasIPv6Leak:
		res.Status = "risk"
		res.StatusText = "存在 IPv6 风险"
		res.Summary = "核心域名可达，但检测到 IPv6 出口跨区或位于受限区，建议关闭代理 IPv6"
	case cloudOK && webOK && apiOK:
		res.Status = "yes"
		res.StatusText = "链路 + API 就绪"
		res.Summary = "3 个核心域名可达、出口地区受支持且 Google AI API 交叉验证通过"
	case cloudOK && webOK:
		res.Status = "yes"
		res.StatusText = "网络链路就绪"
		res.Summary = "3 个核心域名可达且出口地区受支持（注：最终资格仍取决于登录后的 Google 账号归属地）"
	default:
		res.Status = "risk"
		res.StatusText = "部分异常"
		res.Summary = "部分核心域名连通状态异常，请查看明细"
	}

	return res, nil
}
