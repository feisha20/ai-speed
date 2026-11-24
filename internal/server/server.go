package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"ai-speed/internal/clash"
	"ai-speed/internal/latency"
	"ai-speed/internal/netutil"
	"ai-speed/internal/speed"
	"ai-speed/internal/unlock"
	"ai-speed/web"
)

// Config configures the HTTP server defaults.
type Config struct {
	DefaultProxy      netutil.ProxyConfig
	DefaultClash      clash.Config
	DefaultDownloadMB int
	DefaultUploadMB   int
}

// Server serves the embedded Web UI and JSON/SSE APIs.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// New constructs a Server with hardened HTTP headers and API routes.
func New(cfg Config) *Server {
	if cfg.DefaultProxy.Mode == "" {
		cfg.DefaultProxy.Mode = "system"
	}
	if cfg.DefaultDownloadMB <= 0 {
		cfg.DefaultDownloadMB = 10
	}
	if cfg.DefaultUploadMB < 0 {
		cfg.DefaultUploadMB = 0
	}
	s := &Server{
		cfg: cfg,
		mux: http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// Handler returns the root HTTP handler wrapped in security headers middleware.
func (s *Server) Handler() http.Handler {
	return securityHeaders(s.mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /", s.serveStatic("index.html", "text/html; charset=utf-8"))
	s.mux.HandleFunc("GET /style.css", s.serveStatic("style.css", "text/css; charset=utf-8"))
	s.mux.HandleFunc("GET /app.js", s.serveStatic("app.js", "application/javascript; charset=utf-8"))
	s.mux.HandleFunc("GET /theme-init.js", s.serveStatic("theme-init.js", "application/javascript; charset=utf-8"))

	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	s.mux.HandleFunc("POST /api/egress", s.handleEgress)
	s.mux.HandleFunc("POST /api/clash/proxies", s.handleClashProxies)
	s.mux.HandleFunc("POST /api/clash/select", s.handleClashSelect)
	s.mux.HandleFunc("POST /api/clash/delay", s.handleClashDelay)
	s.mux.HandleFunc("POST /api/latency", s.handleLatency)
	s.mux.HandleFunc("POST /api/unlock", s.handleUnlock)
	s.mux.HandleFunc("POST /api/speed/stream", s.handleSpeedStream)
}

func (s *Server) serveStatic(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if name == "index.html" && r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(web.Assets, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("请求参数解析失败: %v", err))
		return false
	}
	return true
}

func (s *Server) effectiveProxy(reqProxy *netutil.ProxyConfig) netutil.ProxyConfig {
	if reqProxy == nil || strings.TrimSpace(reqProxy.Mode) == "" {
		return s.cfg.DefaultProxy
	}
	return *reqProxy
}

func (s *Server) effectiveClash(apiURL, secret string) clash.Config {
	urlVal := strings.TrimSpace(apiURL)
	if urlVal == "" {
		urlVal = s.cfg.DefaultClash.APIURL
	}
	secVal := strings.TrimSpace(secret)
	if secVal == "" && urlVal == s.cfg.DefaultClash.APIURL {
		secVal = s.cfg.DefaultClash.Secret
	}
	return clash.Config{APIURL: urlVal, Secret: secVal}
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	_, label, _ := netutil.ResolveProxy(s.cfg.DefaultProxy)
	writeJSON(w, http.StatusOK, map[string]any{
		"proxy":          s.cfg.DefaultProxy,
		"proxyLabel":     label,
		"downloadMb":     s.cfg.DefaultDownloadMB,
		"uploadMb":       s.cfg.DefaultUploadMB,
		"clashApiUrl":    s.cfg.DefaultClash.APIURL,
		"hasClashSecret": s.cfg.DefaultClash.Secret != "",
	})
}

func (s *Server) handleEgress(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Proxy *netutil.ProxyConfig `json:"proxy"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	info := netutil.FetchEgress(r.Context(), s.effectiveProxy(req.Proxy))
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleClashProxies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIURL string `json:"apiUrl"`
		Secret string `json:"secret"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	client, err := clash.NewClient(s.effectiveClash(req.APIURL, req.Secret))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := client.GetProxies(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleClashSelect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIURL string `json:"apiUrl"`
		Secret string `json:"secret"`
		Group  string `json:"group"`
		Node   string `json:"node"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	client, err := clash.NewClient(s.effectiveClash(req.APIURL, req.Secret))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := client.SelectProxy(r.Context(), req.Group, req.Node); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	res, err := client.GetProxies(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleClashDelay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIURL string `json:"apiUrl"`
		Secret string `json:"secret"`
		Node   string `json:"node"`
		URL    string `json:"url"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	client, err := clash.NewClient(s.effectiveClash(req.APIURL, req.Secret))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	delay, err := client.TestNodeDelay(r.Context(), req.Node, req.URL, 5000)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"delay": delay})
}

func (s *Server) handleLatency(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Proxy   *netutil.ProxyConfig `json:"proxy"`
		Samples int                  `json:"samples"`
		Target  string               `json:"target"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	proxy := s.effectiveProxy(req.Proxy)
	if strings.TrimSpace(req.Target) != "" {
		item, err := latency.TestOne(r.Context(), proxy, strings.TrimSpace(req.Target), req.Samples)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, []latency.TargetResult{item})
		return
	}
	items, err := latency.TestAll(r.Context(), proxy, req.Samples)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Proxy        *netutil.ProxyConfig `json:"proxy"`
		Target       string               `json:"target"`
		GeminiAPIKey string               `json:"geminiApiKey"`
		OpenAIAPIKey string               `json:"openaiApiKey"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	opts := unlock.Options{
		Proxy:        s.effectiveProxy(req.Proxy),
		GeminiAPIKey: req.GeminiAPIKey,
		OpenAIAPIKey: req.OpenAIAPIKey,
	}
	if strings.TrimSpace(req.Target) != "" {
		item, err := unlock.CheckOne(r.Context(), opts, strings.TrimSpace(req.Target))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, []unlock.ItemResult{item})
		return
	}
	items, err := unlock.CheckAll(r.Context(), opts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleSpeedStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Proxy        *netutil.ProxyConfig `json:"proxy"`
		DownloadMB   int                  `json:"downloadMb"`
		UploadMB     int                  `json:"uploadMb"`
		OpenAIAPIKey string               `json:"openaiApiKey"`
		OpenAIModel  string               `json:"openaiModel"`
		GeminiAPIKey string               `json:"geminiApiKey"`
		GeminiModel  string               `json:"geminiModel"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	dlMB := req.DownloadMB
	if dlMB <= 0 {
		dlMB = s.cfg.DefaultDownloadMB
	}
	upMB := req.UploadMB
	if upMB < 0 {
		upMB = 0
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, _ := w.(http.Flusher)
	sendEvent := func(ev speed.ProgressEvent) {
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	opts := speed.Options{
		Proxy:        s.effectiveProxy(req.Proxy),
		DownloadMB:   dlMB,
		UploadMB:     upMB,
		OpenAIAPIKey: req.OpenAIAPIKey,
		OpenAIModel:  req.OpenAIModel,
		GeminiAPIKey: req.GeminiAPIKey,
		GeminiModel:  req.GeminiModel,
	}

	if _, err := speed.Run(r.Context(), opts, sendEvent); err != nil {
		sendEvent(speed.ProgressEvent{
			Type:    "error",
			Message: err.Error(),
		})
	}
}
