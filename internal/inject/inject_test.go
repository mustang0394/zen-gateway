package inject

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"zengateway/internal/prompt"
)

// fakeStore 提供可控的关键词与提示词覆盖。
type fakeStore struct {
	keywords  map[string][]Keyword
	overrides map[string]string
}

func (f *fakeStore) ActiveInjectKeywords(module string) ([]Keyword, error) {
	var out []Keyword
	for _, k := range f.keywords[module] {
		out = append(out, k)
	}
	// module='*' 对每个模块都生效，模拟 store 的查询口径
	for _, k := range f.keywords["*"] {
		out = append(out, k)
	}
	return out, nil
}

func (f *fakeStore) PromptOverride(module string) (string, error) {
	return f.overrides[module], nil
}

const testPrompt = "You are opencode, an interactive CLI tool."

func newEngine(t *testing.T, kws ...Keyword) (*Engine, *fakeStore) {
	t.Helper()
	st := &fakeStore{
		keywords:  map[string][]Keyword{},
		overrides: map[string]string{"zen": testPrompt}, // 用短文本便于断言
	}
	for i, k := range kws {
		if k.ID == 0 {
			k.ID = int64(i + 1)
		}
		if k.Module == "" {
			k.Module = "zen"
		}
		st.keywords[k.Module] = append(st.keywords[k.Module], k)
	}
	return New(st, nil), st
}

func body(msgs ...map[string]any) map[string]any {
	arr := make([]any, len(msgs))
	for i, m := range msgs {
		arr[i] = m
	}
	return map[string]any{"model": "m", "messages": arr}
}

func contentOf(t *testing.T, b map[string]any, i int) string {
	t.Helper()
	s, _ := b["messages"].([]any)[i].(map[string]any)["content"].(string)
	return s
}

// ---- 检测语义 -------------------------------------------------------------

func TestHitReplacesWholeSystemPrompt(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	b := body(
		map[string]any{"role": "system", "content": "You are Claude Code, a CLI tool."},
		map[string]any{"role": "user", "content": "hello"},
	)
	res := e.Apply("zen", "chat", b)

	if !res.Injected || res.Matched != "Claude Code" {
		t.Fatalf("expected injection, got %+v", res)
	}
	if got := contentOf(t, b, 0); got != testPrompt {
		t.Errorf("system prompt should be fully replaced, got %q", got)
	}
	// 用户消息不受影响
	if got := contentOf(t, b, 1); got != "hello" {
		t.Errorf("user message must be untouched, got %q", got)
	}
}

func TestCaseInsensitiveMatching(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	for _, variant := range []string{
		"You are Claude Code.", "you are claude code", "CLAUDE CODE rocks", "cLaUdE cOdE",
	} {
		b := body(map[string]any{"role": "system", "content": variant})
		if res := e.Apply("zen", "chat", b); !res.Injected {
			t.Errorf("variant %q should hit (case-insensitive)", variant)
		}
		if got := contentOf(t, b, 0); got != testPrompt {
			t.Errorf("variant %q: prompt not replaced, got %q", variant, got)
		}
	}
}

func TestNoHitLeavesBodyUntouched(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	original := "You are a helpful assistant."
	b := body(map[string]any{"role": "system", "content": original})

	res := e.Apply("zen", "chat", b)
	if res.Injected {
		t.Error("should not inject without a keyword hit")
	}
	if got := contentOf(t, b, 0); got != original {
		t.Errorf("content must be unchanged, got %q", got)
	}
}

func TestOnlyFirstSystemMessageConsidered(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	b := body(
		map[string]any{"role": "system", "content": "clean first prompt"},
		map[string]any{"role": "system", "content": "You are Claude Code."},
	)
	res := e.Apply("zen", "chat", b)
	if res.Injected {
		t.Error("only the first system message should be inspected")
	}
	if got := contentOf(t, b, 0); got != "clean first prompt" {
		t.Errorf("first system message should be untouched, got %q", got)
	}
	if got := contentOf(t, b, 1); got != "You are Claude Code." {
		t.Errorf("later system messages should stay as-is, got %q", got)
	}
}

