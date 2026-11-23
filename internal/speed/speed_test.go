package speed

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"ai-speed/internal/netutil"
)

func TestSpeedEngineWithMockServersAndFallback(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 512*1024) // 512 KB

	var eventsCount atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/google-cdn", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = w.Write(payload)
	})
	mux.HandleFunc("/cf-fail", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/fallback-ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload)
	})
	mux.HandleFunc("/cf-up", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/ggc", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("127.0.0.1 => nrt01-isp (127.0.0.0/24)\n"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ep := Endpoints{
		GoogleCDNURL:      srv.URL + "/google-cdn",
		YouTubeGGCURL:     srv.URL + "/ggc",
		CloudflareDownURL: srv.URL + "/cf-fail",
		CloudflareUpURL:   srv.URL + "/cf-up",
		FallbackMirrors: []Mirror{
			{Name: "测试备用镜像", URL: srv.URL + "/fallback-ok"},
		},
		TargetProbeURLs: map[string]string{
			"chatgpt":     srv.URL + "/probe",
			"gemini":      srv.URL + "/probe",
			"antigravity": srv.URL + "/probe",
			"youtube":     srv.URL + "/probe",
		},
		MinValidBytes: 1024,
	}

	opts := Options{
		Proxy:      netutil.ProxyConfig{Mode: "direct"},
		DownloadMB: 1,
		UploadMB:   1,
		TimeoutSec: 10,
	}

	report, err := RunWithEndpoints(context.Background(), opts, ep, func(ev ProgressEvent) {
		eventsCount.Add(1)
	})
	if err != nil {
		t.Fatalf("RunWithEndpoints failed: %v", err)
	}
	if eventsCount.Load() == 0 {
		t.Fatal("expected progress events to be emitted")
	}
	if len(report.Links) != 4 {
		t.Fatalf("expected 4 links (google, cf fail, fallback ok, upload), got %d", len(report.Links))
	}

	googleLink := report.Links[0]
	if !googleLink.OK || googleLink.Bytes > 1024*1024 || googleLink.AvgMbps <= 0 {
		t.Fatalf("unexpected Google CDN link result: %+v", googleLink)
	}

	cfLink := report.Links[1]
	if cfLink.OK || cfLink.Error == "" {
		t.Fatalf("expected Cloudflare link to honestly record failure, got %+v", cfLink)
	}

	fbLink := report.Links[2]
	if !fbLink.OK || !fbLink.IsFallback || fbLink.AvgMbps <= 0 {
		t.Fatalf("expected fallback mirror to succeed, got %+v", fbLink)
	}

	upLink := report.Links[3]
	if !upLink.OK || upLink.Bytes != 1024*1024 || upLink.AvgMbps <= 0 {
		t.Fatalf("unexpected upload link result: %+v", upLink)
	}

	if len(report.Targets) != 4 {
		t.Fatalf("expected 4 targets, got %d", len(report.Targets))
	}
	if report.Targets[0].ID != "chatgpt" || report.Targets[0].DownloadMbps <= 0 || report.Targets[0].UploadMbps <= 0 {
		t.Fatalf("expected ChatGPT summary populated from fallback + upload, got %+v", report.Targets[0])
	}
	if report.Targets[3].ID != "youtube" || report.Targets[3].GGCNode != "nrt01-isp" {
		t.Fatalf("expected YouTube summary with GGC node nrt01-isp, got %+v", report.Targets[3])
	}
}
