// Package zen 实现 OpenCode Zen 模块的上游适配。
//
// 上游会校验请求是否来自 opencode 官方客户端，缺失任一要素即 403 FreeTierError：
//
//	User-Agent: opencode/<latest>      每日从 GitHub 同步，无条件覆盖下游值
//	x-opencode-session                 缺失时按 opencode ID 算法生成
//	x-opencode-request                 缺失时生成 msg_ ID
//	x-opencode-client                  缺失时补 cli
//	x-opencode-project                 缺失时补 global
//	Authorization                      缺失时补 Bearer public
//	body.stream                        强制 true
//	body.tools                         必须同时包含 bash 与 read
//
// 请求头采用白名单透传，其余（Cookie、x-stainless-* 等）一律丢弃。
package zen

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"zengateway/internal/idgen"
	"zengateway/internal/provider"
	"zengateway/internal/store"
)

// 默认值。
const (
	DefaultUpstream = "https://opencode.ai/zen/v1"
	defaultKey      = "public"
	defaultClient   = "cli"
	defaultProject  = "global"
)

// passHeaders 是允许透传给上游的请求头白名单。
var passHeaders = map[string]bool{
	"accept":             true,
	"authorization":      true,
	"content-type":       true,
	"user-agent":         true, // 会随后被覆盖为网关值
	"x-opencode-client":  true,
	"x-opencode-project": true,
	"x-opencode-request": true,
	"x-opencode-session": true,
}

// requiredTools 是上游判定必需的工具名集合。
var requiredTools = []string{"bash", "read"}

// Provider 实现 provider.Provider。
type Provider struct {
	upstream string
}

// New 创建 zen 模块；upstreamBase 为空时使用默认上游。
func New(upstreamBase string) *Provider {
	if strings.TrimSpace(upstreamBase) == "" {
		upstreamBase = DefaultUpstream
	}
	return &Provider{upstream: strings.TrimRight(upstreamBase, "/")}
}

func (p *Provider) Name() string         { return "zen" }
func (p *Provider) UpstreamBase() string { return p.upstream }
func (p *Provider) RequireKey() bool     { return false } // 未配置 key 时允许匿名 public

// PathFor 返回上游路径；cli 的鉴权由 idgen 生成的 session 等要素保证。
func (p *Provider) PathFor(kind provider.Kind) string {
	switch kind {
	case provider.KindResponses:
		return "/responses"
	case provider.KindModels:
		return "/models"
	default:
		return "/chat/completions"
	}
}

// PrepareBody 强制流式并补齐必需工具。
func (p *Provider) PrepareBody(kind provider.Kind, body map[string]any) bool {
	changed := false
	if v, ok := body["stream"].(bool); !ok || !v {
		body["stream"] = true
		changed = true
	}
	if kind == provider.KindResponses {
		if ensureToolsResponses(body) {
			changed = true
		}
	} else if ensureToolsChat(body) {
		changed = true
	}
	return changed
}

// BuildHeaders 按白名单重建请求头，并覆盖/补齐 opencode 身份要素。
func (p *Provider) BuildHeaders(downstream http.Header, key store.APIKey, v provider.Versions) http.Header {
	h := http.Header{}

	// 白名单透传
	for k, vs := range downstream {
		if !passHeaders[strings.ToLower(k)] {
			continue
		}
		for _, val := range vs {
			h.Add(k, val)
		}
	}

	// Authorization：优先池中 key；匿名 key 使用 public
	authz := strings.TrimSpace(h.Get("Authorization"))
	if key.APIKey != "" && !key.IsAnonymous {
		authz = "Bearer " + key.APIKey
	}
	if authz == "" {
		authz = "Bearer " + defaultKey
	}
	h.Set("Authorization", authz)

	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/json")
	}
	h.Set("Accept", orDefault(h.Get("Accept"), "*/*"))

	// User-Agent：不信任下游，强制网关值
	h.Set("User-Agent", "opencode/"+v.Zen)

	// x-opencode-* 四件套
	h.Set("x-opencode-client", orDefault(h.Get("x-opencode-client"), defaultClient))
	h.Set("x-opencode-project", orDefault(h.Get("x-opencode-project"), defaultProject))
	h.Set("x-opencode-request", orDefault(h.Get("x-opencode-request"), idgen.Message()))
	h.Set("x-opencode-session", orDefault(h.Get("x-opencode-session"), idgen.Session()))
	return h
}

// ModelOf 提取模型名。
func (p *Provider) ModelOf(body map[string]any) string {
	m, _ := body["model"].(string)
	return strings.TrimSpace(m)
}

// Classify 仅按 429 判定冷却；冷却到次日 0 点。其余响应一律透传。
func (p *Provider) Classify(status int, _ []byte) provider.Decision {
	if status != http.StatusTooManyRequests {
		return provider.Decision{Action: provider.ActionPass}
	}
	now := time.Now()
	d := cooldownUntilNextMidnight(now)
	return provider.Decision{
		Action:   provider.ActionCooldownAndNext,
		Cooldown: d,
		Reason:   "429",
		Detail:   "free tier limit reached, cooling until next midnight",
	}
}

// cooldownUntilNextMidnight 计算到次日 0 点的时长。
func cooldownUntilNextMidnight(now time.Time) time.Duration {
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	if d := midnight.Sub(now); d > 0 {
		return d
	}
	return 24 * time.Hour
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// ---- 请求体工具注入 -------------------------------------------------------

// ensureToolsChat 补齐 chat/completions 风格的工具定义。
func ensureToolsChat(body map[string]any) bool {
	raw, _ := body["tools"].([]any)
	have := map[string]bool{}
	for _, t := range raw {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := m["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				have[name] = true
			}
			continue
		}
		if name, _ := m["name"].(string); name != "" {
			have[name] = true
		}
	}
	missing := missingTools(have)
	if len(missing) == 0 {
		return false
	}
	for _, name := range missing {
		raw = append(raw, stubChatTool(name))
	}
	body["tools"] = raw
	return true
}

// ensureToolsResponses 补齐 responses 风格的工具定义（function 平铺）。
func ensureToolsResponses(body map[string]any) bool {
	raw, _ := body["tools"].([]any)
	have := map[string]bool{}
	for _, t := range raw {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := m["name"].(string); name != "" {
			have[name] = true
		}
	}
	missing := missingTools(have)
	if len(missing) == 0 {
		return false
	}
	for _, name := range missing {
		raw = append(raw, stubResponsesTool(name))
	}
	body["tools"] = raw
	return true
}

// missingTools 返回缺失的工具名（排序保证注入顺序确定）。
func missingTools(have map[string]bool) []string {
	var out []string
	for _, name := range requiredTools {
		if !have[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

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