func TestFirstSystemMessageIsReplacedWhenHit(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	b := body(
		map[string]any{"role": "system", "content": "You are Claude Code."},
		map[string]any{"role": "system", "content": "extra rules"},
	)
	if res := e.Apply("zen", "chat", b); !res.Injected {
		t.Fatal("should inject")
	}
	if got := contentOf(t, b, 0); got != testPrompt {
		t.Errorf("first system message should be replaced, got %q", got)
	}
	if got := contentOf(t, b, 1); got != "extra rules" {
		t.Errorf("second system message should be kept, got %q", got)
	}
}

func TestDeveloperRoleIsSystemRole(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	b := body(map[string]any{"role": "developer", "content": "You are Claude Code."})
	if res := e.Apply("zen", "chat", b); !res.Injected {
		t.Error("developer role should be treated as system")
	}
}

func TestOnlySystemRoleIsChecked(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	// 关键词只出现在 user/assistant 中 → 不注入
	b := body(
		map[string]any{"role": "user", "content": "what is Claude Code?"},
		map[string]any{"role": "assistant", "content": "Claude Code is a tool"},
	)
	if res := e.Apply("zen", "chat", b); res.Injected {
		t.Error("user/assistant messages must not trigger injection")
	}
}

// ---- 模块与规则筛选 -------------------------------------------------------

func TestModuleIsolationAndGlobalRules(t *testing.T) {
	st := &fakeStore{
		keywords: map[string][]Keyword{
			"zen":   {{ID: 1, Module: "zen", Keyword: "zen-only", Enabled: true}},
			"cline": {{ID: 2, Module: "cline", Keyword: "cline-only", Enabled: true}},
			"*":     {{ID: 3, Module: "*", Keyword: "global-kw", Enabled: true}},
		},
		overrides: map[string]string{"zen": testPrompt, "cline": "cline prompt"},
	}
	e := New(st, nil)

	// zen 关键词对 zen 生效
	b := body(map[string]any{"role": "system", "content": "has zen-only inside"})
	if res := e.Apply("zen", "chat", b); !res.Injected {
		t.Error("zen keyword should apply to zen")
	}
	// zen 关键词不影响 cline
	b2 := body(map[string]any{"role": "system", "content": "has zen-only inside"})
	if res := e.Apply("cline", "chat", b2); res.Injected {
		t.Error("zen keyword must not apply to cline")
	}
	// 全局关键词对两者生效
	for _, m := range []string{"zen", "cline"} {
		bb := body(map[string]any{"role": "system", "content": "has global-kw inside"})
		if res := e.Apply(m, "chat", bb); !res.Injected {
			t.Errorf("global keyword should apply to %s", m)
		}
	}
	// cline 模块使用自己的提示词
	bb := body(map[string]any{"role": "system", "content": "has global-kw inside"})
	e.Apply("cline", "chat", bb)
	if got := contentOf(t, bb, 0); got != "cline prompt" {
		t.Errorf("cline should use its own prompt, got %q", got)
	}
}

func TestDisabledKeywordIgnored(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: false})
	b := body(map[string]any{"role": "system", "content": "You are Claude Code."})
	if res := e.Apply("zen", "chat", b); res.Injected {
		t.Error("disabled keyword must not trigger injection")
	}
}

func TestBlankKeywordIgnored(t *testing.T) {
	// 空关键词会匹配一切，必须在加载时被丢弃
	st := &fakeStore{
		keywords:  map[string][]Keyword{"zen": {{ID: 1, Module: "zen", Keyword: "   ", Enabled: true}}},
		overrides: map[string]string{"zen": testPrompt},
	}
	e := New(st, nil)
	if len(e.Keywords("zen")) != 0 {
		t.Errorf("blank keyword should be dropped, got %+v", e.Keywords("zen"))
	}
	b := body(map[string]any{"role": "system", "content": "anything at all"})
	if res := e.Apply("zen", "chat", b); res.Injected {
		t.Error("blank keyword must not cause injection")
	}
}

