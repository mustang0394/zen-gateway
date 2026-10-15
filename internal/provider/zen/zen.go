// Package zen 实现 OpenCode Zen 模块的上游适配。
//
// 上游会校验请求是否来自 opencode 官方客户端，缺失任一要素即 403 FreeTierError。
// 2026-10-14 起校验升级为「客户端指纹」——按 opencode 2.x（CLI 2.0.29）在线上
// 真实发出的请求逐项比对（对照 backups/opencode 客户端的
// packages/core/src/session/model-request.ts 与 packages/core/src/app.ts）：
//
//	User-Agent          opencode/<channel>/<version>/cli
//	                    （旧形状 opencode/<version> 一律 403）
//	x-opencode-session      会话亲和 ID（非法值映射为网关生成的合法 ID）
//	x-opencode-session-id   与 x-opencode-session 同值，二者必须同时存在
//	x-session-affinity /
//	X-Session-Id            客户端同样携带，取同一会话 ID
//	x-opencode-project      裸 40 位小写 hex；canonical 的 prj_* 值会 403
//	x-opencode-request      已废弃：携带即 403，网关彻底不再发送
//	x-opencode-client       补 cli
//	Authorization           完全由池中 key 决定（匿名回落到 Bearer public）
//	body.stream             强制 true
//	body.tools              必须带核心工具，且工具定义与官方逐字节一致（见 tools.go）
//
// 另外匿名免费层与 Key 层已不在同一主机：匿名走 https://opencode.ai/inference/v1，
// Key 层仍走配置的 zen 基址（见 UpstreamBaseFor）。请求头不再透传下游值，
// 只按上面的固定集合构造，避免任何多余头破坏指纹。
package zen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
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
	// DefaultUpstream 是 Key 层的 zen 基址（控制台代理）。
	DefaultUpstream = "https://opencode.ai/zen/v1"
	// DefaultAnonymousUpstream 是匿名免费层的基址。
	//
	// 2026-10-14 起 keyless 流量不再走 /zen：控制台代理会把 public 匿名的
	// Authorization 摘掉，新网关据此判定为非官方客户端。匿名流量因此单独指向
	// /inference（Key 层也可用 https://opencode.ai/inference/openai，网关保持
	// 原有的 /zen 基址不变）。
	DefaultAnonymousUpstream = "https://opencode.ai/inference/v1"

	defaultKey     = "public"
	defaultClient  = "cli"
	defaultProject = "global"

	// userAgentChannel 是 UA 的 channel 段，与官方 beta/prod 渠道等价即可
	// （上游只校验开放形状，不校验具体取值）。
	userAgentChannel = "latest"
	// fallbackVersion 在版本号缺失时兜底，避免产出 opencode/latest//cli 这类畸形 UA。
	fallbackVersion = "2.0.29"
)

// 会话相关请求头。x-opencode-request 不在其中：它已被上游废弃，携带即 403。
const (
	headerSession        = "x-opencode-session"
	headerSessionID      = "x-opencode-session-id"
	headerSessionAffinit = "x-session-affinity"
	headerSessionIDUpper = "X-Session-Id"
	headerClient         = "x-opencode-client"
	headerProject        = "x-opencode-project"
)

// 下游可能用任意名字传入会话 ID 的头（按优先级取第一个非空值）。
var sessionHeaders = []string{headerSession, headerSessionID, headerSessionAffinit, headerSessionIDUpper}

// Provider 实现 provider.Provider。
type Provider struct {
	upstream string
	// anonymous 是匿名免费层（未配置 key / 标记为匿名）使用的上游基址。
	anonymous string
	// inj 是系统提示词注入引擎；为 nil 时跳过注入。
	inj *inject.Engine
	// sessions 把下游传来的**非法**会话 ID 映射为网关生成的合法 ID。
	//
	// 上游只接受严格格式的 x-opencode-session（见 internal/idgen 的实测记录），
	// 第三方客户端（如 OpenClaw 用 randomUUID()）的 ID 会被判为非官方客户端而 403。
	// 若每个请求都重新生成，上游 session 会不断变化、prompt 缓存前缀失效；
	// 因此按原始值建立映射，让同一客户端会话始终得到同一个合法 ID。
	// 映射仅存内存，条目 1 小时未访问即过期。
	sessions *idmap.Map
}

// SessionTTL 是 ID 映射条目的存活时间（需求：3600s）。
const SessionTTL = time.Hour

// New 创建 zen 模块；upstreamBase / anonymousBase 为空时使用默认上游，
// inj 为 nil 表示不注入。
func New(upstreamBase, anonymousBase string, inj *inject.Engine) *Provider {
	return NewWithTTL(upstreamBase, anonymousBase, inj, SessionTTL)
}

// NewWithTTL 同 New，但可指定 ID 映射的过期时间（<=0 时用默认值）。主要供测试使用。
func NewWithTTL(upstreamBase, anonymousBase string, inj *inject.Engine, ttl time.Duration) *Provider {
	if strings.TrimSpace(upstreamBase) == "" {
		upstreamBase = DefaultUpstream
	}
	if strings.TrimSpace(anonymousBase) == "" {
		anonymousBase = DefaultAnonymousUpstream
	}
	return &Provider{
		upstream:  strings.TrimRight(upstreamBase, "/"),
		anonymous: strings.TrimRight(anonymousBase, "/"),
		inj:       inj,
		sessions:  idmap.New(idgen.Session, ttl),
	}
}

