// Package inject 实现「关键词检测 → 整体替换系统提示词」。
//
// 背景：免费层上游会按关键词检测 system prompt，识别到非官方客户端的特征
// （例如第三方工具的品牌名）就拒绝服务。逐词替换（把 A 换成 B）不足以绕过，
// 因此改为：一旦系统提示词中出现配置的关键词，就把**整条系统提示词**
// 替换为本模块的原生提示词，使请求看起来来自官方客户端。
//
// 检测规则（与用户需求一致）：
//   - 关键词匹配**忽略大小写**
//   - 只检查**首条** system/developer 消息（以及 responses 的 instructions）
//   - 命中即整体替换该条内容；其余 system 消息保持原样
//
// 设计要点：
//   - 关键词与提示词目标值都可在运行时热更新（内存快照，请求路径零 DB 查询）
//   - 绝不改动 model、messages 结构、工具定义或其他字段
//   - 任何异常降级为原样透传，不阻断请求
package inject

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"zengateway/internal/prompt"
	"zengateway/internal/store"
)

// Target 描述一次注入的目标提示词。
type Target struct {
	// Module 是模块名（zen / cline），用于取默认提示词与统计。
	Module string
	// SystemPrompt 是命中后要写入的原生提示词。
	SystemPrompt string
}

// Rule 是一条关键词规则（内存快照中的形态）。
type Rule struct {
	ID      int64
	Module  string // zen | cline | *
	Keyword string
	Note    string
	Enabled bool
}

// Config 是注入引擎的运行时配置。
type Config struct {
	Keywords []Rule
}

// Engine 持有内存快照并执行注入。
type Engine struct {
	log *slog.Logger
	st  source

	mu  sync.RWMutex
	cfg Config

	hitsMu sync.Mutex
	hits   map[int64]int64
}

// source 是引擎依赖的持久化接口（便于测试注入）。
type source interface {
	ActiveInjectKeywords(module string) ([]store.Keyword, error)
	PromptOverride(module string) (string, error)
}

// Keyword 是关键词规则的对外形态（复用存储层定义，避免无意义的类型转换）。
type Keyword = store.Keyword

// New 创建引擎并立即加载一次配置。
func New(st source, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	e := &Engine{log: log, st: st, hits: map[int64]int64{}}
	e.Reload()
	return e
}

// Reload 重新加载关键词与提示词覆盖值。
func (e *Engine) Reload() {
	if e.st == nil {
		return
	}

	// 提示词覆盖值（模块 → 文本），优先于内置提示词
	overrides := map[string]string{}
	for _, module := range []string{"zen", "cline"} {
		if txt, err := e.st.PromptOverride(module); err != nil {
			e.log.Warn("load prompt override failed", "module", module, "err", err)
		} else if strings.TrimSpace(txt) != "" {
			overrides[module] = txt
		}
	}
	prompt.SetAllOverrides(overrides)

	// 关键词规则
	byModule := map[string][]Rule{}
	for _, module := range []string{"zen", "cline"} {
		rows, err := e.st.ActiveInjectKeywords(module)
		if err != nil {
			e.log.Warn("load inject keywords failed", "module", module, "err", err)
			continue
		}
		rules := make([]Rule, 0, len(rows))
		for _, r := range rows {
			kw := strings.TrimSpace(r.Keyword)
			if kw == "" {
				continue // 空关键词会匹配一切，跳过
			}
			rules = append(rules, Rule{
				ID: r.ID, Module: r.Module, Keyword: kw, Note: r.Note, Enabled: r.Enabled,
			})
		}
		byModule[module] = rules
	}

	e.mu.Lock()
	e.cfg = Config{Keywords: flatten(byModule)}
	e.mu.Unlock()

	e.log.Debug("inject config reloaded",
		"zenKeywords", len(byModule["zen"]), "clineKeywords", len(byModule["cline"]),
		"overrides", len(overrides))
}

// flatten 把「模块 → 规则」摊平为一个切片，便于统一遍历（规则数很少）。
func flatten(m map[string][]Rule) []Rule {
	var out []Rule
	for _, rules := range m {
		out = append(out, rules...)
	}
	return out
}