func TestMultipleKeywordsFirstMatchWins(t *testing.T) {
	e, _ := newEngine(t,
		Keyword{ID: 1, Keyword: "alpha", Enabled: true},
		Keyword{ID: 2, Keyword: "beta", Enabled: true},
	)
	b := body(map[string]any{"role": "system", "content": "has both alpha and beta"})
	res := e.Apply("zen", "chat", b)
	if !res.Injected {
		t.Fatal("should inject")
	}
	if res.RuleID != 1 {
		t.Errorf("first keyword in order should win, got rule %d", res.RuleID)
	}
}

// ---- 无可用提示词时不破坏请求 ---------------------------------------------

func TestNoPromptAvailableKeepsContent(t *testing.T) {
	st := &fakeStore{
		keywords:  map[string][]Keyword{"zen": {{ID: 1, Module: "zen", Keyword: "Claude Code", Enabled: true}}},
		overrides: map[string]string{}, // 无覆盖
	}
	e := New(st, nil)
	// 用 cline（无内置提示词）验证：命中但无提示词可用时应保持原样
	b := body(map[string]any{"role": "system", "content": "You are Claude Code."})
	res := e.Apply("cline", "chat", b)
	if res.Injected {
		t.Error("without an available prompt, injection must be skipped")
	}
	if got := contentOf(t, b, 0); got != "You are Claude Code." {
		t.Errorf("content must be preserved, got %q", got)
	}
}

// ---- responses 结构 -------------------------------------------------------

func TestResponsesInstructionsReplaced(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	b := map[string]any{
		"model":        "m",
		"instructions": "You are Claude Code.",
		"input":        "hello",
	}
	res := e.Apply("zen", "responses", b)
	if !res.Injected {
		t.Fatal("instructions should be inspected and replaced")
	}
	if got, _ := b["instructions"].(string); got != testPrompt {
		t.Errorf("instructions = %q", got)
	}
	if got, _ := b["input"].(string); got != "hello" {
		t.Errorf("input must be untouched, got %q", got)
	}
}

func TestResponsesInputItems(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	b := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"role": "system", "content": "You are Claude Code."},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	res := e.Apply("zen", "responses", b)
	if !res.Injected {
		t.Fatal("input system item should be inspected")
	}
	items := b["input"].([]any)
	if got, _ := items[0].(map[string]any)["content"].(string); got != testPrompt {
		t.Errorf("system item = %q", got)
	}
	if got, _ := items[1].(map[string]any)["content"].(string); got != "hi" {
		t.Errorf("user item must be untouched, got %q", got)
	}
}

func TestResponsesInstructionsPartsArray(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	b := map[string]any{
		"model": "m",
		"instructions": []any{
			map[string]any{"type": "text", "text": "You are Claude Code."},
			map[string]any{"type": "image_url", "image_url": "http://x"},
		},
	}
	res := e.Apply("zen", "responses", b)
	if !res.Injected {
		t.Fatal("should inject for parts array")
	}
	parts := b["instructions"].([]any)
	if got, _ := parts[0].(map[string]any)["text"].(string); got != testPrompt {
		t.Errorf("first text part = %q", got)
	}
	// 非文本 part 必须保留
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Error("non-text part must be preserved")
	}
}

// ---- content parts（chat）-------------------------------------------------

func TestChatContentPartsArray(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	b := body(map[string]any{
		"role": "system",
		"content": []any{
			map[string]any{"type": "text", "text": "You are Claude Code."},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x"}},
		},
	})
	res := e.Apply("zen", "chat", b)
	if !res.Injected {
		t.Fatal("parts array should be inspected")
	}
	parts := b["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if got, _ := parts[0].(map[string]any)["text"].(string); got != testPrompt {
		t.Errorf("text part = %q", got)
	}
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Error("image part must be preserved")
	}
}

// ---- 结构化字段保护 -------------------------------------------------------