// StartIDSweeper 启动 ID 映射的后台清理协程。
// 调用方应在进程退出时 cancel context。
func (p *Provider) StartIDSweeper(ctx context.Context, interval time.Duration) {
	if p.sessions != nil {
		p.sessions.StartSweeper(ctx, interval)
	}
}

// resolveSession 返回可安全发往上游的 session ID：
//   - 下游未传 → 生成新的合法 ID
//   - 下游传了合法值 → 原样保留（保住客户端自己的 session 与上游缓存）
//   - 下游传了非法值 → 按原值映射到网关生成的合法 ID（同一原值稳定复用）
func (p *Provider) resolveSession(v string) string {
	v = strings.TrimSpace(v)
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

func (p *Provider) Name() string         { return "zen" }
func (p *Provider) UpstreamBase() string { return p.upstream }
func (p *Provider) RequireKey() bool     { return false } // 未配置 key 时允许匿名 public

// UpstreamBaseFor 实现 provider.BaseProvider：按 key 选择上游基址。
//
// 匿名免费层（内置 public 回退条目，或管理端标记为匿名的 key，或值就是 public 的 key）
// 走 /inference，Key 层走配置的 zen 基址。两者不能混用：新网关对 keyless 请求(不带凭据)
// 一律判 403，而 /inference 又拒绝 canonical 的 sk-* 凭据。
func (p *Provider) UpstreamBaseFor(key store.APIKey) string {
	if isAnonymousLane(key) {
		return p.anonymous
	}
	return p.upstream
}

// isAnonymousLane 报告该 key 是否应走匿名免费层。
//
// 除了管理端的匿名标记，值等于 public 的 key 也归入匿名层：项目对匿名条目的语义
// 就是“凭据为 public”（store 的内置回退与 Web 新建时都是这样处理的）。
func isAnonymousLane(key store.APIKey) bool {
	return key.IsAnonymous || strings.TrimSpace(key.APIKey) == defaultKey
}

// PathFor 返回上游路径；cli 的鉴权由指纹头与 session 等要素保证。
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

// PrepareBody 按关键词注入原生系统提示词、强制流式并补齐核心工具。
//
// 顺序很关键：先做提示词注入（基于下游原始内容判断），再补齐工具定义。
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

// BuildHeaders 构造发往上游的固定头集合。
//
// 上游按「官方客户端指纹」判请求来源，因此这里不透传下游头，只发客户端
// 自己会发的那些（见包注释）；任何多余的头都可能导致 403。
func (p *Provider) BuildHeaders(downstream http.Header, key store.APIKey, v provider.Versions) http.Header {
	h := http.Header{}

	// Authorization：完全由池中 key 决定，不透传下游值。
	//
	// 下游的 Authorization 是网关自己的接入 Token（或客户端随手带的值），
	// 与上游凭据无关。早期实现优先沿用下游值，导致配置了接入 Token 后，
	// 网关 Token 被当作上游 key 发送，上游一律回 401 Invalid API key
	// （实测确认）。仅匿名 key（池中未配）且下游未带时回落到 public。
	authz := ""
	if !isAnonymousLane(key) && strings.TrimSpace(key.APIKey) != "" {
		authz = "Bearer " + key.APIKey
	}
	if authz == "" {
		authz = "Bearer " + defaultKey
	}
	h.Set("Authorization", authz)

	h.Set("Content-Type", "application/json")
	h.Set("Accept", orDefault(downstream.Get("Accept"), "*/*"))

	// User-Agent：不信任下游，强制网关值（形状 opencode/<channel>/<version>/<cli>）。
	h.Set("User-Agent", userAgent(v.Zen))

	// 会话四件套取同一个合法 ID；下游任一会话头合法则保留（保住客户端 session
	// 与上游 prompt 缓存），非法则映射，缺失则生成。
	session := p.resolveSession(firstHeader(downstream, sessionHeaders...))
	h.Set(headerSession, session)
	h.Set(headerSessionID, session)
	h.Set(headerSessionAffinit, session)
	h.Set(headerSessionIDUpper, session)

	h.Set(headerClient, defaultClient)
	// project 上游只接受裸 40 位小写 hex（canonical prj_* 会 403），
	// 这里把下游信号哈希进该形状，既通过校验又能把同一会话钉在同一 project 上。
	h.Set(headerProject, clientProjectID(orDefault(downstream.Get(headerProject), defaultProject)))

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

// userAgent 返回上游要求的 UA 形状：opencode/<channel>/<version>/<name>。
//
// 旧形状 opencode/<version>（1.x 客户端）在新指纹校验下会被判为非官方客户端。
func userAgent(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		version = fallbackVersion
	}
	return "opencode/" + userAgentChannel + "/" + version + "/" + defaultClient
}

// clientProjectID 把本地 project 信号哈希为客户端那样的 40 位小写 hex。
//
// 上游只校验形状（canonical 的 prj_* 值会 403），值本身不校验；
// 哈希保证同一会话稳定落在同一上游 project 上。
func clientProjectID(project string) string {
	sum := sha256.Sum256([]byte("opencode-project\x00" + project))
	return hex.EncodeToString(sum[:20])
}

// firstHeader 返回给定头名中第一个非空值（取首值，忽略大小写由 http.Header 处理）。
func firstHeader(h http.Header, names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
