package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"ai-speed/internal/clash"
	"ai-speed/internal/latency"
	"ai-speed/internal/netutil"
	"ai-speed/internal/server"
	"ai-speed/internal/speed"
	"ai-speed/internal/unlock"
)

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:8787", "Web 服务监听地址")
		proxyURL    = flag.String("proxy", "", "自定义代理地址（例如 http://127.0.0.1:7897 或 socks5://127.0.0.1:7890），留空则跟随系统代理")
		direct      = flag.Bool("direct", false, "直连 / Clash TUN 虚拟网卡模式（忽略代理配置）")
		clashAPI    = flag.String("clash-api", "", "可选 Clash Verge / Mihomo 外部控制 API 地址（例如 http://127.0.0.1:9097）")
		clashSecret = flag.String("clash-secret", "", "可选 Clash 外部控制 API Secret")
		downloadMB  = flag.Int("download-mb", 10, "单条测速链路最大下载流量（MB，1~50）")
		uploadMB    = flag.Int("upload-mb", 2, "上行测速流量（MB，0 表示关闭，最大 10）")
		openUI      = flag.Bool("open", true, "启动 Web 模式后自动打开浏览器")
		cliMode     = flag.Bool("cli", false, "在终端直接运行测试并退出（不启动 Web 服务）")
		mode        = flag.String("mode", "all", "CLI 测试模式: all | latency | unlock | speed")
		jsonOut     = flag.Bool("json", false, "CLI 模式下以 JSON 格式输出结果")
	)
	flag.Parse()

	proxyCfg := netutil.ProxyConfig{Mode: "system"}
	if *direct {
		proxyCfg.Mode = "direct"
	} else if strings.TrimSpace(*proxyURL) != "" {
		proxyCfg.Mode = "custom"
		proxyCfg.ProxyURL = strings.TrimSpace(*proxyURL)
	}

	if _, _, err := netutil.ResolveProxy(proxyCfg); err != nil {
		fmt.Fprintf(os.Stderr, "代理配置错误: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *cliMode {
		if err := runCLI(ctx, proxyCfg, *mode, *downloadMB, *uploadMB, *jsonOut); err != nil {
			fmt.Fprintf(os.Stderr, "执行失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	srv := server.New(server.Config{
		DefaultProxy: proxyCfg,
		DefaultClash: clash.Config{
			APIURL: strings.TrimSpace(*clashAPI),
			Secret: strings.TrimSpace(*clashSecret),
		},
		DefaultDownloadMB: *downloadMB,
		DefaultUploadMB:   *uploadMB,
	})

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "监听 %s 失败: %v\n", *addr, err)
		os.Exit(1)
	}

	urlStr := "http://" + ln.Addr().String()
	_, proxyLabel, _ := netutil.ResolveProxy(proxyCfg)
	fmt.Printf("AI Speed 已启动: %s\n", urlStr)
	fmt.Printf("当前代理模式: %s\n", proxyLabel)
	fmt.Println("按 Ctrl+C 退出服务")

	if *openUI {
		go func() {
			time.Sleep(150 * time.Millisecond)
			openBrowser(urlStr)
		}()
	}

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	case serveErr := <-errCh:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "HTTP 服务异常退出: %v\n", serveErr)
			os.Exit(1)
		}
	}
}

