// Package rewrite 按规则改写请求体中的系统提示词文本。
//
// 背景：免费层上游可能检测系统提示词中的特定品牌关键词，注入规则可把
// 关键词替换为等价表述（例如 OpenClaw → OpenCode）以通过检测。
//
// 设计要点：
//   - 规则预编译并存入内存快照（atomic.Pointer），请求路径零 DB 查询
//   - 只改写「文本位置」，绝不触碰 model、工具名、结构键与其他 JSON 键
//   - 支持 chat/completions 与 responses 两套结构
//   - 任何异常都降级为原样返回（不影响转发），错误仅记录日志
//   - 不记录 prompt 原文，只记录规则命中次数
package rewrite

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zengateway/internal/store"
)

// maxExpansionRatio 限制单次替换的文本膨胀倍数，防止正则异常导致内存暴涨。
const maxExpansionRatio = 10

// ErrEmptyMatch 表示规则会产生零宽匹配（会导致在文本的每个位置插入替换文本）。
var ErrEmptyMatch = errors.New("rewrite: match pattern must not match empty string")

// hasZeroWidthMatch 报告正则在样例文本上是否产生零宽（起止下标相同）匹配。
//
// 不能只用 re.MatchString("") 判断：像 \b（词边界）这类零宽断言在空串上不成立，
// 但在真实文本的每个词边界都会命中零宽匹配，从而把替换文本插入到每个词边界
// （例如 "hi there" 被 "\b"→"X" 改成 "XhiX XthereX"）。
// 因此必须实际扫描一段同时含词字符与分隔符的样本文本。
func hasZeroWidthMatch(re *regexp.Regexp) bool {
	const probe = "a b"
	for _, m := range re.FindAllStringIndex(probe, -1) {
		if m[0] == m[1] {
			return true
		}
	}
	// 起点/终点锚定类断言（^、$、\A、\z）在 probe 上不会命中零宽，
	// 但它们在空串上成立，因此仍需单独覆盖。
	return re.MatchString("")
}

// Rule 是编译后的规则。
type Rule struct {
	ID            int64
	Module        string
	Name          string
	Match         string
	Replace       string
	IsRegex       bool
	CaseSensitive bool
	Scope         string
	IncludeTools  bool
	SortOrder     int

	// re 用于实际替换（字面量模式也会编译为正则，便于统一处理大小写与计数）。
	re *regexp.Regexp
	// literalReplace 为真时替换文本按字面量处理（不解释 $1 等捕获组引用）。
	// 仅在规则为「字面量匹配」时为真，避免用户写的 "$1" 被意外吃掉。
	literalReplace bool
}

// CompileRule 校验并编译规则。
//
// 会拒绝「匹配空字符串」的模式：否则 ReplaceAll 会在每个字符之间插入替换文本，
// 把提示词彻底破坏。
func CompileRule(r store.RewriteRule) (Rule, error) {
	out := Rule{
		ID: r.ID, Module: r.Module, Name: r.Name, Match: r.Match, Replace: r.Replace,
		IsRegex: r.IsRegex, CaseSensitive: r.CaseSensitive, Scope: r.Scope,
		IncludeTools: r.IncludeTools, SortOrder: r.SortOrder,
		// 字面量匹配时替换文本也必须按字面量处理：
		// 否则 "ver$1" 会被当成捕获组引用（无捕获组 → 变成 "ver"），损坏用户文本。
		literalReplace: !r.IsRegex,
	}
	if out.Scope == "" {
		out.Scope = store.ScopeSystemFirstUser
	}
	if !store.IsValidScope(out.Scope) {
		return Rule{}, fmt.Errorf("rewrite: invalid scope %q", out.Scope)
	}
	if r.Match == "" {
		return Rule{}, ErrEmptyMatch
	}

	pattern := r.Match
	if !r.IsRegex {
		pattern = regexp.QuoteMeta(r.Match)
	}
	if !r.CaseSensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return Rule{}, fmt.Errorf("rewrite: invalid pattern %q: %w", r.Match, err)
	}
	if hasZeroWidthMatch(re) {
		return Rule{}, ErrEmptyMatch
	}
	out.re = re
	return out, nil
}

// apply 对单段文本执行替换，返回新文本与替换次数。
func (r Rule) apply(s string) (string, int) {
	if s == "" || r.re == nil {
		return s, 0
	}
	if !r.re.MatchString(s) {
		return s, 0
	}

	// 计数：用空替换跑一遍匹配，只累加次数、不保留匹配下标。
	// 不用 FindAllStringIndex 是为了避免大文本 + 宽泛规则时物化大量下标切片
	// （匹配数可达数十万，仅计数就会分配可观内存）。
	n := 0
	r.re.ReplaceAllStringFunc(s, func(string) string {
		n++
		return ""
	})
	if n == 0 {
		return s, 0
	}

	// 替换语义分两种：
	//   字面量模式：$1 / ${name} / $$ 按普通字符写出（否则用户写的 "$1" 会被吃掉）
	//   正则模式  ：允许 $1 等捕获组引用
	var out string
	if r.literalReplace {
		out = r.re.ReplaceAllLiteralString(s, r.Replace)
	} else {
		out = r.re.ReplaceAllString(s, r.Replace)
	}
	if len(out) > len(s)*maxExpansionRatio {
		return s, 0
	}
	return out, n
}

