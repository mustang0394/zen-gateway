// Package proxy 实现 Zen 免费模型中转网关。
//
// 核心改写逻辑（源自对上游判定的逆向结论，缺一即 403 FreeTierError）：
//  1. User-Agent            → 网关覆盖为 opencode/<latest>（每日从 GitHub 同步）
//  2. x-opencode-session    → 缺失时按 opencode ID 算法生成（ses_ + 12hex + 14base62）
//  3. x-opencode-request    → 缺失时生成 msg_ ID
//  4. x-opencode-client     → 缺失时补 "cli"
//  5. x-opencode-project    → 缺失时补 "global"
//  6. Authorization         → 缺失时补 "Bearer public"；key 决定走哪个代理
//  7. body.stream           → 强制 true
//  8. body.tools            → 缺少 bash/read 工具名时补齐（schema 可任意）
//  9. header 白名单          → 仅透传 Accept/Authorization/Content-Type/UA + x-opencode-*
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"

	"zengateway/internal/config"
	"zengateway/internal/idgen"
	"zengateway/internal/upstream"
	"zengateway/internal/version"
)

// 仅这些请求头会透传给上游；其余（含各 SDK 注入的 x-stainless-* 等）全部丢弃。
var passHeaders = map[string]bool{
	"accept":             true,
	"authorization":      true,
	"content-type":       true,
	"user-agent":         true, // 会随后被网关覆盖
	"x-opencode-client":  true,
	"x-opencode-project": true,
	"x-opencode-request": true,
	"x-opencode-session": true,
}

const (
	defaultKey     = "public"
	defaultClient  = "cli"
	defaultProject = "global"
)

// allowedBodyTools 是上游判定必需的工具名集合。
var requiredTools = map[string]bool{"bash": false, "read": false}

// Gateway 聚合配置与按 key 缓存的代理客户端。
type Gateway struct {
	cfg      *config.File
	clients  *clientCache
	log      *slog.Logger
	upstream string // 上游基址
}

// New 创建网关。upstreamBase 一般为 https://opencode.ai/zen/v1。
func New(cfg *config.File, log *slog.Logger, upstreamBase string) *Gateway {
	return &Gateway{
		cfg:      cfg,
		clients:  newClientCache(),
		log:      log,
		upstream: upstreamBase,
	}
}

// ---- key→代理客户端缓存 -------------------------------------------------

type clientCache struct {
	direct *http.Client
	m      map[string]*http.Client
}

func newClientCache() *clientCache {
	return &clientCache{m: map[string]*http.Client{}}
}

func (g *Gateway) clientFor(key string) (*http.Client, error) {
	proxyURL := g.cfg.ProxyFor(key)
	if proxyURL == "" {
		if g.clients.direct == nil {
			c, err := upstream.ClientFor("")
			if err != nil {
				return nil, err
			}
			g.clients.direct = c
		}
		return g.clients.direct, nil
	}
	if c, ok := g.clients.m[key]; ok {
		return c, nil
	}
	c, err := upstream.ClientFor(proxyURL)
	if err != nil {
		return nil, err
	}
	g.clients.m[key] = c
	g.log.Info("proxy route registered", "keyPrefix", keyPrefix(key), "proxy", proxyURL)
	return c, nil
}

// ---- HTTP handler --------------------------------------------------------

// ChatCompletions 处理 POST /v1/chat/completions（OpenAI chat 风格）。
func (g *Gateway) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	g.forward(w, r, "/chat/completions", rewriteChatBody)
}

// Responses 处理 POST /v1/responses（OpenAI responses 风格）。
func (g *Gateway) Responses(w http.ResponseWriter, r *http.Request) {
	g.forward(w, r, "/responses", rewriteResponsesBody)
}

type bodyRewriter func(body map[string]any)

func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, path string, rw bodyRewriter) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 保留数字精度，避免 int64 溢出变 float64
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return
	}

	key := extractKey(r.Header.Get("Authorization"))
	rewriteHeaders(r.Header, key)
	rw(body)

	newBody, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "re-encode body: "+err.Error(), http.StatusInternalServerError)
		return
	}

	client, err := g.clientFor(key)
	if err != nil {
		http.Error(w, "proxy client: "+err.Error(), http.StatusBadGateway)
		return
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, g.upstream+path, bytes.NewReader(newBody))
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	copyWhitelistedHeaders(upstreamReq.Header, r.Header)

	g.log.Debug("forwarding", "path", path, "keyPrefix", keyPrefix(key),
		"session", upstreamReq.Header.Get("x-opencode-session"))

	resp, err := client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "upstream request: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 透传响应头（只保留安全子集）+ 状态码
	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "content-length" || lk == "connection" || lk == "transfer-encoding" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// SSE 流式逐块搬运；非流式（如上游错误 JSON）走同一通道一次写完
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 客户端断开
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				g.log.Warn("upstream stream error", "err", err)
			}
			return
		}
	}
}