func runCLI(ctx context.Context, proxyCfg netutil.ProxyConfig, mode string, downloadMB, uploadMB int, jsonOut bool) error {
	m := strings.ToLower(strings.TrimSpace(mode))
	switch m {
	case "all", "latency", "unlock", "speed":
	default:
		return fmt.Errorf("未知的 -mode %q（支持 all | latency | unlock | speed）", mode)
	}

	type cliBundle struct {
		Egress  netutil.EgressInfo     `json:"egress"`
		Latency []latency.TargetResult `json:"latency,omitempty"`
		Unlock  []unlock.ItemResult    `json:"unlock,omitempty"`
		Speed   *speed.Report          `json:"speed,omitempty"`
	}

	out := cliBundle{
		Egress: netutil.FetchEgress(ctx, proxyCfg),
	}

	if !jsonOut {
		printEgress(out.Egress)
	}

	if m == "all" || m == "latency" {
		res, err := latency.TestAll(ctx, proxyCfg, 3)
		if err != nil {
			return err
		}
		out.Latency = res
		if !jsonOut {
			printLatency(res)
		}
	}

	if m == "all" || m == "unlock" {
		res, err := unlock.CheckAll(ctx, unlock.Options{Proxy: proxyCfg})
		if err != nil {
			return err
		}
		out.Unlock = res
		if !jsonOut {
			printUnlock(res)
		}
	}

	if m == "all" || m == "speed" {
		if !jsonOut {
			fmt.Println("\n=== 3. 网速测速 ===")
		}
		rep, err := speed.Run(ctx, speed.Options{
			Proxy:      proxyCfg,
			DownloadMB: downloadMB,
			UploadMB:   uploadMB,
		}, func(ev speed.ProgressEvent) {
			if jsonOut {
				return
			}
			switch ev.Type {
			case "stage":
				fmt.Printf("[阶段] %s: %s\n", ev.Label, ev.Message)
			case "link_done":
				if ev.Message != "" && ev.AvgMbps == 0 {
					fmt.Printf("  -> %s 失败: %s\n", ev.Label, ev.Message)
				} else {
					fmt.Printf("  -> %s 完成: 平均 %.2f Mbps (峰值 %.2f Mbps, %.2f MB)\n",
						ev.Label, ev.AvgMbps, ev.PeakMbps, float64(ev.Downloaded)/1048576.0)
				}
			}
		})
		if err != nil {
			return err
		}
		out.Speed = rep
		if !jsonOut {
			printSpeedTargets(rep)
		}
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	return nil
}

func printEgress(info netutil.EgressInfo) {
	fmt.Println("=== 出口网络 ===")
	fmt.Printf("代理: %s\n", info.ProxyLabel)
	if info.Error != "" && info.IPv4 == "" {
		fmt.Printf("状态: 异常 (%s)\n", info.Error)
		return
	}
	fmt.Printf("IPv4: %s  地区: %s %s %s  ISP: %s %s\n",
		info.IPv4, info.IPv4Emoji, info.IPv4Country, info.IPv4City, info.ISP, info.ASN)
	if info.IPv6Available {
		fmt.Printf("IPv6: %s (%s)\n", info.IPv6, info.IPv6Country)
	} else {
		fmt.Println("IPv6: 未启用")
	}
	if info.IPv6LeakWarning != "" {
		fmt.Printf("风险提示: %s\n", info.IPv6LeakWarning)
	}
}

func printLatency(items []latency.TargetResult) {
	fmt.Println("\n=== 1. 网络延迟测试 ===")
	for _, it := range items {
		if it.SuccessCount == 0 {
			fmt.Printf("- %-24s | 失败 (%s)\n", it.Name, it.LastError)
			continue
		}
		fmt.Printf("- %-24s | 平均 %4dms | 最小/最大 %d/%dms | 抖动 %dms | 成功 %d/%d\n",
			it.Name, it.AvgMs, it.MinMs, it.MaxMs, it.JitterMs, it.SuccessCount, it.TotalCount)
	}
}

func printUnlock(items []unlock.ItemResult) {
	fmt.Println("\n=== 2. 解锁 / 可用性测试 ===")
	for _, it := range items {
		region := strings.TrimSpace(it.Emoji + " " + it.Region)
		if region != "" {
			region = " [" + region + "]"
		}
		fmt.Printf("- %-22s | %-8s%s | %s\n", it.Name, it.StatusText, region, it.Summary)
		for _, sub := range it.SubChecks {
			fmt.Printf("    [%s] %s: %s\n", strings.ToUpper(sub.Status), sub.Label, sub.Detail)
		}
	}
}

func printSpeedTargets(rep *speed.Report) {
	fmt.Println("\n目标体验汇总:")
	for _, t := range rep.Targets {
		line := fmt.Sprintf("- %-22s | 下行 %.2f Mbps", t.Name, t.DownloadMbps)
		if t.UploadMbps > 0 {
			line += fmt.Sprintf(" | 上行 %.2f Mbps", t.UploadMbps)
		}
		if t.TTFBMs > 0 {
			line += fmt.Sprintf(" | TTFB %dms", t.TTFBMs)
		}
		if t.GGCNode != "" {
			line += fmt.Sprintf(" | GGC %s", t.GGCNode)
		}
		line += fmt.Sprintf(" | %s (%s)", t.QualityTag, t.BackboneLabel)
		fmt.Println(line)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	_ = cmd.Start()
}
