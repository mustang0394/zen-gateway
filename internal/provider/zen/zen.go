// Package zen 实现 OpenCode Zen 模块的上游适配。
//
// 上游会校验请求是否来自 opencode 官方客户端，缺失任一要素即 403 FreeTierError：
//
//	User-Agent: opencode/<latest>      每日从 GitHub 同步，无条件覆盖下游值
//	x-opencode-session                 缺失时按 opencode ID 算法生成
//	x-opencode-request                 缺失时生成 msg_ ID
//	x-opencode-client                  缺失时补 cli
//	x-opencode-project                 缺失时补 global
//	Authorization                      完全由池中 key 决定（匿名回落到 Bearer public）
//	body.stream                        强制 true
//	body.tools                         必须同时包含 bash 与 read
//
// 请求头采用白名单透传，其余（Cookie、x-stainless-* 等）一律丢弃。
package zen

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"zengateway/internal/idgen"
	"zengateway/internal/idmap"
	"zengateway/internal/inject"
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
//
// authorization 故意不在其中：下游的 Authorization 是网关自身的接入 Token，
// 必须由池中 key 决定发给上游的值（见 BuildHeaders）。
var passHeaders = map[string]bool{
	"accept":             true,
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
	// inj 是系统提示词注入引擎；为 nil 时跳过注入。
	inj *inject.Engine
	// sessions 与 messages 把下游传来的**非法** ID 映射为网关生成的合法 ID。
	//
	// 上游只接受严格格式的 x-opencode-session（见 internal/idgen 的实测记录），
	// 第三方客户端（如 OpenClaw 用 randomUUID()）的 ID 会被判为非官方客户端而 403。
	// 若每个请求都重新生成，上游 session 会不断变化、prompt 缓存前缀失效；
	// 因此按原始值建立映射，让同一客户端会话始终得到同一个合法 ID。
	// 映射仅存内存，条目 1 小时未访问即过期。
	sessions *idmap.Map
	messages *idmap.Map
}

// SessionTTL 是 ID 映射条目的存活时间（需求：3600s）。
const SessionTTL = time.Hour

// New 创建 zen 模块；upstreamBase 为空时使用默认上游，inj 为 nil 表示不注入。
func New(upstreamBase string, inj *inject.Engine) *Provider {
	return NewWithTTL(upstreamBase, inj, SessionTTL)
}

// NewWithTTL 同 New，但可指定 ID 映射的过期时间（<=0 时用默认值）。主要供测试使用。
func NewWithTTL(upstreamBase string, inj *inject.Engine, ttl time.Duration) *Provider {
	if strings.TrimSpace(upstreamBase) == "" {
		upstreamBase = DefaultUpstream
	}
	return &Provider{
		upstream: strings.TrimRight(upstreamBase, "/"),
		inj:      inj,
		sessions: idmap.New(idgen.Session, ttl),
		messages: idmap.New(idgen.Message, ttl),
	}
}

// StartIDSweeper 启动 ID 映射的后台清理协程。
// 调用方应在进程退出时 cancel context。
func (p *Provider) StartIDSweeper(ctx context.Context, interval time.Duration) {
	if p.sessions != nil {
		p.sessions.StartSweeper(ctx, interval)
	}
	if p.messages != nil {
		p.messages.StartSweeper(ctx, interval)
	}
}

// resolveSession 返回可安全发往上游的 session ID：
//   - 下游未传 → 生成新的合法 ID
//   - 下游传了合法值 → 原样保留（保住客户端自己的 session 与上游缓存）
//   - 下游传了非法值 → 按原值映射到网关生成的合法 ID（同一原值稳定复用）
func (p *Provider) resolveSession(v string) string {
	if v == "" {
		return idgen.Session()
	}
	if idgen.IsSession(v) {
		return v
	}
	if p.sessions == nil {
		return idgen.Session()
	}
	return p.sessions.Resolve(v)
}

// resolveMessage 同 resolveSession，用于 x-opencode-request。
// 注：上游实测不校验该头格式，此处仍统一处理以保持两个头行为一致。
func (p *Provider) resolveMessage(v string) string {
	if v == "" {
		return idgen.Message()
	}
	if idgen.IsMessage(v) {
		return v
	}
	if p.messages == nil {
		return idgen.Message()
	}
	return p.messages.Resolve(v)
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

// PrepareBody 按关键词注入原生系统提示词、强制流式并补齐必需工具。
//
// 顺序很关键：先做提示词注入（基于下游原始内容判断），再注入 bash/read stub。
//
// body 为 nil 时直接返回：本方法会写入 body（stream/tools），
// 在 nil map 上写入会 panic，因此即便调用方已做防护，这里仍显式拦住。
func (p *Provider) PrepareBody(kind provider.Kind, body map[string]any) bool {
	if body == nil {
		return false
	}
	changed := false

	if p.inj != nil {
		if res := p.inj.Apply(p.Name(), string(kind), body); res.Injected {
			changed = true
		}
	}

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

	// Authorization：完全由池中 key 决定，不透传下游值。
	//
	// 下游的 Authorization 是网关自己的接入 Token（或客户端随手带的值），
	// 与上游凭据无关。早期实现优先沿用下游值，导致配置了接入 Token 后，
	// 网关 Token 被当作上游 key 发送，上游一律回 401 Invalid API key
	// （实测确认）。仅匿名 key（池中未配）且下游未带时回落到 public。
	authz := ""
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
	// x-opencode-session / x-opencode-request：下游值合法则保留，非法则映射/生成。
	// 上游只校验 session 的格式（实测），非法 session 会导致 403 FreeTierError。
	h.Set("x-opencode-request", p.resolveMessage(strings.TrimSpace(h.Get("x-opencode-request"))))
	h.Set("x-opencode-session", p.resolveSession(strings.TrimSpace(h.Get("x-opencode-session"))))
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
