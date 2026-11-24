package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-speed/internal/clash"
	"ai-speed/internal/netutil"
)

func TestServerStaticAndSecurityHeaders(t *testing.T) {
	srv := New(Config{
		DefaultProxy:      netutil.ProxyConfig{Mode: "direct"},
		DefaultDownloadMB: 10,
		DefaultUploadMB:   2,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("missing CSP header")
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing X-Content-Type-Options: nosniff")
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing X-Frame-Options: DENY")
	}

	// Verify /theme-init.js returns 200 with nosniff
	themeResp, err := http.Get(ts.URL + "/theme-init.js")
	if err != nil {
		t.Fatalf("GET /theme-init.js failed: %v", err)
	}
	themeResp.Body.Close()
	if themeResp.StatusCode != http.StatusOK || themeResp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unexpected /theme-init.js response: status=%d nosniff=%q", themeResp.StatusCode, themeResp.Header.Get("X-Content-Type-Options"))
	}

	// Verify unknown static path returns 404
	notFoundResp, err := http.Get(ts.URL + "/nonexistent.js")
	if err != nil {
		t.Fatalf("GET nonexistent failed: %v", err)
	}
	notFoundResp.Body.Close()
	if notFoundResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown path, got %d", notFoundResp.StatusCode)
	}

	// Verify /api/config
	cfgResp, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config failed: %v", err)
	}
	defer cfgResp.Body.Close()
	var cfgPayload map[string]any
	if err := json.NewDecoder(cfgResp.Body).Decode(&cfgPayload); err != nil {
		t.Fatalf("decode /api/config failed: %v", err)
	}
	if cfgPayload["downloadMb"] != float64(10) {
		t.Fatalf("unexpected downloadMb: %v", cfgPayload["downloadMb"])
	}
}

func TestServerClashProxyIntegration(t *testing.T) {
	mockClash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer top-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/version":
			_, _ = w.Write([]byte(`{"version":"v1.19.0"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/proxies":
			_, _ = w.Write([]byte(`{
				"proxies": {
					"节点选择": {"name":"节点选择","type":"Selector","now":"新加坡01","all":["新加坡01","美国01"]},
					"新加坡01": {"name":"新加坡01","type":"Vless","udp":true,"history":[{"delay":182}]},
					"美国01": {"name":"美国01","type":"Vless","udp":true,"history":[{"delay":240}]}
				}
			}`))
		case r.Method == http.MethodPut && r.URL.Path == "/proxies/%E8%8A%82%E7%82%B9%E9%80%89%E6%8B%A9":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockClash.Close()

	srv := New(Config{
		DefaultProxy: netutil.ProxyConfig{Mode: "direct"},
		DefaultClash: clash.Config{APIURL: mockClash.URL, Secret: "top-secret"},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/clash/proxies", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /api/clash/proxies failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from clash proxies, got %d", resp.StatusCode)
	}
	var proxiesResp clash.ProxiesResponse
	if err := json.NewDecoder(resp.Body).Decode(&proxiesResp); err != nil {
		t.Fatalf("decode proxiesResp failed: %v", err)
	}
	if proxiesResp.Version != "v1.19.0" || len(proxiesResp.Groups) != 1 || proxiesResp.Groups[0].DelayMs != 182 {
		t.Fatalf("unexpected Clash proxies response: %+v", proxiesResp)
	}
}