// ---- 引擎 -----------------------------------------------------------------

// Engine 持有规则快照，支持热重载。
type Engine struct {
	log *slog.Logger
	st  loader
	// snapshots 按模块缓存编译结果（zen / cline / * 各自的规则集合）
	rules atomic.Pointer[map[string][]Rule]
	// hits 累计命中次数（规则 id → 次数），由 FlushHits 定期落库。
	// 用互斥锁而非 atomic.Pointer：累加本身是「读-改-写」，
	// 若用 atomic Load/Store 实现会在并发下丢失更新（实测丢失率可达 36%）。
	hitsMu sync.Mutex
	hits   map[int64]int64
}

// loader 是 Engine 依赖的存储接口（便于测试注入）。
type loader interface {
	ActiveRewriteRules(module string) ([]store.RewriteRule, error)
}

// New 创建引擎并立即加载一次规则。
func New(st loader, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	e := &Engine{log: log, st: st, hits: map[int64]int64{}}
	empty := map[string][]Rule{}
	e.rules.Store(&empty)
	e.Reload()
	return e
}

// Reload 从存储重新加载并编译全部规则。编译失败的规则被跳过（记日志），
// 不会影响其他规则。
func (e *Engine) Reload() {
	if e.st == nil {
		return
	}
	loaded := map[string][]Rule{}
	for _, module := range []string{"zen", "cline"} {
		raw, err := e.st.ActiveRewriteRules(module)
		if err != nil {
			e.log.Warn("load rewrite rules failed", "module", module, "err", err)
			continue
		}
		compiled := make([]Rule, 0, len(raw))
		for _, r := range raw {
			rule, err := CompileRule(r)
			if err != nil {
				e.log.Warn("skip invalid rewrite rule", "id", r.ID, "name", r.Name, "err", err)
				continue
			}
			compiled = append(compiled, rule)
		}
		loaded[module] = compiled
	}
	e.rules.Store(&loaded)
	e.log.Debug("rewrite rules reloaded",
		"zen", len(loaded["zen"]), "cline", len(loaded["cline"]))
}

// Rules 返回指定模块当前生效的规则（只读副本，用于 Web 展示与测试）。
func (e *Engine) Rules(module string) []Rule {
	m := e.rules.Load()
	if m == nil {
		return nil
	}
	src := (*m)[module]
	out := make([]Rule, len(src))
	copy(out, src)
	return out
}

// Result 描述一次改写的统计结果。
type Result struct {
	// Hits 是规则 id → 命中次数
	Hits map[int64]int
	// Total 是替换总次数
	Total int
}

// Apply 按当前模块规则改写请求体中的文本位置。
//
// 只处理文本：system/developer 消息、responses 的 instructions，以及
// 按规则 scope 决定的 user 文本；IncludeTools 为真时才处理工具描述。
// 任何结构性异常都会被忽略（保持原样），保证不影响转发。
func (e *Engine) Apply(module string, kind string, body map[string]any) Result {
	res := Result{Hits: map[int64]int{}}
	if body == nil {
		return res
	}
	rules := e.Rules(module)
	if len(rules) == 0 {
		return res
	}

	if kind == "responses" {
		e.applyResponses(rules, body, &res)
	} else {
		e.applyChat(rules, body, &res)
	}

	// 记录命中，供定期落库
	if res.Total > 0 {
		e.addHits(res.Hits)
	}
	return res
}

func (e *Engine) addHits(hits map[int64]int) {
	if len(hits) == 0 {
		return
	}
	e.hitsMu.Lock()
	defer e.hitsMu.Unlock()
	for id, n := range hits {
		if id > 0 {
			e.hits[id] += int64(n)
		}
	}
}

// ResetHits 丢弃尚未落库的命中计数。
//
// 管理端「清零」会先把库中计数置零，若不同步丢弃内存缓冲，
// 下一轮 persistHits 会把旧计数加回，表现为「清零无效」。
func (e *Engine) ResetHits() {
	e.hitsMu.Lock()
	defer e.hitsMu.Unlock()
	e.hits = map[int64]int64{}
}

