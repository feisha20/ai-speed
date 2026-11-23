package unlock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-speed/internal/netutil"
)

func TestUnlockCheckersMocked(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cdn-cgi/trace", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fl=1\nip=1.2.3.4\nloc=US\ncolo=SJC\nwarp=off\n"))
	})
	mux.HandleFunc("/compliance/cookie_requirements", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"cookie_consent_required":false}`))
	})
	mux.HandleFunc("/ios-ok", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/ios-vpn", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"access blocked due to VPN"}`))
	})
	mux.HandleFunc("/ios-dc", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"cf_details":"Request is not allowed. Please try again later.", "type":"dc"}`))
	})
	mux.HandleFunc("/openai-models-ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Missing bearer authentication"}}`))
	})
	mux.HandleFunc("/openai-models-blocked", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"unsupported_country"}}`))
	})
	mux.HandleFunc("/gemini-us", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><script>window.DATA=[1,2,1,200,"USA"];</script></html>`))
	})
	mux.HandleFunc("/gemini-hk", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><script>window.DATA=[1,2,1,200,"HKG"];</script></html>`))
	})
	mux.HandleFunc("/gemini-api-400", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") == "" {
			t.Error("expected x-goog-api-key header")
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}`))
	})
	mux.HandleFunc("/gemini-api-200", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	})
	mux.HandleFunc("/ag-web", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html>Antigravity</html>`))
	})
	mux.HandleFunc("/cloudcode", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/yt-us", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><script>var cfg={"INNERTUBE_CONTEXT_GL":"US"};</script>YouTube Premium ad-free</html>`))
	})
	mux.HandleFunc("/yt-cn", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><link href="https://www.google.cn"></html>`))
	})
	mux.HandleFunc("/yt-ggc", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("1.2.3.4 => sin01-isp (1.2.3.0/24)\n\nDebug Info:\nabc"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ep := Endpoints{
		CFTraceURL:        srv.URL + "/cdn-cgi/trace",
		OpenAICookieURL:   srv.URL + "/compliance/cookie_requirements",
		OpenAIIOSURL:      srv.URL + "/ios-ok",
		OpenAIModelsURL:   srv.URL + "/openai-models-ok",
		GeminiWebURL:      srv.URL + "/gemini-us",
		GeminiAPIURL:      srv.URL + "/gemini-api-200",
		AntigravityWebURL: srv.URL + "/ag-web",
		CloudCodeURL:      srv.URL + "/cloudcode",
		DailyCloudCodeURL: srv.URL + "/cloudcode",
		YouTubePremiumURL: srv.URL + "/yt-us",
		YouTubeGGCURL:     srv.URL + "/yt-ggc",
	}

	opts := Options{
		Proxy:        netutil.ProxyConfig{Mode: "direct"},
		GeminiAPIKey: "test-key",
	}
	egress := &netutil.EgressInfo{
		IPv4:        "1.2.3.4",
		IPv4Country: "US",
		ISP:         "Residential Fiber",
	}

	results, err := checkAllWith(context.Background(), opts, ep, egress)
	if err != nil {
		t.Fatalf("checkAllWith failed: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}
	for _, item := range results {
		if item.Status != "yes" {
			t.Fatalf("expected %s status=yes, got %s (%s)", item.ID, item.Status, item.Summary)
		}
	}
	if results[3].Region != "US" || !strings.Contains(results[3].Summary, "sin01-isp") {
		t.Fatalf("unexpected YouTube result: %+v", results[3])
	}

	// Verify ChatGPT partial when iOS VPN blocked
	epVPN := ep
	epVPN.OpenAIIOSURL = srv.URL + "/ios-vpn"
	gptVPN, _ := checkChatGPT(context.Background(), opts, epVPN)
	if gptVPN.Status != "partial" {
		t.Fatalf("expected ChatGPT partial on VPN block, got %s", gptVPN.Status)
	}

	// Verify ChatGPT partial when iOS returns 403 type:dc
	epDC := ep
	epDC.OpenAIIOSURL = srv.URL + "/ios-dc"
	gptDC, _ := checkChatGPT(context.Background(), opts, epDC)
	if gptDC.Status != "partial" || gptDC.SubChecks[2].Status != "fail" {
		t.Fatalf("expected ChatGPT partial with app subcheck fail on type:dc, got %+v", gptDC)
	}

	// Verify ChatGPT API unsupported_country
	epGPTBlock := ep
	epGPTBlock.OpenAIModelsURL = srv.URL + "/openai-models-blocked"
	gptBlock, _ := checkChatGPT(context.Background(), opts, epGPTBlock)
	if gptBlock.Status != "partial" {
		t.Fatalf("expected ChatGPT partial when web ok but API blocked, got %s", gptBlock.Status)
	}

	// Verify Gemini Web ok + API 400 location unsupported -> partial
	epGemAPI400 := ep
	epGemAPI400.GeminiAPIURL = srv.URL + "/gemini-api-400"
	gem400, _ := checkGemini(context.Background(), opts, epGemAPI400)
	if gem400.Status != "partial" {
		t.Fatalf("expected Gemini partial on API 400 geo deny, got %s", gem400.Status)
	}

	// Verify Gemini HK blocked
	epGemHK := ep
	epGemHK.GeminiWebURL = srv.URL + "/gemini-hk"
	gemHK, _ := checkGemini(context.Background(), Options{Proxy: opts.Proxy}, epGemHK)
	if gemHK.Status != "no" || gemHK.Region != "HK" {
		t.Fatalf("expected Gemini no/HK, got %+v", gemHK)
	}

	// Verify Antigravity IPv6 leak warning -> risk
	egressLeak := &netutil.EgressInfo{
		IPv4:            "1.2.3.4",
		IPv4Country:     "US",
		IPv6Available:   true,
		IPv6Country:     "HK",
		IPv6LeakWarning: "检测到 IPv6 出口 (HK) 与 IPv4 (US) 不一致",
	}
	agLeak, _ := checkAntigravity(context.Background(), Options{Proxy: opts.Proxy}, ep, egressLeak)
	if agLeak.Status != "risk" {
		t.Fatalf("expected Antigravity risk on IPv6 leak, got %s", agLeak.Status)
	}

	// Verify YouTube 送中
	epYTCN := ep
	epYTCN.YouTubePremiumURL = srv.URL + "/yt-cn"
	ytCN, _ := checkYouTube(context.Background(), opts, epYTCN)
	if ytCN.Status != "no" || ytCN.Region != "CN" {
		t.Fatalf("expected YouTube no/CN, got %+v", ytCN)
	}
}
