(() => {
  "use strict";

  const SVG_NS = "http://www.w3.org/2000/svg";

  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v === null || v === undefined || v === false) continue;
        if (k === "className") {
          node.className = String(v);
        } else if (k === "textContent") {
          node.textContent = String(v);
        } else if (k.startsWith("on") && typeof v === "function") {
          node.addEventListener(k.slice(2).toLowerCase(), v);
        } else {
          node.setAttribute(k, v === true ? "" : String(v));
        }
      }
    }
    for (const child of children) {
      if (child === null || child === undefined || child === false) continue;
      if (typeof child === "string" || typeof child === "number") {
        node.appendChild(document.createTextNode(String(child)));
      } else {
        node.appendChild(child);
      }
    }
    return node;
  }

  function refreshSvg() {
    const svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("viewBox", "0 0 24 24");
    svg.setAttribute("width", "15");
    svg.setAttribute("height", "15");
    svg.setAttribute("fill", "none");
    svg.setAttribute("stroke", "currentColor");
    svg.setAttribute("stroke-width", "2");
    svg.setAttribute("stroke-linecap", "round");
    svg.setAttribute("stroke-linejoin", "round");

    const p1 = document.createElementNS(SVG_NS, "path");
    p1.setAttribute("d", "M21 12a9 9 0 1 1-2.64-6.36");
    const p2 = document.createElementNS(SVG_NS, "polyline");
    p2.setAttribute("points", "21 3 21 9 15 9");
    svg.appendChild(p1);
    svg.appendChild(p2);
    return svg;
  }

  const state = {
    clash: null,
    selectedGroup: "",
    latencyMap: new Map(),
    unlockMap: new Map(),
    speedAbort: null,
    chartPoints: [],
    allRunning: false,
  };

  const LATENCY_META = [
    { id: "baseline", name: "基准连通 (Google 204)", url: "https://www.gstatic.com/generate_204" },
    { id: "chatgpt", name: "ChatGPT / OpenAI", url: "https://api.openai.com/v1/models" },
    { id: "gemini", name: "Google Gemini", url: "https://gemini.google.com/" },
    { id: "antigravity", name: "Antigravity (反重力)", url: "https://cloudcode-pa.googleapis.com/" },
    { id: "youtube", name: "YouTube", url: "https://www.youtube.com/generate_204" },
  ];

  const UNLOCK_META = [
    { id: "chatgpt", name: "ChatGPT / GPT" },
    { id: "gemini", name: "Google Gemini" },
    { id: "antigravity", name: "Antigravity (反重力)" },
    { id: "youtube", name: "YouTube" },
  ];

  const SPEED_META = [
    { id: "chatgpt", name: "ChatGPT / GPT" },
    { id: "gemini", name: "Google Gemini" },
    { id: "antigravity", name: "Antigravity (反重力)" },
    { id: "youtube", name: "YouTube" },
  ];

  function getProxyConfig() {
    const mode = document.getElementById("cfg-proxy-mode").value;
    const proxyUrl = document.getElementById("cfg-proxy-url").value.trim();
    return { mode, proxyUrl };
  }

  function getClashConfig() {
    const apiUrl = document.getElementById("cfg-clash-url").value.trim() ||
      document.getElementById("quick-clash-url").value.trim();
    const secret = document.getElementById("cfg-clash-secret").value;
    return { apiUrl, secret };
  }

  function getAPIKeys() {
    return {
      openaiApiKey: document.getElementById("cfg-openai-key").value.trim(),
      geminiApiKey: document.getElementById("cfg-gemini-key").value.trim(),
    };
  }

  async function postJSON(url, body, signal) {
    const resp = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal,
    });
    const text = await resp.text();
    let parsed = null;
    try {
      parsed = JSON.parse(text);
    } catch {
      throw new Error(text || `HTTP ${resp.status}`);
    }
    if (!resp.ok) {
      throw new Error((parsed && parsed.error) || `HTTP ${resp.status}`);
    }
    return parsed;
  }

  function updateProxyBadge(label) {
    const badge = document.getElementById("proxy-badge");
    badge.textContent = label || "跟随系统代理";
  }

  function syncCustomProxyVisibility() {
    const mode = document.getElementById("cfg-proxy-mode").value;
    document.getElementById("custom-proxy-wrap").hidden = mode !== "custom";
  }

  function renderEgress(info) {
    const dot = document.getElementById("egress-dot");
    const box = document.getElementById("egress-body");
    box.replaceChildren();

    if (!info) {
      dot.className = "status-dot";
      box.appendChild(el("p", { className: "muted-text", textContent: "尚未检测" }));
      return;
    }

    if (info.proxyLabel) {
      updateProxyBadge(info.proxyLabel);
    }

    if (info.error && !info.ipv4 && !info.ipv6Available) {
      dot.className = "status-dot fail";
      box.appendChild(el("p", { className: "pill pill-fail", textContent: info.error }));
      return;
    }

    dot.className = info.ipv6LeakWarning ? "status-dot warn" : "status-dot ok";

    const regionText = [info.ipv4Emoji, info.ipv4Country, info.ipv4City].filter(Boolean).join(" ");
    box.appendChild(
      el("div", { className: "egress-main" },
        el("span", { className: "egress-ip", textContent: info.ipv4 || "未获取到 IPv4" }),
        regionText ? el("span", { className: "pill pill-accent", textContent: regionText }) : null
      )
    );

    const meta = el("div", { className: "meta-list" });
    if (info.isp) meta.appendChild(el("span", { textContent: `ISP: ${info.isp}` }));
    if (info.asn) meta.appendChild(el("span", { textContent: `ASN: ${info.asn}` }));
    if (info.cfColo) meta.appendChild(el("span", { textContent: `CF POP: ${info.cfColo}` }));
    if (info.warp && info.warp !== "off") meta.appendChild(el("span", { textContent: `WARP: ${info.warp}` }));
    meta.appendChild(
      el("span", {
        textContent: info.ipv6Available
          ? `IPv6: ${info.ipv6Country || "已启用"} (${info.ipv6})`
          : "IPv6: 未启用",
      })
    );
    if (info.checkedAt) meta.appendChild(el("span", { textContent: `检测于 ${info.checkedAt}` }));
    box.appendChild(meta);

    if (info.ipv6LeakWarning) {
      box.appendChild(el("div", { className: "warn-banner", textContent: `⚠️ ${info.ipv6LeakWarning}` }));
    }
  }

  function delayPillClass(ms) {
    if (!ms || ms <= 0) return "pill-neutral";
    if (ms < 180) return "pill-ok";
    if (ms < 380) return "pill-accent";
    if (ms < 650) return "pill-warn";
    return "pill-fail";
  }

  function renderClashPanel(data, errMsg) {
    const dot = document.getElementById("clash-dot");
    const verPill = document.getElementById("clash-version-pill");
    const body = document.getElementById("clash-body");
    body.replaceChildren();

    if (errMsg) {
      dot.className = "status-dot fail";
      verPill.hidden = true;
      body.appendChild(el("p", { className: "pill pill-fail", textContent: errMsg }));
      const retryWrap = el("div", { className: "clash-quick-row" });
      const inp = el("input", {
        type: "text",
        value: getClashConfig().apiUrl || "http://127.0.0.1:9097",
        "aria-label": "Clash API 地址",
      });
      const btn = el("button", {
        type: "button",
        className: "btn btn-secondary btn-sm",
        textContent: "重新连接",
        onClick: () => {
          document.getElementById("cfg-clash-url").value = inp.value.trim();
          document.getElementById("quick-clash-url").value = inp.value.trim();
          connectClash();
        },
      });
      retryWrap.append(inp, btn);
      body.appendChild(retryWrap);
      return;
    }

    if (!data || !Array.isArray(data.groups) || data.groups.length === 0) {
      dot.className = "status-dot";
      return;
    }

    dot.className = "status-dot ok";
    if (data.version) {
      verPill.textContent = `Core ${data.version}`;
      verPill.hidden = false;
    }

    const groups = data.groups;
    let activeGroup = groups.find((g) => g.name === state.selectedGroup) ||
      groups.find((g) => g.selectable) ||
      groups[0];
    state.selectedGroup = activeGroup.name;

    const banner = el("div", { className: "clash-node-banner" },
      el("div", null,
        el("div", { className: "clash-node-name", textContent: activeGroup.leafNode || activeGroup.now || activeGroup.name }),
        el("div", { className: "inline-actions" },
          activeGroup.leafType ? el("span", { className: "pill pill-neutral", textContent: activeGroup.leafType }) : null,
          activeGroup.udp ? el("span", { className: "pill pill-neutral", textContent: "UDP" }) : null,
          el("span", { className: "muted-text", textContent: `代理组: ${activeGroup.name}` })
        )
      ),
      el("span", {
        className: `pill tabular ${delayPillClass(activeGroup.delayMs)}`,
        textContent: activeGroup.delayMs > 0 ? `${activeGroup.delayMs} ms` : "未测",
      })
    );
    body.appendChild(banner);

    const groupSelect = el("select", {
      className: "clash-select",
      "aria-label": "选择代理组",
      onChange: (e) => {
        state.selectedGroup = e.target.value;
        renderClashPanel(state.clash);
      },
    });
    for (const g of groups) {
      const opt = el("option", { value: g.name, textContent: `${g.name} (${g.type})` });
      if (g.name === activeGroup.name) opt.selected = true;
      groupSelect.appendChild(opt);
    }

    const nodeSelect = el("select", {
      className: "clash-select",
      "aria-label": "选择节点",
      disabled: !activeGroup.selectable,
      onChange: async (e) => {
        const nextNode = e.target.value;
        try {
          const cfg = getClashConfig();
          const updated = await postJSON("/api/clash/select", {
            apiUrl: cfg.apiUrl,
            secret: cfg.secret,
            group: activeGroup.name,
            node: nextNode,
          });
          state.clash = updated;
          renderClashPanel(updated);
          refreshEgress();
        } catch (err) {
          alert(`切换节点失败: ${err.message}`);
        }
      },
    });
    for (const nodeName of activeGroup.all || []) {
      const meta = data.allNodes && data.allNodes[nodeName];
      const suffix = meta && meta.delayMs > 0 ? ` (${meta.delayMs}ms)` : "";
      const opt = el("option", { value: nodeName, textContent: `${nodeName}${suffix}` });
      if (nodeName === activeGroup.now) opt.selected = true;
      nodeSelect.appendChild(opt);
    }

    const testBtn = el("button", {
      type: "button",
      className: "btn btn-primary btn-sm",
      textContent: "测当前节点",
      onClick: () => runAllTests(),
    });

    body.appendChild(el("div", { className: "clash-controls" }, groupSelect, nodeSelect, testBtn));
  }

  function latencyRatingClass(rating) {
    switch (rating) {
      case "excellent":
        return "pill-ok";
      case "good":
        return "pill-accent";
      case "fair":
      case "poor":
        return "pill-warn";
      case "failed":
        return "pill-fail";
      default:
        return "pill-neutral";
    }
  }

  function renderLatencyGrid(loadingIds = new Set()) {
    const grid = document.getElementById("latency-grid");
    grid.replaceChildren();

    for (const meta of LATENCY_META) {
      const item = state.latencyMap.get(meta.id);
      const isLoading = loadingIds.has(meta.id);
      const btn = el("button", {
        type: "button",
        className: `icon-btn ${isLoading ? "spinning" : ""}`,
        title: `重测 ${meta.name}`,
        "aria-label": `重测 ${meta.name}`,
        disabled: isLoading,
        onClick: () => runLatency(meta.id),
      }, refreshSvg());

      let host = meta.url;
      try {
        host = new URL(meta.url).host;
      } catch {}

      const card = el("article", { className: "card latency-card" },
        el("div", { className: "latency-top" },
          el("div", null,
            el("div", { className: "latency-name", textContent: meta.name }),
            el("div", { className: "latency-host", textContent: host })
          ),
          btn
        )
      );

      if (isLoading) {
        card.appendChild(el("span", { className: "pill pill-neutral", textContent: "探测中..." }));
      } else if (!item) {
        card.appendChild(el("span", { className: "pill pill-neutral", textContent: "待测试" }));
      } else {
        const msText = item.avgMs > 0 ? `${item.avgMs} ms` : "失败";
        card.appendChild(
          el("div", { className: "latency-value-row" },
            el("span", { className: `latency-ms-badge ${latencyRatingClass(item.rating)}`, textContent: msText }),
            el("span", { className: "muted-text tabular", textContent: `成功率 ${item.successCount}/${item.totalCount}` })
          )
        );
        if (item.successCount > 0) {
          card.appendChild(
            el("div", { className: "latency-metrics" },
              el("span", { textContent: `最小/最大: ${item.minMs}/${item.maxMs} ms` }),
              el("span", { textContent: `抖动: ${item.jitterMs} ms` }),
              el("span", { textContent: `建连: ${item.avgConnectMs} ms` }),
              el("span", { textContent: `TLS: ${item.avgTlsMs} ms` })
            )
          );
        } else if (item.lastError) {
          card.appendChild(el("p", { className: "muted-text", textContent: item.lastError }));
        }
      }
      grid.appendChild(card);
    }
  }

  function unlockStatusClass(status) {
    switch (status) {
      case "yes":
        return "pill-ok";
      case "partial":
      case "risk":
        return "pill-warn";
      case "no":
      case "failed":
        return "pill-fail";
      default:
        return "pill-neutral";
    }
  }

  function subcheckPill(status) {
    switch (status) {
      case "ok":
        return { cls: "pill-ok", text: "通过" };
      case "warn":
        return { cls: "pill-warn", text: "提示" };
      case "fail":
        return { cls: "pill-fail", text: "拦截" };
      default:
        return { cls: "pill-neutral", text: "跳过" };
    }
  }

  function renderUnlockGrid(loadingIds = new Set()) {
    const grid = document.getElementById("unlock-grid");
    grid.replaceChildren();

    for (const meta of UNLOCK_META) {
      const item = state.unlockMap.get(meta.id);
      const isLoading = loadingIds.has(meta.id);
      const btn = el("button", {
        type: "button",
        className: `icon-btn ${isLoading ? "spinning" : ""}`,
        title: `重测 ${meta.name}`,
        "aria-label": `重测 ${meta.name}`,
        disabled: isLoading,
        onClick: () => runUnlock(meta.id),
      }, refreshSvg());

      const card = el("article", { className: "card unlock-card" },
        el("div", { className: "card-head" },
          el("h3", { textContent: meta.name }),
          btn
        )
      );

      if (isLoading) {
        card.appendChild(el("span", { className: "pill pill-neutral", textContent: "检测中..." }));
      } else if (!item) {
        card.appendChild(el("span", { className: "pill pill-neutral", textContent: "待检测" }));
      } else {
        const regionLabel = [item.emoji, item.region].filter(Boolean).join(" ");
        card.appendChild(
          el("div", { className: "unlock-badges" },
            el("div", { className: "unlock-badge-left" },
              el("span", { className: `pill ${unlockStatusClass(item.status)}`, textContent: item.statusText }),
              regionLabel ? el("span", { className: "pill pill-accent", textContent: regionLabel }) : null
            ),
            el("span", { className: "unlock-time", textContent: item.checkedAt || "" })
          )
        );
        card.appendChild(el("p", { className: "unlock-summary", textContent: item.summary }));

        const list = el("ul", { className: "subcheck-list" });
        for (const sub of item.subChecks || []) {
          const badge = subcheckPill(sub.status);
          list.appendChild(
            el("li", { className: "subcheck-item" },
              el("div", { className: "subcheck-left" },
                el("span", { className: "subcheck-label", textContent: sub.label }),
                el("span", { className: "subcheck-detail", textContent: sub.detail })
              ),
              el("div", { className: "inline-actions" },
                sub.latencyMs > 0 ? el("span", { className: "subcheck-ms", textContent: `${sub.latencyMs}ms` }) : null,
                el("span", { className: `pill ${badge.cls}`, textContent: badge.text })
              )
            )
          );
        }
        card.appendChild(list);
      }

      grid.appendChild(card);
    }
  }

  function renderSpeedChart(points) {
    const svg = document.getElementById("speed-chart");
    svg.replaceChildren();
    const w = 420;
    const h = 110;
    const pad = 8;

    for (const y of [28, 56, 84]) {
      const line = document.createElementNS(SVG_NS, "line");
      line.setAttribute("x1", "0");
      line.setAttribute("y1", String(y));
      line.setAttribute("x2", String(w));
      line.setAttribute("y2", String(y));
      line.setAttribute("stroke", "currentColor");
      line.setAttribute("stroke-opacity", "0.12");
      line.setAttribute("stroke-dasharray", "3 3");
      svg.appendChild(line);
    }

    if (!points || points.length === 0) return;

    const maxVal = Math.max(10, ...points) * 1.15;
    const coords = points.map((val, idx) => {
      const x = points.length === 1 ? w - pad : pad + (idx / (points.length - 1)) * (w - pad * 2);
      const y = h - pad - (Math.max(0, val) / maxVal) * (h - pad * 2);
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    });

    const areaPoints = `${pad},${h - pad} ${coords.join(" ")} ${w - pad},${h - pad}`;
    const polyArea = document.createElementNS(SVG_NS, "polygon");
    polyArea.setAttribute("points", areaPoints);
    polyArea.setAttribute("fill", "var(--accent)");
    polyArea.setAttribute("fill-opacity", "0.16");
    svg.appendChild(polyArea);

    const polyline = document.createElementNS(SVG_NS, "polyline");
    polyline.setAttribute("points", coords.join(" "));
    polyline.setAttribute("fill", "none");
    polyline.setAttribute("stroke", "var(--accent)");
    polyline.setAttribute("stroke-width", "2.5");
    polyline.setAttribute("stroke-linejoin", "round");
    polyline.setAttribute("stroke-linecap", "round");
    svg.appendChild(polyline);
  }

  function renderSpeedTargets(report) {
    const grid = document.getElementById("speed-targets-grid");
    const strip = document.getElementById("speed-links-strip");
    const banner = document.getElementById("speed-fallback-banner");
    grid.replaceChildren();
    strip.replaceChildren();
    banner.hidden = true;

    const map = new Map();
    if (report && Array.isArray(report.targets)) {
      for (const t of report.targets) map.set(t.id, t);
    }

    if (report && Array.isArray(report.links) && report.links.length > 0) {
      strip.hidden = false;
      for (const link of report.links) {
        if (link.isFallback && link.note) {
          banner.textContent = `ℹ️ ${link.note}`;
          banner.hidden = false;
        }
        strip.appendChild(
          el("div", { className: "link-chip" },
            el("div", { className: "inline-actions" },
              el("strong", { textContent: link.name }),
              el("span", {
                className: `pill ${link.ok ? "pill-ok" : "pill-fail"}`,
                textContent: link.ok ? `${link.avgMbps.toFixed(2)} Mbps` : "失败",
              })
            ),
            el("span", {
              className: "muted-text tabular",
              textContent: link.ok
                ? `峰值 ${link.peakMbps.toFixed(2)} Mbps · ${(link.bytes / 1048576).toFixed(2)} MB · TTFB ${link.ttfbMs}ms`
                : (link.error || "连接失败"),
            })
          )
        );
      }
    } else {
      strip.hidden = true;
    }

    for (const meta of SPEED_META) {
      const item = map.get(meta.id);
      const card = el("article", { className: "card speed-target-card" },
        el("div", { className: "card-head" },
          el("h3", { textContent: meta.name }),
          item ? el("span", { className: `pill ${item.downloadMbps > 0 ? "pill-accent" : "pill-fail"}`, textContent: item.qualityTag }) : el("span", { className: "pill pill-neutral", textContent: "待测速" })
        )
      );

      if (item) {
        card.appendChild(
          el("div", { className: "speed-number-row" },
            el("span", { className: "speed-big-stat", textContent: item.downloadMbps > 0 ? item.downloadMbps.toFixed(2) : "--" }),
            el("span", { className: "speed-unit", textContent: "Mbps 下行" })
          )
        );
        const metaRows = el("div", { className: "meta-list tabular" },
          el("span", { textContent: `链路: ${item.backboneLabel}` }),
          item.uploadMbps > 0 ? el("span", { textContent: `上行: ${item.uploadMbps.toFixed(2)} Mbps` }) : null,
          item.ttfbMs > 0 ? el("span", { textContent: `TTFB: ${item.ttfbMs} ms` }) : null,
          item.jitterMs > 0 ? el("span", { textContent: `抖动: ${item.jitterMs} ms` }) : null,
          item.ggcNode ? el("span", { textContent: `GGC 节点: ${item.ggcNode}` }) : null
        );
        card.appendChild(metaRows);

        if (item.tokenSpeed) {
          const ts = item.tokenSpeed;
          card.appendChild(
            el("div", { className: `pill ${ts.ok ? "pill-ok" : "pill-warn"}` },
              document.createTextNode(
                ts.ok
                  ? `${ts.model}: TTFT ${ts.ttftMs}ms · ${ts.tokensPerSec.toFixed(1)} tok/s`
                  : `${ts.model}: ${ts.error}`
              )
            )
          );
        }
        if (item.note) {
          card.appendChild(el("p", { className: "muted-text", textContent: item.note }));
        }
      }
      grid.appendChild(card);
    }
  }

  async function refreshEgress() {
    const btn = document.getElementById("btn-refresh-egress");
    btn.classList.add("spinning");
    try {
      const info = await postJSON("/api/egress", { proxy: getProxyConfig() });
      renderEgress(info);
    } catch (err) {
      renderEgress({ error: err.message });
    } finally {
      btn.classList.remove("spinning");
    }
  }

  async function connectClash() {
    const msg = document.getElementById("clash-connect-msg");
    const btn = document.getElementById("btn-refresh-clash");
    btn.classList.add("spinning");
    msg.textContent = "连接中...";
    try {
      const cfg = getClashConfig();
      const data = await postJSON("/api/clash/proxies", cfg);
      state.clash = data;
      renderClashPanel(data);
      msg.textContent = `已连接 (${data.version || "Mihomo"})`;
    } catch (err) {
      msg.textContent = err.message;
      renderClashPanel(null, err.message);
    } finally {
      btn.classList.remove("spinning");
    }
  }

  async function runLatency(singleId) {
    const ids = singleId ? [singleId] : LATENCY_META.map((x) => x.id);
    renderLatencyGrid(new Set(ids));
    try {
      const items = await postJSON("/api/latency", {
        proxy: getProxyConfig(),
        samples: 3,
        target: singleId || "",
      });
      for (const item of items) {
        state.latencyMap.set(item.id, item);
      }
    } catch (err) {
      for (const id of ids) {
        state.latencyMap.set(id, {
          id,
          rating: "failed",
          successCount: 0,
          totalCount: 3,
          lastError: err.message,
        });
      }
    }
    renderLatencyGrid(new Set());
  }

  async function runUnlock(singleId) {
    const ids = singleId ? [singleId] : UNLOCK_META.map((x) => x.id);
    renderUnlockGrid(new Set(ids));
    const keys = getAPIKeys();
    try {
      const items = await postJSON("/api/unlock", {
        proxy: getProxyConfig(),
        target: singleId || "",
        geminiApiKey: keys.geminiApiKey,
        openaiApiKey: keys.openaiApiKey,
      });
      for (const item of items) {
        state.unlockMap.set(item.id, item);
      }
    } catch (err) {
      for (const id of ids) {
        state.unlockMap.set(id, {
          id,
          status: "failed",
          statusText: "测试失败",
          summary: err.message,
          subChecks: [],
        });
      }
    }
    renderUnlockGrid(new Set());
  }

  function setSpeedButtonRunning(running) {
    const btn = document.getElementById("btn-run-speed");
    if (running) {
      btn.textContent = "停止测速";
      btn.className = "btn btn-danger btn-sm";
    } else {
      btn.textContent = "开始测速";
      btn.className = "btn btn-primary btn-sm";
    }
  }

  function handleSpeedEvent(ev) {
    const stageEl = document.getElementById("speed-stage-label");
    const liveEl = document.getElementById("speed-live-mbps");
    const peakEl = document.getElementById("speed-peak-mbps");
    const txEl = document.getElementById("speed-transferred");
    const fillEl = document.getElementById("speed-progress-fill");
    const lastAtEl = document.getElementById("speed-last-at");

    if (ev.type === "stage") {
      stageEl.textContent = ev.label || ev.stage;
      return;
    }
    if (ev.type === "sample") {
      const mbps = ev.currentMbps || ev.avgMbps || 0;
      liveEl.textContent = mbps.toFixed(2);
      peakEl.textContent = (ev.peakMbps || 0).toFixed(2);
      txEl.textContent = ((ev.downloaded || 0) / 1048576).toFixed(2);
      if (ev.totalBytes > 0) {
        const pct = Math.min(100, Math.round((ev.downloaded / ev.totalBytes) * 100));
        fillEl.style.width = `${pct}%`;
      }
      state.chartPoints.push(mbps);
      if (state.chartPoints.length > 60) state.chartPoints.shift();
      renderSpeedChart(state.chartPoints);
      return;
    }
    if (ev.type === "link_done") {
      liveEl.textContent = (ev.avgMbps || 0).toFixed(2);
      peakEl.textContent = (ev.peakMbps || 0).toFixed(2);
      txEl.textContent = ((ev.downloaded || 0) / 1048576).toFixed(2);
      fillEl.style.width = "100%";
      return;
    }
    if (ev.type === "done" && ev.report) {
      stageEl.textContent = "测速完成";
      fillEl.style.width = "100%";
      lastAtEl.textContent = `完成于 ${ev.report.checkedAt}`;
      renderSpeedTargets(ev.report);
      return;
    }
    if (ev.type === "error") {
      stageEl.textContent = `测速错误: ${ev.message || "未知错误"}`;
    }
  }

  async function runSpeed() {
    if (state.speedAbort) {
      state.speedAbort.abort();
      state.speedAbort = null;
      setSpeedButtonRunning(false);
      document.getElementById("speed-stage-label").textContent = "已停止测速";
      return;
    }

    const controller = new AbortController();
    state.speedAbort = controller;
    state.chartPoints = [];
    renderSpeedChart([]);
    document.getElementById("speed-progress-fill").style.width = "0%";
    document.getElementById("speed-live-mbps").textContent = "0.00";
    document.getElementById("speed-peak-mbps").textContent = "0.00";
    document.getElementById("speed-transferred").textContent = "0.0";
    setSpeedButtonRunning(true);

    const keys = getAPIKeys();
    const payload = {
      proxy: getProxyConfig(),
      downloadMb: Number(document.getElementById("cfg-download-mb").value) || 10,
      uploadMb: Number(document.getElementById("cfg-upload-mb").value) || 0,
      openaiApiKey: keys.openaiApiKey,
      geminiApiKey: keys.geminiApiKey,
    };

    try {
      const resp = await fetch("/api/speed/stream", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
        signal: controller.signal,
      });
      if (!resp.ok || !resp.body) {
        throw new Error(`HTTP ${resp.status}`);
      }

      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        const parts = buffer.split("\n\n");
        buffer = parts.pop() || "";
        for (const chunk of parts) {
          for (const line of chunk.split("\n")) {
            if (!line.startsWith("data:")) continue;
            const raw = line.slice(5).trim();
            if (!raw) continue;
            try {
              handleSpeedEvent(JSON.parse(raw));
            } catch {}
          }
        }
      }
    } catch (err) {
      if (err.name !== "AbortError") {
        document.getElementById("speed-stage-label").textContent = `测速失败: ${err.message}`;
      }
    } finally {
      state.speedAbort = null;
      setSpeedButtonRunning(false);
    }
  }

  async function runAllTests() {
    if (state.allRunning) return;
    state.allRunning = true;
    const btn = document.getElementById("btn-run-all");
    btn.disabled = true;
    btn.textContent = "测试进行中...";
    try {
      await Promise.all([refreshEgress(), runLatency(), runUnlock()]);
      await runSpeed();
    } finally {
      state.allRunning = false;
      btn.disabled = false;
      btn.textContent = "一键全部测试";
    }
  }

  function setupViewFilter() {
    const buttons = Array.from(document.querySelectorAll(".filter-btn"));
    const sections = {
      latency: document.getElementById("section-latency"),
      unlock: document.getElementById("section-unlock"),
      speed: document.getElementById("section-speed"),
    };
    for (const btn of buttons) {
      btn.addEventListener("click", () => {
        const view = btn.dataset.view;
        for (const b of buttons) {
          b.setAttribute("aria-pressed", String(b === btn));
        }
        for (const [k, sec] of Object.entries(sections)) {
          sec.hidden = view !== "all" && view !== k;
        }
      });
    }
  }

  function setupThemeToggle() {
    const root = document.documentElement;
    const btn = document.getElementById("btn-theme-toggle");
    btn.addEventListener("click", () => {
      const current = root.getAttribute("data-theme");
      const prefersDark = window.matchMedia("(prefers-color-scheme: dark)").matches;
      const isDarkNow = current ? current === "dark" : prefersDark;
      root.setAttribute("data-theme", isDarkNow ? "light" : "dark");
    });
  }

  async function init() {
    renderLatencyGrid();
    renderUnlockGrid();
    renderSpeedTargets(null);
    renderSpeedChart([]);
    setupViewFilter();
    setupThemeToggle();

    document.getElementById("cfg-proxy-mode").addEventListener("change", () => {
      syncCustomProxyVisibility();
    });

    document.getElementById("btn-toggle-settings").addEventListener("click", (e) => {
      const drawer = document.getElementById("settings-drawer");
      const nextHidden = !drawer.hidden;
      drawer.hidden = nextHidden;
      e.currentTarget.setAttribute("aria-expanded", String(!nextHidden));
    });

    document.getElementById("btn-refresh-egress").addEventListener("click", refreshEgress);
    document.getElementById("btn-connect-clash").addEventListener("click", connectClash);
    document.getElementById("btn-quick-clash").addEventListener("click", () => {
      document.getElementById("cfg-clash-url").value = document.getElementById("quick-clash-url").value.trim();
      connectClash();
    });
    document.getElementById("btn-refresh-clash").addEventListener("click", connectClash);
    document.getElementById("btn-run-latency").addEventListener("click", () => runLatency());
    document.getElementById("btn-run-unlock").addEventListener("click", () => runUnlock());
    document.getElementById("btn-run-speed").addEventListener("click", runSpeed);
    document.getElementById("btn-run-all").addEventListener("click", runAllTests);

    try {
      const res = await fetch("/api/config");
      if (res.ok) {
        const cfg = await res.json();
        if (cfg.proxy) {
          document.getElementById("cfg-proxy-mode").value = cfg.proxy.mode || "system";
          document.getElementById("cfg-proxy-url").value = cfg.proxy.proxyUrl || "";
        }
        if (cfg.proxyLabel) updateProxyBadge(cfg.proxyLabel);
        if (cfg.downloadMb) document.getElementById("cfg-download-mb").value = String(cfg.downloadMb);
        if (cfg.uploadMb !== undefined) document.getElementById("cfg-upload-mb").value = String(cfg.uploadMb);
        if (cfg.clashApiUrl) {
          document.getElementById("cfg-clash-url").value = cfg.clashApiUrl;
          document.getElementById("quick-clash-url").value = cfg.clashApiUrl;
        }
        syncCustomProxyVisibility();
        if (cfg.clashApiUrl) {
          connectClash();
        }
      }
    } catch {}

    syncCustomProxyVisibility();
    refreshEgress();
  }

  document.addEventListener("DOMContentLoaded", init);
})();