// FlushHits 取出并清空累计命中次数，交由存储层落库。
// 取出与清空在同一临界区内完成，避免与并发的 addHits 竞争导致丢失。
func (e *Engine) FlushHits() map[int64]int64 {
	e.hitsMu.Lock()
	defer e.hitsMu.Unlock()
	if len(e.hits) == 0 {
		return nil
	}
	out := e.hits
	e.hits = map[int64]int64{}
	return out
}

// ---- chat/completions 结构 -------------------------------------------------

func (e *Engine) applyChat(rules []Rule, body map[string]any, res *Result) {
	msgs, _ := body["messages"].([]any)
	if len(msgs) > 0 {
		firstUserIdx := -1
		for i, mv := range msgs {
			m, ok := mv.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			switch role {
			case "user":
				if firstUserIdx < 0 {
					firstUserIdx = i
				}
			}
		}
		for i, mv := range msgs {
			m, ok := mv.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			for _, rule := range rules {
				if !ruleAppliesToRole(rule, role, i == firstUserIdx) {
					continue
				}
				e.rewriteContentField(m, "content", rule, res)
			}
		}
	}
	e.applyToolDescriptions(rules, body, res)
}

// ---- responses 结构 -------------------------------------------------------

func (e *Engine) applyResponses(rules []Rule, body map[string]any, res *Result) {
	// 1) 顶层 instructions：等价于插入到上下文最前面的 system 消息，
	//    优先级高于 input，因此必须处理。
	if instr, ok := body["instructions"]; ok {
		switch iv := instr.(type) {
		case string:
			out := iv
			for _, rule := range rules {
				if applied, n := rule.apply(out); n > 0 {
					out = applied
					recordHit(rule, n, res)
				}
			}
			body["instructions"] = out
		case []any:
			for _, pv := range iv {
				p, ok := pv.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := p["type"].(string); t != "text" && t != "input_text" {
					continue
				}
				s, ok := p["text"].(string)
				if !ok {
					continue
				}
				for _, rule := range rules {
					if out, n := rule.apply(s); n > 0 {
						s = out
						recordHit(rule, n, res)
					}
				}
				p["text"] = s
			}
		}
	}

	// 2) input：可能是字符串（等价于唯一用户输入）或 item 数组
	switch in := body["input"].(type) {
	case string:
		// 纯字符串视为用户输入：仅在 scope 允许处理 user 文本时改写
		for _, rule := range rules {
			if rule.Scope == store.ScopeMessages || rule.Scope == store.ScopeSystemFirstUser {
				if out, n := rule.apply(in); n > 0 {
					in = out
					recordHit(rule, n, res)
				}
			}
		}
		body["input"] = in
	case []any:
		firstUserIdx := -1
		for i, iv := range in {
			m, ok := iv.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := m["role"].(string); role == "user" && firstUserIdx < 0 {
				firstUserIdx = i
			}
		}
		for i, iv := range in {
			m, ok := iv.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			for _, rule := range rules {
				if !ruleAppliesToRole(rule, role, i == firstUserIdx) {
					continue
				}
				e.rewriteContentField(m, "content", rule, res)
			}
		}
	}
	e.applyToolDescriptions(rules, body, res)
}

// ---- 通用工具 -------------------------------------------------------------

// ruleAppliesToRole 判断规则是否应作用于该角色。
//
//	scope=system             → 仅 system / developer
//	scope=system_first_user  → 上述 + 首条 user 消息
//	scope=messages           → 全部角色
func ruleAppliesToRole(rule Rule, role string, isFirstUser bool) bool {
	isSystemRole := role == "system" || role == "developer"
	switch rule.Scope {
	case store.ScopeMessages:
		return true
	case store.ScopeSystemFirstUser:
		return isSystemRole || (role == "user" && isFirstUser)
	default: // ScopeSystem
		return isSystemRole
	}
}

// rewriteContentField 处理 message 对象中的 content 字段（string 或 parts 数组）。
func (e *Engine) rewriteContentField(m map[string]any, field string, rule Rule, res *Result) {
	v, ok := m[field]
	if !ok {
		return
	}
	switch cv := v.(type) {
	case string:
		if out, n := rule.apply(cv); n > 0 {
			m[field] = out
			recordHit(rule, n, res)
		}
	case []any:
		for _, pv := range cv {
			p, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			// 只处理文本 part；图片等非文本 part 原样保留
			if t, _ := p["type"].(string); t != "text" && t != "input_text" && t != "output_text" {
				continue
			}
			if s, ok := p["text"].(string); ok {
				if out, n := rule.apply(s); n > 0 {
					p["text"] = out
					recordHit(rule, n, res)
				}
			}
		}
	case map[string]any:
		// 少数客户端把 content 包成 {"type":"text","text":"..."}
		if t, _ := cv["type"].(string); t == "text" || t == "input_text" {
			if s, ok := cv["text"].(string); ok {
				if out, n := rule.apply(s); n > 0 {
					cv["text"] = out
					recordHit(rule, n, res)
				}
			}
		}
	}
}