// Result 描述一次注入的结果。
type Result struct {
	// Injected 表示是否发生了整体替换。
	Injected bool
	// Matched 是被命中的关键词。
	Matched string
	// RuleID 是命中的规则 id（用于命中统计）。
	RuleID int64
	// PromptSource 说明改写后的提示词来源。
	PromptSource string
}

// Apply 按 kind 选择结构执行注入。kind 取 "chat" 或 "responses"。
func (e *Engine) Apply(module, kind string, body map[string]any) Result {
	if body == nil {
		return Result{}
	}
	target := Target{Module: module, SystemPrompt: prompt.Resolve(module)}

	switch kind {
	case "responses":
		return e.applyResponses(target, body)
	default:
		return e.applyChat(target, body)
	}
}

// keywordsFor 返回对该模块生效的关键词（含 module='*' 的全局规则）。
func (e *Engine) keywordsFor(module string) []Rule {
	e.mu.RLock()
	all := e.cfg.Keywords
	e.mu.RUnlock()

	var out []Rule
	for _, r := range all {
		if !r.Enabled {
			continue
		}
		if r.Module == module || r.Module == "*" || r.Module == "" {
			out = append(out, r)
		}
	}
	return out
}

// detect 在文本中查找第一个命中的关键词（忽略大小写），并返回命中的规则。
func (e *Engine) detect(module, text string) (Rule, bool) {
	lower := strings.ToLower(text)
	for _, r := range e.keywordsFor(module) {
		if strings.Contains(lower, strings.ToLower(r.Keyword)) {
			return r, true
		}
	}
	return Rule{}, false
}

// record 累加命中次数（供定期落库观察）。
func (e *Engine) record(rule Rule) {
	if rule.ID <= 0 {
		return
	}
	e.hitsMu.Lock()
	e.hits[rule.ID]++
	e.hitsMu.Unlock()
}

// FlushHits 取出并清空命中计数。
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

// ResetHits 丢弃尚未落库的命中计数（管理端「清零」时同步调用）。
func (e *Engine) ResetHits() {
	e.hitsMu.Lock()
	defer e.hitsMu.Unlock()
	e.hits = map[int64]int64{}
}

// Keywords 返回指定模块当前生效的关键词（只读副本，供 Web 展示与测试）。
func (e *Engine) Keywords(module string) []Rule {
	out := e.keywordsFor(module)
	return out
}

// Detect 供 Web「测试」面板使用：报告文本是否命中关键词。
func (e *Engine) Detect(module, text string) (Rule, bool) {
	return e.detect(module, text)
}

// ---- chat/completions 结构 -------------------------------------------------

func (e *Engine) applyChat(target Target, body map[string]any) Result {
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		return Result{}
	}
	// 只检查并改写首条 system/developer 消息
	for _, mv := range msgs {
		m, ok := mv.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		return e.rewriteMessage(target, m, "content")
	}
	return Result{}
}

// ---- responses 结构 -------------------------------------------------------

func (e *Engine) applyResponses(target Target, body map[string]any) Result {
	// responses 的顶层 instructions 等价于首条 system 消息，优先处理
	if v, ok := body["instructions"]; ok {
		if res, out := e.rewriteValue(target, v); out.Injected {
			body["instructions"] = res
			return out
		}
	}

	// 其次检查 input 数组中的首条 system/developer item
	items, _ := body["input"].([]any)
	for _, iv := range items {
		m, ok := iv.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		return e.rewriteMessage(target, m, "content")
	}
	return Result{}
}

// rewriteMessage 在消息对象的文本字段上执行检测与整体替换。
func (e *Engine) rewriteMessage(target Target, m map[string]any, field string) Result {
	v, ok := m[field]
	if !ok {
		return Result{}
	}
	res, out := e.rewriteValue(target, v)
	if out.Injected {
		m[field] = res
	}
	return out
}