func TestStructuredFieldsUntouched(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	b := map[string]any{
		"model": "claude-code-model", // 含关键词但绝不能改
		"messages": []any{
			map[string]any{"role": "system", "content": "You are Claude Code."},
		},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "ClaudeCode_tool", "description": "Claude Code helper"},
		}},
		"temperature": 0.7,
		"stream":      true,
	}
	e.Apply("zen", "chat", b)

	if b["model"] != "claude-code-model" {
		t.Errorf("model must never change, got %v", b["model"])
	}
	if b["temperature"] != 0.7 || b["stream"] != true {
		t.Error("other fields must be preserved")
	}
	fn := b["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "ClaudeCode_tool" || fn["description"] != "Claude Code helper" {
		t.Errorf("tools must never be modified, got %+v", fn)
	}
	if got := contentOf(t, b, 0); got != testPrompt {
		t.Errorf("system prompt should be replaced, got %q", got)
	}
}

// ---- 健壮性 ---------------------------------------------------------------

func TestNilAndMalformedBodiesDoNotPanic(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "x", Enabled: true})
	cases := []map[string]any{
		nil,
		{},
		{"messages": nil},
		{"messages": "not-an-array"},
		{"messages": []any{"str", 42, nil}},
		{"messages": []any{map[string]any{"role": "system"}}},
		{"messages": []any{map[string]any{"role": "system", "content": nil}}},
		{"messages": []any{map[string]any{"role": "system", "content": 42}}},
		{"messages": []any{map[string]any{"role": "system", "content": []any{"raw"}}}},
		{"input": 42, "instructions": 7},
		{"instructions": []any{"raw", nil, 5}},
		{"input": []any{map[string]any{"role": "system", "content": nil}}},
	}
	for i, b := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("case %d panicked: %v", i, r)
				}
			}()
			e.Apply("zen", "chat", b)
			e.Apply("zen", "responses", b)
		}()
	}
}

func TestHitsRecordedAndFlushed(t *testing.T) {
	e, _ := newEngine(t, Keyword{ID: 7, Keyword: "Claude Code", Enabled: true})
	for i := 0; i < 3; i++ {
		b := body(map[string]any{"role": "system", "content": "You are Claude Code."})
		e.Apply("zen", "chat", b)
	}
	hits := e.FlushHits()
	if hits[7] != 3 {
		t.Errorf("hits = %d, want 3", hits[7])
	}
	if again := e.FlushHits(); len(again) != 0 {
		t.Errorf("second flush should be empty, got %+v", again)
	}
	// ResetHits 丢弃未落盘计数
	b := body(map[string]any{"role": "system", "content": "You are Claude Code."})
	e.Apply("zen", "chat", b)
	e.ResetHits()
	if again := e.FlushHits(); len(again) != 0 {
		t.Errorf("after reset, pending hits must be discarded, got %+v", again)
	}
}

func TestConcurrentApplyDoesNotLoseHits(t *testing.T) {
	e, _ := newEngine(t, Keyword{ID: 1, Keyword: "Claude Code", Enabled: true})
	const workers, per = 32, 100
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				b := body(map[string]any{"role": "system", "content": "You are Claude Code."})
				e.Apply("zen", "chat", b)
			}
		}()
	}
	wg.Wait()
	if got := e.FlushHits()[1]; got != int64(workers*per) {
		t.Errorf("lost hits: got %d, want %d", got, workers*per)
	}
}

func TestReloadPicksUpChanges(t *testing.T) {
	st := &fakeStore{
		keywords:  map[string][]Keyword{},
		overrides: map[string]string{"zen": testPrompt},
	}
	e := New(st, nil)

	b := body(map[string]any{"role": "system", "content": "You are Claude Code."})
	if res := e.Apply("zen", "chat", b); res.Injected {
		t.Fatal("no keywords yet")
	}

	st.keywords["zen"] = []Keyword{{ID: 1, Module: "zen", Keyword: "Claude Code", Enabled: true}}
	e.Reload()

	b2 := body(map[string]any{"role": "system", "content": "You are Claude Code."})
	if res := e.Apply("zen", "chat", b2); !res.Injected {
		t.Error("reload should pick up the new keyword")
	}

	// 提示词覆盖值变更也应生效
	st.overrides["zen"] = "new prompt"
	e.Reload()
	b3 := body(map[string]any{"role": "system", "content": "You are Claude Code."})
	e.Apply("zen", "chat", b3)
	if got := contentOf(t, b3, 0); got != "new prompt" {
		t.Errorf("override change should apply, got %q", got)
	}
}

