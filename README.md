# AI Speed

轻量级代理节点专项体检工具（纯 Go 1.24 零第三方依赖单二进制 + 内嵌 Web UI + CLI），围绕 **ChatGPT / GPT、Google Gemini、Google Antigravity（反重力）、YouTube** 四个目标，专注提供三大核心能力：

1. **网络延迟测试**：在代理链路上实测 TCP 建连、TLS 握手、TTFB 首字节耗时、抖动（Jitter）与连续采样成功率。
2. **解锁 / 可用性测试**：
   - **ChatGPT / GPT**：联合检测 `cdn-cgi/trace`、Web 合规接口、`ios.chat.openai.com` 客户端/ISP 风控与 `/v1/models` API 路由，区分完整支持与风控受限。
   - **Google Gemini**：模拟标准桌面浏览器请求头提取 Google 识别地区，支持可选填入 Gemini API Key 实测 `generativelanguage.googleapis.com` 是否存在「网页可用但 API 地区被拒」。
   - **Antigravity（反重力）**：分层检测 `antigravity.google`、`cloudcode-pa.googleapis.com`、`daily-cloudcode-pa.googleapis.com` 连通性 + Google 地区与 **IPv6 跨区泄露风险**（坦诚标注：未登录状态下仅验证网络链路与出口地区，最终资格仍受 Google 账号归属地影响）。
   - **YouTube**：检测是否「送中」（`google.cn`）、YouTube Premium 地区与 `redirector.googlevideo.com` 命中的 GGC 视频缓存节点。
3. **网速测速**：实测 Google 全球 CDN（对应 Gemini / 反重力 / YouTube）与 Cloudflare 边缘（对应 ChatGPT/GPT）下行/上行吞吐；若 Cloudflare TLS 握手失败则如实报错并自动切换国际备用链路，支持实时速率波形图与可选 LLM `TTFT` / `Tokens/s` 实测。

## 快速开始

### 启动 Web 界面（默认）

```bash
go run .
# 或编译为单个可执行文件
go build -o ai-speed .
./ai-speed
```

启动后默认监听 `http://127.0.0.1:8787` 并自动打开浏览器（传 `-open=false` 可禁止自动打开）。

### 命令行 CLI 模式

```bash
# 运行全部三项测试
./ai-speed -cli

# 仅运行某一项（latency / unlock / speed）
./ai-speed -cli -mode latency
./ai-speed -cli -mode unlock
./ai-speed -cli -mode speed -download-mb 10

# 指定自定义代理（HTTP 或 SOCKS5）
./ai-speed -cli -proxy socks5://127.0.0.1:7890

# 直连 / Clash TUN 模式
./ai-speed -cli -direct

# 输出 JSON
./ai-speed -cli -mode unlock -json
```

## 联动 Clash Verge Rev（可选）

在 Clash Verge Rev 中开启 **设置 -> 外部控制**（常见默认地址 `http://127.0.0.1:9097`，可在界面右上角「代理与 Clash 设置」或启动参数传入）：

```bash
./ai-speed -clash-api http://127.0.0.1:9097 -clash-secret "你的密钥"
```

连接后可直接在页面顶部查看当前节点、协议类型、Clash 延迟，并切换代理组/节点后一键测试。

## 灵感与参考项目

- [clash-verge-rev/clash-verge-rev](https://github.com/clash-verge-rev/clash-verge-rev)（`crates/clash-verge-media-unlock` 解锁架构与卡片交互）
- [beck-8/subs-check](https://github.com/beck-8/subs-check)（OpenAI Web/App 联合判定、YouTube 送中检测与流量上限控制）
- [oneclickvirt/UnlockTests](https://github.com/oneclickvirt/UnlockTests)（YouTube GGC 缓存节点探测）
- [faceair/clash-speedtest](https://github.com/faceair/clash-speedtest) / [zhsama/clash-speedtest](https://github.com/zhsama/clash-speedtest)
