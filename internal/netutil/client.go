package netutil

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ProxyConfig describes how outbound test HTTP clients should route traffic.
type ProxyConfig struct {
	Mode     string `json:"mode"`     // "system" | "custom" | "direct"
	ProxyURL string `json:"proxyUrl"` // Used when Mode is "custom"
}

// NormalizeProxyURL parses and validates a proxy URL, defaulting bare host:port inputs to http://.
func NormalizeProxyURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("代理地址不能为空")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://" + trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("解析代理地址失败: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
		u.Scheme = scheme
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q（仅支持 http / https / socks5 / socks5h）", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("代理地址缺少主机与端口")
	}
	return u, nil
}

// DetectMacSystemProxy reads macOS system proxy settings via `scutil --proxy` as a fallback
// when process environment variables (HTTP_PROXY/HTTPS_PROXY/ALL_PROXY) are unset.
func DetectMacSystemProxy() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "scutil", "--proxy").Output()
	if err != nil {
		return ""
	}
	kv := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:idx])
		v := strings.TrimSpace(line[idx+1:])
		kv[k] = v
	}
	if kv["HTTPSEnable"] == "1" && kv["HTTPSProxy"] != "" && kv["HTTPSPort"] != "" {
		return fmt.Sprintf("http://%s:%s", kv["HTTPSProxy"], kv["HTTPSPort"])
	}
	if kv["HTTPEnable"] == "1" && kv["HTTPProxy"] != "" && kv["HTTPPort"] != "" {
		return fmt.Sprintf("http://%s:%s", kv["HTTPProxy"], kv["HTTPPort"])
	}
	if kv["SOCKSEnable"] == "1" && kv["SOCKSProxy"] != "" && kv["SOCKSPort"] != "" {
		return fmt.Sprintf("socks5://%s:%s", kv["SOCKSProxy"], kv["SOCKSPort"])
	}
	return ""
}

func firstEnvProxy() string {
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

// ResolveProxy returns an http.Transport Proxy function and a human-readable proxy label.
func ResolveProxy(cfg ProxyConfig) (func(*http.Request) (*url.URL, error), string, error) {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" {
		mode = "system"
	}

	switch mode {
	case "direct":
		return nil, "直连 / TUN 模式", nil
	case "custom":
		u, err := NormalizeProxyURL(cfg.ProxyURL)
		if err != nil {
			return nil, "", err
		}
		return http.ProxyURL(u), fmt.Sprintf("自定义代理 (%s)", u.String()), nil
	case "system":
		if envProxy := firstEnvProxy(); envProxy != "" {
			if u, err := NormalizeProxyURL(envProxy); err == nil {
				return http.ProxyURL(u), fmt.Sprintf("系统代理 (%s)", u.String()), nil
			}
			return http.ProxyFromEnvironment, fmt.Sprintf("系统代理 (%s)", envProxy), nil
		}
		if macProxy := DetectMacSystemProxy(); macProxy != "" {
			if u, err := NormalizeProxyURL(macProxy); err == nil {
				return http.ProxyURL(u), fmt.Sprintf("macOS 系统代理 (%s)", u.String()), nil
			}
		}
		return http.ProxyFromEnvironment, "直连 / TUN（未检测到环境变量或系统代理）", nil
	default:
		return nil, "", fmt.Errorf("未知代理模式 %q", cfg.Mode)
	}
}

// NewHTTPClient builds an HTTP client routed according to ProxyConfig.
func NewHTTPClient(cfg ProxyConfig, timeout time.Duration, disableKeepAlive bool) (*http.Client, error) {
	proxyFunc, _, err := ResolveProxy(cfg)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{
		Timeout:   8 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		Proxy:                 proxyFunc,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     disableKeepAlive,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}, nil
}

// NewDirectClient creates an HTTP client that always bypasses proxies (used for local Clash API).
func NewDirectClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: 4 * time.Second,
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
	}
}

// ApplyBrowserHeaders applies realistic Chrome desktop headers to minimize anti-bot page downgrades.
func ApplyBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-CH-UA", `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	req.Header.Set("Sec-CH-UA-Platform", `"macOS"`)
}

// ParseCFTrace parses Cloudflare /cdn-cgi/trace key=value text output.
func ParseCFTrace(body string) map[string]string {
	res := make(map[string]string)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.IndexByte(line, '='); idx > 0 {
			res[line[:idx]] = line[idx+1:]
		}
	}
	return res
}