func TestDetectForTestPanel(t *testing.T) {
	e, _ := newEngine(t, Keyword{ID: 5, Keyword: "Claude Code", Note: "第三方", Enabled: true})

	if _, hit := e.Detect("zen", "I use Claude Code daily"); !hit {
		t.Error("should detect")
	}
	if _, hit := e.Detect("zen", "nothing here"); hit {
		t.Error("should not detect")
	}
	// Detect 不得写命中统计
	if h := e.FlushHits(); len(h) != 0 {
		t.Errorf("Detect must not record hits, got %+v", h)
	}
}

func TestValidateKeyword(t *testing.T) {
	if err := ValidateKeyword("Claude Code"); err != nil {
		t.Errorf("valid keyword rejected: %v", err)
	}
	if err := ValidateKeyword("   "); err == nil {
		t.Error("blank keyword should be rejected")
	}
	if err := ValidateKeyword(strings.Repeat("x", 300)); err == nil {
		t.Error("oversized keyword should be rejected")
	}
}

// TestBuiltinZenPromptIsUsedByDefault 验证未设置覆盖时会使用内置 zen 提示词。
func TestBuiltinZenPromptIsUsedByDefault(t *testing.T) {
	st := &fakeStore{
		keywords:  map[string][]Keyword{"zen": {{ID: 1, Module: "zen", Keyword: "Claude Code", Enabled: true}}},
		overrides: map[string]string{}, // 无覆盖 → 用内置
	}
	e := New(st, nil)

	b := body(map[string]any{"role": "system", "content": "You are Claude Code."})
	res := e.Apply("zen", "chat", b)
	if !res.Injected {
		t.Fatal("should inject using the builtin prompt")
	}
	if res.PromptSource != "builtin" {
		t.Errorf("source = %q, want builtin", res.PromptSource)
	}
	got := contentOf(t, b, 0)
	if !strings.Contains(got, "You are opencode") {
		t.Errorf("should use the builtin zen prompt, got %q", truncate(got, 80))
	}
	if len(got) != len(prompt.Module("zen")) {
		t.Errorf("prompt length mismatch: %d vs %d", len(got), len(prompt.Module("zen")))
	}
	// 确认是逐字节原样写入
	if got != prompt.Module("zen") {
		t.Error("builtin prompt must be used byte-for-byte")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestPromptOverrideSourceReported 验证覆盖值的来源标记。
func TestPromptOverrideSourceReported(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	b := body(map[string]any{"role": "system", "content": "You are Claude Code."})
	res := e.Apply("zen", "chat", b)
	if res.PromptSource != "override" {
		t.Errorf("source = %q, want override", res.PromptSource)
	}
}

// 确保 JSON 往返后仍能正确注入（模拟真实请求体经 json 解码的形态）。
func TestAfterJSONRoundTrip(t *testing.T) {
	e, _ := newEngine(t, Keyword{Keyword: "Claude Code", Enabled: true})
	raw := `{"model":"m","messages":[{"role":"system","content":"You are Claude Code."},
		{"role":"user","content":"hi"}],"temperature":0.5}`
	var b map[string]any
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res := e.Apply("zen", "chat", b); !res.Injected {
		t.Fatal("should inject after JSON round-trip")
	}
	out, _ := json.Marshal(b)
	var back map[string]any
	json.Unmarshal(out, &back)
	msgs := back["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"].(string); got != testPrompt {
		t.Errorf("after round-trip = %q", got)
	}
	if back["temperature"] != 0.5 {
		t.Error("other fields must survive")
	}
}