// applyToolDescriptions 按规则开关改写 tools 的描述文本。
// 只改顶层 description，不递归 parameters，避免影响模型对参数结构的理解。
// 同时兼容两种结构：
//
//	chat:      {"type":"function","function":{"name":...,"description":...}}
//	responses: {"type":"function","name":...,"description":...}
func (e *Engine) applyToolDescriptions(rules []Rule, body map[string]any, res *Result) {
	tools, _ := body["tools"].([]any)
	if len(tools) == 0 {
		return
	}
	for _, tv := range tools {
		m, ok := tv.(map[string]any)
		if !ok {
			continue
		}
		// 有 function 包装时改嵌套层，否则改顶层（responses 扁平结构）
		target := m
		if fn, ok := m["function"].(map[string]any); ok {
			target = fn
		}
		desc, ok := target["description"].(string)
		if !ok {
			continue
		}
		for _, rule := range rules {
			if !rule.IncludeTools {
				continue
			}
			if out, n := rule.apply(desc); n > 0 {
				desc = out
				recordHit(rule, n, res)
			}
		}
		target["description"] = desc
	}
}

func recordHit(rule Rule, n int, res *Result) {
	if res.Hits == nil {
		res.Hits = map[int64]int{}
	}
	if rule.ID > 0 {
		res.Hits[rule.ID] += n
	}
	res.Total += n
}

// Preview 对给定文本试跑规则（不落库、不改动请求），用于 Web 测试面板。
// 只应用作用域为 system / system_first_user / messages 的字面文本替换，
// 便于用户直观看到关键词会被改写成什么。
func (e *Engine) Preview(module, text string) (string, []Hit) {
	rules := e.Rules(module)
	var hits []Hit
	out := text
	for _, rule := range rules {
		applied, n := rule.apply(out)
		if n > 0 {
			hits = append(hits, Hit{RuleID: rule.ID, Name: rule.Name, Count: n})
			out = applied
		}
	}
	return out, hits
}

// Hit 描述预览时某条规则的命中情况。
type Hit struct {
	RuleID int64  `json:"ruleId"`
	Name   string `json:"name"`
	Count  int    `json:"count"`
}

// ValidateRuleText 供 Web 保存前校验（不需要完整 store 记录）。
func ValidateRuleText(match string, isRegex, caseSensitive bool) error {
	if strings.TrimSpace(match) == "" {
		return errors.New("匹配内容不能为空")
	}
	pattern := match
	if !isRegex {
		pattern = regexp.QuoteMeta(match)
	}
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("正则表达式无效: %w", err)
	}
	if hasZeroWidthMatch(re) {
		return errors.New("匹配内容不能产生零宽匹配（否则会在文本每个位置插入替换文本）")
	}
	return nil
}

// DefaultReloadInterval 是规则快照的兜底重载间隔。
// Web 修改规则后会主动调用 Reload，这里的周期重载用于兜住「直接改库」的情形。
const DefaultReloadInterval = 60 * time.Second

// StartPeriodicReload 启动后台协程：定期重载规则并落库命中计数。
// 调用方应在进程退出时 cancel context。
func (e *Engine) StartPeriodicReload(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultReloadInterval
	}
	go func() {
		reload := time.NewTicker(interval)
		defer reload.Stop()
		// 命中计数落库频率低于规则重载（写库成本更高）
		flush := time.NewTicker(30 * time.Second)
		defer flush.Stop()
		for {
			select {
			case <-ctx.Done():
				e.persistHits()
				return
			case <-reload.C:
				e.Reload()
			case <-flush.C:
				e.persistHits()
			}
		}
	}()
}

// persistHits 把累计命中次数写入存储（失败仅记日志，不影响转发）。
// 写入失败时把计数放回缓冲，避免在退出竞态等场景下永久丢失。
func (e *Engine) persistHits() {
	hits := e.FlushHits()
	if len(hits) == 0 {
		return
	}
	p, ok := e.st.(hitPersister)
	if !ok {
		return
	}
	if err := p.AddRewriteHits(hits); err != nil {
		e.log.Warn("persist rewrite hits failed, counting will be retried", "err", err)
		e.restoreHits(hits)
	}
}

// restoreHits 把未能落库的计数放回缓冲（累加式，避免覆盖期间新增的计数）。
func (e *Engine) restoreHits(hits map[int64]int64) {
	e.hitsMu.Lock()
	defer e.hitsMu.Unlock()
	if e.hits == nil {
		e.hits = map[int64]int64{}
	}
	for id, n := range hits {
		e.hits[id] += n
	}
}

// hitPersister 由 store 实现（单独接口便于测试注入）。
type hitPersister interface {
	AddRewriteHits(map[int64]int64) error
}
