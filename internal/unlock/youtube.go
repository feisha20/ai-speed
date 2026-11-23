package unlock

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"ai-speed/internal/netutil"
)

var youtubeRegionRegexes = []*regexp.Regexp{
	regexp.MustCompile(`"INNERTUBE_CONTEXT_GL"\s*:\s*"([A-Za-z]{2,3})"`),
	regexp.MustCompile(`id=["']country-code["'][^>]*>\s*([A-Za-z]{2,3})\s*<`),
	regexp.MustCompile(`"GL"\s*:\s*"([A-Za-z]{2})"`),
	regexp.MustCompile(`"countryCode"\s*:\s*"([A-Za-z]{2})"`),
	regexp.MustCompile(`"country_code"\s*:\s*"([A-Za-z]{2})"`),
}

// ParseGGCNode extracts the YouTube GGC cache server identifier from report_mapping output.
func ParseGGCNode(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, "=>"); idx >= 0 {
			right := strings.TrimSpace(line[idx+2:])
			if fields := strings.Fields(right); len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}

func checkYouTube(ctx context.Context, opts Options, ep Endpoints) (ItemResult, error) {
	client, err := netutil.NewHTTPClient(opts.Proxy, 10*time.Second, false)
	if err != nil {
		return ItemResult{}, err
	}

	res := ItemResult{
		ID:        "youtube",
		Name:      "YouTube",
		CheckedAt: netutil.FormatNow(),
	}

	var (
		wg           sync.WaitGroup
		premiumCheck SubCheck
		ggcCheck     SubCheck
		region       string
		ggcNode      string
		sentToCN     bool
		premOK       bool
		premNetErr   bool
	)

	wg.Add(2)

	// 1. YouTube Premium & region / 送中 check
	go func() {
		defer wg.Done()
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.YouTubePremiumURL, nil)
		if err != nil {
			premNetErr = true
			premiumCheck = SubCheck{Label: "YouTube Premium & 地区", Status: "fail", Detail: err.Error()}
			return
		}
		netutil.ApplyBrowserHeaders(req)
		resp, err := client.Do(req)
		ms := time.Since(start).Milliseconds()
		if err != nil {
			premNetErr = true
			premiumCheck = SubCheck{Label: "YouTube Premium & 地区", Status: "fail", Detail: err.Error(), LatencyMs: ms}
			return
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		if err != nil {
			premNetErr = true
			premiumCheck = SubCheck{Label: "YouTube Premium & 地区", Status: "fail", Detail: err.Error(), LatencyMs: ms}
			return
		}
		body := string(data)
		lower := strings.ToLower(body)

		if strings.Contains(body, "www.google.cn") {
			sentToCN = true
			region = "CN"
			premiumCheck = SubCheck{
				Label:     "YouTube Premium & 地区",
				Status:    "fail",
				Detail:    "检测到已被“送中”(google.cn)，无法正常使用 Premium 与部分 AI 功能",
				LatencyMs: ms,
			}
			return
		}

		for _, re := range youtubeRegionRegexes {
			if m := re.FindStringSubmatch(body); len(m) > 1 {
				region = netutil.Alpha3ToAlpha2(m[1])
				break
			}
		}

		if strings.Contains(lower, "youtube premium is not available in your country") ||
			strings.Contains(lower, "premium is not available in your country") ||
			strings.Contains(lower, "premium is not available in your region") {
			premiumCheck = SubCheck{
				Label:     "YouTube Premium & 地区",
				Status:    "fail",
				Detail:    "该地区不支持 YouTube Premium",
				LatencyMs: ms,
			}
			return
		}

		unlocked := resp.StatusCode >= 200 && resp.StatusCode < 300 &&
			(strings.Contains(lower, "youtube premium") ||
				strings.Contains(lower, "ad-free") ||
				strings.Contains(lower, `"browseid":"spunlimited"`))
		if unlocked {
			premOK = true
			detail := "支持 YouTube Premium"
			if region != "" {
				detail = fmt.Sprintf("支持 YouTube Premium (地区: %s)", region)
			}
			premiumCheck = SubCheck{
				Label:     "YouTube Premium & 地区",
				Status:    "ok",
				Detail:    detail,
				LatencyMs: ms,
			}
			return
		}

		premiumCheck = SubCheck{
			Label:     "YouTube Premium & 地区",
			Status:    "warn",
			Detail:    fmt.Sprintf("页面可访问但未匹配 Premium 标记 (HTTP %d)", resp.StatusCode),
			LatencyMs: ms,
		}
	}()

	// 2. YouTube GGC edge POP check
	go func() {
		defer wg.Done()
		start := time.Now()
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(c, http.MethodGet, ep.YouTubeGGCURL, nil)
		if err != nil {
			ggcCheck = SubCheck{Label: "GGC 视频缓存节点", Status: "warn", Detail: err.Error()}
			return
		}
		netutil.ApplyBrowserHeaders(req)
		resp, err := client.Do(req)
		ms := time.Since(start).Milliseconds()
		if err != nil {
			ggcCheck = SubCheck{Label: "GGC 视频缓存节点", Status: "warn", Detail: err.Error(), LatencyMs: ms}
			return
		}
		defer resp.Body.Close()
		// Read conservatively because redirector.googlevideo.com may close mid-body after emitting the mapping line.
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		ggcNode = ParseGGCNode(string(data))
		if ggcNode != "" {
			ggcCheck = SubCheck{
				Label:     "GGC 视频缓存节点",
				Status:    "ok",
				Detail:    fmt.Sprintf("命中视频分发节点: %s", ggcNode),
				LatencyMs: ms,
			}
		} else {
			ggcCheck = SubCheck{
				Label:     "GGC 视频缓存节点",
				Status:    "warn",
				Detail:    fmt.Sprintf("未解析到 GGC 节点代号 (HTTP %d)", resp.StatusCode),
				LatencyMs: ms,
			}
		}
	}()

	wg.Wait()

	res.Region = region
	res.Emoji = netutil.CountryEmoji(region)
	res.SubChecks = []SubCheck{premiumCheck, ggcCheck}

	switch {
	case premNetErr:
		res.Status = "failed"
		res.StatusText = "测试失败"
		res.Summary = "无法连接到 YouTube，请检查代理连通性"
	case sentToCN:
		res.Status = "no"
		res.StatusText = "已被送中"
		res.Summary = "当前节点被识别为中国大陆 (google.cn)"
	case premOK:
		res.Status = "yes"
		res.StatusText = "支持"
		if ggcNode != "" {
			res.Summary = fmt.Sprintf("YouTube Premium 可用，视频缓存节点: %s", ggcNode)
		} else {
			res.Summary = "YouTube 与 Premium 可正常访问"
		}
	default:
		res.Status = "no"
		res.StatusText = "不支持"
		res.Summary = "当前地区不支持 YouTube Premium 或页面异常"
	}

	return res, nil
}
