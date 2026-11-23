package clash

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"ai-speed/internal/netutil"
)

// Config holds connection settings for a Clash Verge / Mihomo External Controller API.
type Config struct {
	APIURL string `json:"apiUrl"`
	Secret string `json:"secret"`
}

// NodeMeta summarizes a Clash proxy node or policy group entry.
type NodeMeta struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	UDP     bool   `json:"udp"`
	DelayMs int    `json:"delayMs"`
}

// ProxyGroup represents a selectable or policy proxy group in Clash.
type ProxyGroup struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Now        string   `json:"now"`
	All        []string `json:"all"`
	Selectable bool     `json:"selectable"`
	LeafNode   string   `json:"leafNode"`
	LeafType   string   `json:"leafType"`
	UDP        bool     `json:"udp"`
	DelayMs    int      `json:"delayMs"`
}

// ProxiesResponse aggregates the Clash core version, groups, and node metadata.
type ProxiesResponse struct {
	Version  string              `json:"version"`
	Groups   []ProxyGroup        `json:"groups"`
	AllNodes map[string]NodeMeta `json:"allNodes"`
}

// Client talks directly to the local Clash / Mihomo REST controller.
type Client struct {
	baseURL string
	secret  string
	http    *http.Client
}

// NewClient validates Config and creates a direct local REST client.
func NewClient(cfg Config) (*Client, error) {
	raw := strings.TrimSpace(cfg.APIURL)
	if raw == "" {
		return nil, fmt.Errorf("Clash 控制 API 地址不能为空")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析 Clash API 地址失败: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("Clash API 仅支持 http/https 协议，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("Clash API 地址缺少主机和端口")
	}
	u.Scheme = scheme
	base := strings.TrimRight(u.String(), "/")
	return &Client{
		baseURL: base,
		secret:  strings.TrimSpace(cfg.Secret),
		http:    netutil.NewDirectClient(5 * time.Second),
	}, nil
}

func (c *Client) doRequest(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, 0, err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// GetVersion retrieves the Mihomo/Clash core version string.
func (c *Client) GetVersion(ctx context.Context) (string, error) {
	data, status, err := c.doRequest(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("GET /version 返回 HTTP %d: %s", status, strings.TrimSpace(string(data)))
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", err
	}
	return payload.Version, nil
}

type rawProxyItem struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Now     string   `json:"now"`
	All     []string `json:"all"`
	UDP     bool     `json:"udp"`
	History []struct {
		Delay int `json:"delay"`
	} `json:"history"`
}

func latestDelay(item rawProxyItem) int {
	if len(item.History) == 0 {
		return 0
	}
	return item.History[len(item.History)-1].Delay
}

func resolveLeaf(start string, rawMap map[string]rawProxyItem) (string, rawProxyItem) {
	curr := start
	visited := make(map[string]bool)
	for range 12 {
		if visited[curr] {
			break
		}
		visited[curr] = true
		item, ok := rawMap[curr]
		if !ok {
			break
		}
		if item.Now != "" && len(item.All) > 0 {
			curr = item.Now
			continue
		}
		return curr, item
	}
	return curr, rawMap[curr]
}

// GetProxies returns all policy groups and node metadata from Clash `/proxies`.
func (c *Client) GetProxies(ctx context.Context) (*ProxiesResponse, error) {
	version, _ := c.GetVersion(ctx)

	data, status, err := c.doRequest(ctx, http.MethodGet, "/proxies", nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("GET /proxies 返回 HTTP %d: %s", status, strings.TrimSpace(string(data)))
	}

	var raw struct {
		Proxies map[string]rawProxyItem `json:"proxies"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析 Clash /proxies 响应失败: %w", err)
	}

	allNodes := make(map[string]NodeMeta, len(raw.Proxies))
	var groups []ProxyGroup

	for name, item := range raw.Proxies {
		allNodes[name] = NodeMeta{
			Name:    name,
			Type:    item.Type,
			UDP:     item.UDP,
			DelayMs: latestDelay(item),
		}
		if len(item.All) == 0 {
			continue
		}
		switch item.Type {
		case "Selector", "URLTest", "Fallback", "LoadBalance":
			leafName, leafItem := resolveLeaf(item.Now, raw.Proxies)
			if leafName == "" {
				leafName = item.Now
			}
			groups = append(groups, ProxyGroup{
				Name:       name,
				Type:       item.Type,
				Now:        item.Now,
				All:        item.All,
				Selectable: item.Type == "Selector",
				LeafNode:   leafName,
				LeafType:   leafItem.Type,
				UDP:        leafItem.UDP,
				DelayMs:    latestDelay(leafItem),
			})
		}
	}

	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Selectable != groups[j].Selectable {
			return groups[i].Selectable
		}
		if groups[i].Name == "GLOBAL" && groups[j].Name != "GLOBAL" {
			return false
		}
		if groups[j].Name == "GLOBAL" && groups[i].Name != "GLOBAL" {
			return true
		}
		return groups[i].Name < groups[j].Name
	})

	return &ProxiesResponse{
		Version:  version,
		Groups:   groups,
		AllNodes: allNodes,
	}, nil
}

// SelectProxy switches the active node inside a Selector proxy group.
func (c *Client) SelectProxy(ctx context.Context, group, node string) error {
	if strings.TrimSpace(group) == "" || strings.TrimSpace(node) == "" {
		return fmt.Errorf("代理组与节点名称不能为空")
	}
	payload, err := json.Marshal(map[string]string{"name": node})
	if err != nil {
		return err
	}
	path := "/proxies/" + url.PathEscape(group)
	data, status, err := c.doRequest(ctx, http.MethodPut, path, payload)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("切换节点失败 HTTP %d: %s", status, strings.TrimSpace(string(data)))
	}
	return nil
}

// TestNodeDelay triggers Clash's built-in URL delay probe for a single node.
func (c *Client) TestNodeDelay(ctx context.Context, node, testURL string, timeoutMs int) (int, error) {
	if strings.TrimSpace(node) == "" {
		return 0, fmt.Errorf("节点名称不能为空")
	}
	if strings.TrimSpace(testURL) == "" {
		testURL = "http://www.gstatic.com/generate_204"
	}
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	q := url.Values{}
	q.Set("url", testURL)
	q.Set("timeout", fmt.Sprintf("%d", timeoutMs))
	path := "/proxies/" + url.PathEscape(node) + "/delay?" + q.Encode()

	data, status, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	if status < 200 || status >= 300 {
		return 0, fmt.Errorf("节点测延迟失败 HTTP %d: %s", status, strings.TrimSpace(string(data)))
	}
	var resp struct {
		Delay int `json:"delay"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Delay, nil
}
