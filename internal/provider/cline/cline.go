// Package cline 实现 Cline 模块的上游适配。
//
// 上游会依据请求头判定客户端身份，因此这里完全忽略下游传入的请求头，
// 只发送一组固定的 Cline 产品头：
//
//	Authorization / HTTP-Referer / X-Title / X-IS-MULTIROOT
//	X-CLIENT-VERSION / X-PLATFORM / X-PLATFORM-VERSION / X-CORE-VERSION
//	User-Agent / X-CLIENT-TYPE
//
// 其中版本号每日从 GitHub 的 cline/cline release 抓取：
//   - X-CLIENT-VERSION / X-PLATFORM-VERSION / User-Agent 用主版本（v4.1.22 → 4.1.22）
//   - X-CORE-VERSION 用 SDK 版本（sdk/sdk/v0.0.90 → 0.0.90）
//
// 另外，上游对旧版本客户端会返回 403 与 "only available via Cline product surfaces"
// 提示，这里对这类响应固定重试 3 次；仍失败则原样返回给下游。
//
// 上游也没有 OpenAI 风格的 /v1/models（官方模型清单接口是
// /ai/cline/recommended-models），因此本模块实现 provider.ModelsProvider：
// models 端点改请求该接口，并把其中的 free 桶转换为 OpenAI /v1/models 格式。
package cline

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"zengateway/internal/inject"
	"zengateway/internal/provider"
	"zengateway/internal/store"
)

// DefaultUpstream 是 cline 上游基址。
// 与 zen 对称，/v1 属于基址的一部分，PathFor 只返回业务路径，
// 最终 URL 形如 https://api.cline.bot/api/v1/chat/completions。
const DefaultUpstream = "https://api.cline.bot/api/v1"

// productSurfaceHint 是触发重试的错误标记。
const productSurfaceHint = "is only available via Cline product surfaces"

// RetryAttempts 是 403 卡片校验错误的重试次数。
const RetryAttempts = 3

// cooldownRe 解析 "Try again in 22h 59m" 形式的冷却时长。
var cooldownRe = regexp.MustCompile(`(?i)try again in\s*(?:(\d+)\s*h(?:ours?|rs?)?)?\s*(?:(\d+)\s*m(?:in(?:ute)?s?)?)?`)

// Provider 实现 provider.Provider。
type Provider struct {
	upstream string
	// fallbackCooldown 是 429 错误文本无法解析时的兜底冷却时长。
	fallbackCooldown time.Duration
	// inj 是系统提示词注入引擎；为 nil 时跳过注入。
	inj *inject.Engine
}

// ModelsProvider 的两个方法都是可选的（靠类型断言生效），方法名一旦写错或
// 改名不会导致编译失败，而是静默退回已被废弃的 PathFor(KindModels)。
// 这行断言把这种退化提前到编译期。
var _ provider.ModelsProvider = (*Provider)(nil)

// New 创建 cline 模块。fallback 为 429 解析失败时的兜底冷却，inj 为 nil 表示不注入。
func New(upstreamBase string, fallback time.Duration, inj *inject.Engine) *Provider {
	if strings.TrimSpace(upstreamBase) == "" {
		upstreamBase = DefaultUpstream
	}
	if fallback <= 0 {
		fallback = time.Hour
	}
	return &Provider{
		upstream:         strings.TrimRight(upstreamBase, "/"),
		fallbackCooldown: fallback,
		inj:              inj,
	}
}

func (p *Provider) Name() string         { return "cline" }
func (p *Provider) UpstreamBase() string { return p.upstream }
func (p *Provider) RequireKey() bool     { return true } // 不支持匿名

// PathFor 返回上游业务路径（/v1 已包含在基址中，与 zen 保持一致）。
//
// 注意：KindModels 不会用到这里的 "/models"——本模块实现了
// provider.ModelsProvider，models 端点由 ModelsUpstreamPath 接管。
// 保留该分支仅为接口完整性。
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

// PrepareBody 按关键词注入原生系统提示词；cline 不改动 stream 与其他字段。
// 当前 cline 尚无内置原生提示词，未配置覆盖时命中不会改写（保持原样）。
func (p *Provider) PrepareBody(kind provider.Kind, body map[string]any) bool {
	if p.inj == nil || body == nil {
		return false
	}
	return p.inj.Apply(p.Name(), string(kind), body).Injected
}

// BuildHeaders 完全忽略下游请求头，只构造固定的 Cline 产品头。
func (p *Provider) BuildHeaders(_ http.Header, key store.APIKey, v provider.Versions) http.Header {
	cli := v.ClineCLI
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+key.APIKey)
	h.Set("HTTP-Referer", "https://cline.bot")
	h.Set("X-Title", "Cline")
	h.Set("X-IS-MULTIROOT", "false")
	h.Set("X-CLIENT-VERSION", cli)
	h.Set("X-PLATFORM", "cli")
	h.Set("X-PLATFORM-VERSION", cli)
	h.Set("X-CORE-VERSION", v.ClineSDK)
	h.Set("User-Agent", "Cline/"+cli)
	h.Set("X-CLIENT-TYPE", "cline-cli")
	return h
}

// ModelOf 提取模型名。
func (p *Provider) ModelOf(body map[string]any) string {
	m, _ := body["model"].(string)
	return strings.TrimSpace(m)
}

