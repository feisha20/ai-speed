package netutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// EgressInfo summarizes the active proxy egress IP, GeoIP metadata, and IPv6 leak checks.
type EgressInfo struct {
	IPv4            string `json:"ipv4,omitempty"`
	IPv4Country     string `json:"ipv4Country,omitempty"`
	IPv4Emoji       string `json:"ipv4Emoji,omitempty"`
	IPv4City        string `json:"ipv4City,omitempty"`
	ISP             string `json:"isp,omitempty"`
	ASN             string `json:"asn,omitempty"`
	Org             string `json:"org,omitempty"`
	IPv6            string `json:"ipv6,omitempty"`
	IPv6Country     string `json:"ipv6Country,omitempty"`
	IPv6Emoji       string `json:"ipv6Emoji,omitempty"`
	IPv6Available   bool   `json:"ipv6Available"`
	IPv6LeakWarning string `json:"ipv6LeakWarning,omitempty"`
	CFColo          string `json:"cfColo,omitempty"`
	WARP            string `json:"warp,omitempty"`
	ProxyLabel      string `json:"proxyLabel"`
	CheckedAt       string `json:"checkedAt"`
	Error           string `json:"error,omitempty"`
}

type ipifyResp struct {
	IP string `json:"ip"`
}

type ipwhoResp struct {
	Success     bool   `json:"success"`
	IP          string `json:"ip"`
	CountryCode string `json:"country_code"`
	City        string `json:"city"`
	Connection  struct {
		ASN int    `json:"asn"`
		Org string `json:"org"`
		ISP string `json:"isp"`
	} `json:"connection"`
}

func fetchBody(ctx context.Context, client *http.Client, targetURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", err
	}
	ApplyBrowserHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// FetchEgress inspects the current outbound IPv4, IPv6, GeoIP, ISP/ASN, and Cloudflare trace data.
func FetchEgress(ctx context.Context, cfg ProxyConfig) EgressInfo {
	info := EgressInfo{
		CheckedAt: FormatNow(),
	}
	_, label, err := ResolveProxy(cfg)
	if err != nil {
		info.Error = err.Error()
		info.ProxyLabel = "代理配置无效"
		return info
	}
	info.ProxyLabel = label

	client, err := NewHTTPClient(cfg, 6*time.Second, false)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	v6Client, err := NewHTTPClient(cfg, 3*time.Second, false)
	if err != nil {
		v6Client = client
	}

	var (
		wg        sync.WaitGroup
		v4IP      string
		v6IP      string
		geo       ipwhoResp
		geoOK     bool
		cfTrace   map[string]string
		v4Err     error
	)

	wg.Add(4)
	go func() {
		defer wg.Done()
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		body, err := fetchBody(c, client, "https://api4.ipify.org?format=json")
		if err != nil {
			v4Err = err
			return
		}
		var r ipifyResp
		if json.Unmarshal([]byte(body), &r) == nil {
			v4IP = strings.TrimSpace(r.IP)
		}
	}()

	go func() {
		defer wg.Done()
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		body, err := fetchBody(c, v6Client, "https://api6.ipify.org?format=json")
		if err != nil {
			return
		}
		var r ipifyResp
		if json.Unmarshal([]byte(body), &r) == nil {
			ip := strings.TrimSpace(r.IP)
			if strings.Contains(ip, ":") {
				v6IP = ip
			}
		}
	}()

	go func() {
		defer wg.Done()
		c, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		body, err := fetchBody(c, client, "https://ipwho.is/")
		if err != nil {
			return
		}
		var r ipwhoResp
		if json.Unmarshal([]byte(body), &r) == nil && r.Success {
			geo = r
			geoOK = true
		}
	}()

	go func() {
		defer wg.Done()
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		body, err := fetchBody(c, client, "https://chat.openai.com/cdn-cgi/trace")
		if err != nil {
			return
		}
		cfTrace = ParseCFTrace(body)
	}()

	wg.Wait()

	if v4IP != "" {
		info.IPv4 = v4IP
	} else if geoOK && geo.IP != "" && !strings.Contains(geo.IP, ":") {
		info.IPv4 = geo.IP
	} else if cfIP := cfTrace["ip"]; cfIP != "" && !strings.Contains(cfIP, ":") {
		info.IPv4 = cfIP
	}

	if geoOK {
		info.IPv4Country = strings.ToUpper(geo.CountryCode)
		info.IPv4City = geo.City
		info.ISP = geo.Connection.ISP
		info.Org = geo.Connection.Org
		if geo.Connection.ASN > 0 {
			info.ASN = fmt.Sprintf("AS%d", geo.Connection.ASN)
		}
	}
	if info.IPv4Country == "" && cfTrace["loc"] != "" {
		info.IPv4Country = strings.ToUpper(cfTrace["loc"])
	}
	info.IPv4Emoji = CountryEmoji(info.IPv4Country)
	info.CFColo = cfTrace["colo"]
	info.WARP = cfTrace["warp"]

	if v6IP != "" {
		info.IPv6 = v6IP
		info.IPv6Available = true
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		body, err := fetchBody(c, client, "https://ipwho.is/"+v6IP)
		cancel()
		if err == nil {
			var v6Geo ipwhoResp
			if json.Unmarshal([]byte(body), &v6Geo) == nil && v6Geo.Success {
				info.IPv6Country = strings.ToUpper(v6Geo.CountryCode)
				info.IPv6Emoji = CountryEmoji(info.IPv6Country)
			}
		}
		if info.IPv6Country == "" && strings.Contains(cfTrace["ip"], ":") && cfTrace["loc"] != "" {
			info.IPv6Country = strings.ToUpper(cfTrace["loc"])
			info.IPv6Emoji = CountryEmoji(info.IPv6Country)
		}
		if info.IPv6Country != "" && (IsBlockedAIRegion(info.IPv6Country) || (info.IPv4Country != "" && info.IPv6Country != info.IPv4Country)) {
			info.IPv6LeakWarning = fmt.Sprintf(
				"检测到 IPv6 出口 (%s) 与 IPv4 (%s) 不一致或位于受限地区，可能触发 Antigravity/Gemini 地区报错",
				info.IPv6Country, info.IPv4Country,
			)
		}
	}

	if info.IPv4 == "" && !info.IPv6Available && len(cfTrace) == 0 {
		if v4Err != nil {
			info.Error = fmt.Sprintf("出口探测失败: %v", v4Err)
		} else {
			info.Error = "出口探测失败，请检查代理或网络连通性"
		}
	}

	return info
}