// rewriteValue 处理文本值（字符串或 parts 数组）。
// 返回 (新值, 已替换)：已替换为 false 时新值无意义，调用方保持原样。
func (e *Engine) rewriteValue(target Target, v any) (any, Result) {
	switch tv := v.(type) {
	case string:
		return e.rewriteString(target, tv)
	case []any:
		// parts 数组：以文本 part 拼接内容做检测
		text := collectText(tv)
		if text == "" {
			return nil, Result{}
		}
		rule, hit := e.detect(target.Module, text)
		if !hit {
			return nil, Result{}
		}
		if target.SystemPrompt == "" {
			// 没有可用的原生提示词：保持原样（不破坏请求）
			return nil, Result{}
		}
		// 整体替换：保留第一个文本 part 承载新提示词，其余文本 part 清空为占位
		replaced := false
		for _, pv := range tv {
			p, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			t, _ := p["type"].(string)
			if t != "text" && t != "input_text" && t != "output_text" {
				continue
			}
			if !replaced {
				p["text"] = target.SystemPrompt
				replaced = true
				continue
			}
			p["text"] = ""
		}
		if !replaced {
			return nil, Result{}
		}
		e.record(rule)
		return tv, Result{
			Injected: true, Matched: rule.Keyword, RuleID: rule.ID,
			PromptSource: prompt.Source(target.Module),
		}
	case map[string]any:
		// 少数客户端把 content 包成 {"type":"text","text":"..."}
		t, _ := tv["type"].(string)
		if t != "text" && t != "input_text" && t != "output_text" {
			return nil, Result{}
		}
		s, ok := tv["text"].(string)
		if !ok {
			return nil, Result{}
		}
		res, done := e.rewriteString(target, s)
		if !done.Injected {
			return nil, Result{}
		}
		tv["text"] = res
		return tv, done
	}
	return nil, Result{}
}

// rewriteString 对单个字符串执行检测；命中则返回原生提示词。
// 已替换信息通过 Result.Injected 表达，未命中时返回原值与零值结果。
func (e *Engine) rewriteString(target Target, s string) (any, Result) {
	if s == "" {
		return nil, Result{}
	}
	rule, hit := e.detect(target.Module, s)
	if !hit {
		return nil, Result{}
	}
	if target.SystemPrompt == "" {
		return nil, Result{}
	}
	e.record(rule)
	return target.SystemPrompt, Result{
		Injected: true, Matched: rule.Keyword, RuleID: rule.ID,
		PromptSource: prompt.Source(target.Module),
	}
}

// collectText 拼接 parts 数组中的文本内容（用于关键词检测）。
func collectText(parts []any) string {
	var b strings.Builder
	for _, pv := range parts {
		p, ok := pv.(map[string]any)
		if !ok {
			continue
		}
		t, _ := p["type"].(string)
		if t != "text" && t != "input_text" && t != "output_text" {
			continue
		}
		if s, ok := p["text"].(string); ok {
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ---- 后台维护 -------------------------------------------------------------

// DefaultReloadInterval 是配置快照的兜底重载间隔。
const DefaultReloadInterval = 60 * time.Second

// StartPeriodicReload 定期重载配置并落库命中计数。
func (e *Engine) StartPeriodicReload(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultReloadInterval
	}
	go func() {
		reload := time.NewTicker(interval)
		defer reload.Stop()
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

func (e *Engine) persistHits() {
	hits := e.FlushHits()
	if len(hits) == 0 {
		return
	}
	p, ok := e.st.(hitPersister)
	if !ok {
		return
	}
	if err := p.AddInjectHits(hits); err != nil {
		e.log.Warn("persist inject hits failed, will retry", "err", err)
		e.restoreHits(hits)
	}
}

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

type hitPersister interface {
	AddInjectHits(map[int64]int64) error
}

// ValidateKeyword 供 Web 保存前校验。
func ValidateKeyword(keyword string) error {
	if strings.TrimSpace(keyword) == "" {
		return fmt.Errorf("关键词不能为空")
	}
	if len(keyword) > 200 {
		return fmt.Errorf("关键词过长（上限 200 字节）")
	}
	return nil
}