// ModelsUpstreamPath 返回 models 端点的上游路径（provider.ModelsProvider）。
// 官方没有 OpenAI 风格的 /v1/models，模型清单实际来自 recommended-models。
func (p *Provider) ModelsUpstreamPath() string { return "/ai/cline/recommended-models" }

// recommendedModel 是 free 桶中的一项。
//
// 上游该项还有一个 tags 数组，网关用不到，因此故意不声明——避免上游改动
// 该字段类型时把整个端点拖挂（encoding/json 会因类型不符而解码失败）。
type recommendedModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// modelList 与 modelItem 是 OpenAI /v1/models 的响应结构。
// name / description 为附加字段：OpenAI 客户端会忽略未知字段，
// 保留它们便于管理端与用户看清模型含义。
type modelList struct {
	Object string      `json:"object"`
	Data   []modelItem `json:"data"`
}

type modelItem struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Created     int64  `json:"created"`
	OwnedBy     string `json:"owned_by"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// ConvertModels 把官方 recommended-models 响应中的 free 桶改写为 OpenAI /v1/models 格式。
//
// 不直接透传上游 /v1/models，原因有二：
//   - 它是全量清单（实测 466 个，含付费模型），而网关只免费转发 free 桶
//   - 它反而缺少部分 free 模型（实测缺 cline-free/mimo-v2.6-flash 等）
//
// created 取转换时刻：官方清单不含时间戳，无法还原真实创建时间。
func (p *Provider) ConvertModels(body []byte) ([]byte, error) {
	// 先用原始字段探测 free 桶是否存在。该接口固定返回 recommended / free /
	// clinePass / clineCloud 四个桶，缺失 free 几乎总是上游 schema 漂移（或返回了
	// 错误对象），此时报错——否则下游只会看到"没有模型"，难以定位。
	// 显式的 `"free": []` 与 `"free": null` 仍视为合法的空列表。
	var buckets map[string]json.RawMessage
	if err := json.Unmarshal(body, &buckets); err != nil {
		return nil, fmt.Errorf("parse recommended-models: %w", err)
	}
	freeRaw, ok := buckets["free"]
	if !ok {
		return nil, fmt.Errorf("recommended-models response has no free bucket (present: %s)", joinKeys(buckets))
	}
	var free []recommendedModel
	if err := json.Unmarshal(freeRaw, &free); err != nil {
		return nil, fmt.Errorf("parse free bucket: %w", err)
	}

	now := time.Now().Unix()
	out := modelList{Object: "list", Data: make([]modelItem, 0, len(free))}
	for _, e := range free {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue // 无 ID 的条目对下游无意义
		}
		out.Data = append(out.Data, modelItem{
			ID:          id,
			Object:      "model",
			Created:     now,
			OwnedBy:     ownerOf(id),
			Name:        strings.TrimSpace(e.Name),
			Description: strings.TrimSpace(e.Description),
		})
	}
	return json.Marshal(out)
}

// joinKeys 按字典序拼接顶层字段名，用于 free 桶缺失时给出可诊断的报错。
func joinKeys(m map[string]json.RawMessage) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// ownerOf 取模型 ID 的命名空间前缀作为 owned_by，与官方 /v1/models 的惯例一致
// （如 inclusionai/ling-3.1-flash → inclusionai）。无前缀时回落为 "cline"。
func ownerOf(id string) string {
	if i := strings.IndexByte(id, '/'); i > 0 {
		return id[:i]
	}
	return "cline"
}

// Classify 处理两类特殊响应：
//
//	403 + 产品面提示 → 同一 key 重试（最多 RetryAttempts 次）
//	429             → 解析错误文本中的冷却时长，冷却该 key 的该模型后换 key
func (p *Provider) Classify(status int, body []byte) provider.Decision {
	switch status {
	case http.StatusForbidden:
		if strings.Contains(string(body), productSurfaceHint) {
			return provider.Decision{
				Action:   provider.ActionRetrySame,
				MaxRetry: RetryAttempts,
				Reason:   "403",
				Detail:   summarize(body),
			}
		}
		return provider.Decision{Action: provider.ActionPass}

	case http.StatusTooManyRequests:
		d, ok := ParseCooldown(string(body))
		if !ok {
			d = p.fallbackCooldown
		}
		return provider.Decision{
			Action:   provider.ActionCooldownAndNext,
			Cooldown: d,
			Reason:   "429",
			Detail:   summarize(body),
		}
	}
	return provider.Decision{Action: provider.ActionPass}
}

// ParseCooldown 解析上游 429 错误文本中的冷却时长。
// 支持 "Try again in 22h 59m"、"Try again in 45m"、"Try again in 3h" 等形式。
func ParseCooldown(body string) (time.Duration, bool) {
	m := cooldownRe.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	var h, min int
	if m[1] != "" {
		h, _ = strconv.Atoi(m[1])
	}
	if m[2] != "" {
		min, _ = strconv.Atoi(m[2])
	}
	if h == 0 && min == 0 {
		return 0, false
	}
	d := time.Duration(h)*time.Hour + time.Duration(min)*time.Minute
	// 留 1 分钟余量，避免与上游时钟误差导致提前重试
	return d + time.Minute, true
}

func summarize(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