// ---- 请求头改写 ----------------------------------------------------------

func extractKey(authz string) string {
	authz = strings.TrimSpace(authz)
	if authz == "" {
		return defaultKey
	}
	parts := strings.SplitN(authz, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return strings.TrimSpace(parts[1])
	}
	return defaultKey
}

// rewriteHeaders 原地清空并按白名单重建；UA 强制网关值，各 x-opencode-* 补默认。
func rewriteHeaders(h http.Header, key string) {
	orig := h.Clone()
	for k := range h {
		delete(h, k)
	}

	// Authorization：缺失补 Bearer public（key 同时已用于代理路由）
	authz := orig.Get("Authorization")
	if authz == "" {
		authz = "Bearer " + defaultKey
	}
	h.Set("Authorization", authz)

	// Content-Type
	ct := orig.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	h.Set("Content-Type", ct)

	// Accept：客户端没传默认 */*
	h.Set("Accept", orDefault(orig.Get("Accept"), "*/*"))

	// User-Agent：不信任下游，网关强制覆盖
	h.Set("User-Agent", version.UserAgent())

	// x-opencode-* 四件套
	h.Set("x-opencode-client", orDefault(orig.Get("x-opencode-client"), defaultClient))
	h.Set("x-opencode-project", orDefault(orig.Get("x-opencode-project"), defaultProject))
	h.Set("x-opencode-request", orDefault(orig.Get("x-opencode-request"), idgen.Message()))
	h.Set("x-opencode-session", orDefault(orig.Get("x-opencode-session"), idgen.Session()))
}

func copyWhitelistedHeaders(dst, src http.Header) {
	for k := range dst {
		delete(dst, k)
	}
	for k, vs := range src {
		if passHeaders[strings.ToLower(k)] {
			dst[k] = vs
		}
	}
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func keyPrefix(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8] + "..."
}

// ---- 请求体改写 ----------------------------------------------------------

// rewriteChatBody 强制 stream:true 并补齐 bash+read 工具。
func rewriteChatBody(body map[string]any) {
	body["stream"] = true
	ensureTools(body)
}

// rewriteResponsesBody 处理 /responses 风格：stream 字段 + tools 结构不同
// （responses 的工具在顶层 tools 数组，function 定义平铺：{"type":"function","name":...}）。
func rewriteResponsesBody(body map[string]any) {
	body["stream"] = true
	ensureToolsResponses(body)
}

func ensureTools(body map[string]any) {
	raw, ok := body["tools"].([]any)
	if !ok {
		raw = nil
	}
	have := map[string]bool{}
	for _, t := range raw {
		if m, ok := t.(map[string]any); ok {
			if fn, ok := m["function"].(map[string]any); ok {
				if name, _ := fn["name"].(string); name != "" {
					have[name] = true
				}
			} else if name, _ := m["name"].(string); name != "" {
				have[name] = true
			}
		}
	}
	missing := missingTools(have)
	for _, name := range missing {
		raw = append(raw, stubChatTool(name))
	}
	if len(missing) > 0 {
		body["tools"] = raw
	}
}

func ensureToolsResponses(body map[string]any) {
	raw, ok := body["tools"].([]any)
	if !ok {
		raw = nil
	}
	have := map[string]bool{}
	for _, t := range raw {
		if m, ok := t.(map[string]any); ok {
			if name, _ := m["name"].(string); name != "" {
				have[name] = true
			}
		}
	}
	missing := missingTools(have)
	for _, name := range missing {
		raw = append(raw, stubResponsesTool(name))
	}
	if len(missing) > 0 {
		body["tools"] = raw
	}
}

func missingTools(have map[string]bool) []string {
	var out []string
	for name := range requiredTools {
		if !have[name] {
			out = append(out, name)
		}
	}
	return out
}

// stubChatTool 返回 OpenAI chat/completions 风格的最小工具定义。
func stubChatTool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": stubDescription(name),
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []any{},
			},
		},
	}
}

// stubResponsesTool 返回 OpenAI responses 风格的最小工具定义。
func stubResponsesTool(name string) map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        name,
		"description": stubDescription(name),
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []any{},
		},
	}
}

func stubDescription(name string) string {
	switch name {
	case "bash":
		return "Run a shell command."
	case "read":
		return "Read a file."
	default:
		return strings.ToUpper(name[:1]) + name[1:] + "."
	}
}

// compile-time 校验：bufio/httputil 在后续扩展（日志转储）时使用
var (
	_ = bufio.NewReader
	_ = httputil.DumpRequestOut
	_ = fmt.Sprintf
)