// CountryEmoji converts a 2-letter ISO-3166 alpha-2 code into a flag emoji.
func CountryEmoji(alpha2 string) string {
	code := strings.ToUpper(strings.TrimSpace(alpha2))
	if len(code) != 2 {
		return ""
	}
	if code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return ""
	}
	r1 := rune(0x1F1E6) + rune(code[0]-'A')
	r2 := rune(0x1F1E6) + rune(code[1]-'A')
	return string([]rune{r1, r2})
}

var alpha3Map = map[string]string{
	"AFG": "AF", "ALB": "AL", "DZA": "DZ", "ARG": "AR", "ARM": "AM",
	"AUS": "AU", "AUT": "AT", "AZE": "AZ", "BHR": "BH", "BGD": "BD",
	"BLR": "BY", "BEL": "BE", "BOL": "BO", "BIH": "BA", "BRA": "BR",
	"BRN": "BN", "BGR": "BG", "KHM": "KH", "CAN": "CA", "CHL": "CL",
	"CHN": "CN", "COL": "CO", "CRI": "CR", "HRV": "HR", "CUB": "CU",
	"CYP": "CY", "CZE": "CZ", "DNK": "DK", "DOM": "DO", "ECU": "EC",
	"EGY": "EG", "EST": "EE", "FIN": "FI", "FRA": "FR", "GEO": "GE",
	"DEU": "DE", "GHA": "GH", "GRC": "GR", "GTM": "GT", "HKG": "HK",
	"HUN": "HU", "ISL": "IS", "IND": "IN", "IDN": "ID", "IRN": "IR",
	"IRQ": "IQ", "IRL": "IE", "ISR": "IL", "ITA": "IT", "JPN": "JP",
	"JOR": "JO", "KAZ": "KZ", "KEN": "KE", "PRK": "KP", "KOR": "KR",
	"KWT": "KW", "LAO": "LA", "LVA": "LV", "LBN": "LB", "LTU": "LT",
	"LUX": "LU", "MAC": "MO", "MYS": "MY", "MDV": "MV", "MLT": "MT",
	"MEX": "MX", "MDA": "MD", "MNG": "MN", "MNE": "ME", "MAR": "MA",
	"MMR": "MM", "NPL": "NP", "NLD": "NL", "NZL": "NZ", "NGA": "NG",
	"MKD": "MK", "NOR": "NO", "OMN": "OM", "PAK": "PK", "PAN": "PA",
	"PRY": "PY", "PER": "PE", "PHL": "PH", "POL": "PL", "PRT": "PT",
	"PRI": "PR", "QAT": "QA", "ROU": "RO", "RUS": "RU", "SAU": "SA",
	"SRB": "RS", "SGP": "SG", "SVK": "SK", "SVN": "SI", "ZAF": "ZA",
	"ESP": "ES", "LKA": "LK", "SWE": "SE", "CHE": "CH", "SYR": "SY",
	"TWN": "TW", "THA": "TH", "TUN": "TN", "TUR": "TR", "UKR": "UA",
	"ARE": "AE", "GBR": "GB", "USA": "US", "URY": "UY", "UZB": "UZ",
	"VEN": "VE", "VNM": "VN",
}

// Alpha3ToAlpha2 maps an ISO-3166 3-letter country code to a 2-letter code.
func Alpha3ToAlpha2(alpha3 string) string {
	code := strings.ToUpper(strings.TrimSpace(alpha3))
	if len(code) == 2 {
		return code
	}
	if v, ok := alpha3Map[code]; ok {
		return v
	}
	return code
}

var blockedAlpha2Regions = map[string]bool{
	"CN": true, "HK": true, "MO": true,
	"RU": true, "BY": true, "CU": true,
	"IR": true, "KP": true, "SY": true,
}

// IsBlockedAIRegion returns true if the region is broadly blocked by OpenAI / Gemini / Antigravity.
func IsBlockedAIRegion(alpha2 string) bool {
	return blockedAlpha2Regions[strings.ToUpper(strings.TrimSpace(alpha2))]
}

// FormatNow returns the current local timestamp formatted consistently with desktop dashboards.
func FormatNow() string {
	return time.Now().Format("2006-01-02 15:04:05")
}
