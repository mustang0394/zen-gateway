package rewrite

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"zengateway/internal/store"
)

// fakeLoader 提供可控的规则集合。
type fakeLoader struct {
	rules map[string][]store.RewriteRule
}

func (f *fakeLoader) ActiveRewriteRules(module string) ([]store.RewriteRule, error) {
	return f.rules[module], nil
}

func newEngine(t *testing.T, rules ...store.RewriteRule) *Engine {
	t.Helper()
	byModule := map[string][]store.RewriteRule{}
	for i, r := range rules {
		if r.ID == 0 {
			r.ID = int64(i + 1)
		}
		if r.Module == "" {
			r.Module = "zen"
		}
		byModule[r.Module] = append(byModule[r.Module], r)
		// module='*' 对两个模块都生效，模拟 store.ActiveRewriteRules 的行为
		if r.Module == store.ModuleAll {
			byModule["zen"] = append(byModule["zen"], r)
			byModule["cline"] = append(byModule["cline"], r)
		}
	}
	return New(&fakeLoader{rules: byModule}, nil)
}

func mustJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func str(t *testing.T, v any) string {
	t.Helper()
	s, _ := v.(string)
	return s
}

// ---- 编译校验 -------------------------------------------------------------

func TestCompileRejectsEmptyMatch(t *testing.T) {
	// 字面量空匹配
	if _, err := CompileRule(store.RewriteRule{Match: "x"}); err != nil {
		t.Fatalf("normal literal should compile: %v", err)
	}
	if _, err := CompileRule(store.RewriteRule{Match: ""}); !errors.Is(err, ErrEmptyMatch) {
		t.Errorf("empty match should be rejected, got %v", err)
	}
	// 能匹配空串的正则（这是最危险的：会在每个字符间插入替换文本）
	for _, pattern := range []string{`a*`, `(?:)`, `x?`, `\d*`} {
		if _, err := CompileRule(store.RewriteRule{Match: pattern, IsRegex: true}); !errors.Is(err, ErrEmptyMatch) {
			t.Errorf("regex %q matching empty should be rejected, got %v", pattern, err)
		}
	}
}

// TestCompileRejectsZeroWidthAssertions 覆盖零宽断言。
//
// 回归保护：仅用 re.MatchString("") 判断会漏掉 `\b`——它在空串上不成立
// （空串无词边界），但在真实文本的每个词边界都产生零宽匹配，
// 导致文本被逐词边界插入替换文本（实测 "hi there" → "XhiX XthereX"）。
func TestCompileRejectsZeroWidthAssertions(t *testing.T) {
	// 在真实文本上会产生零宽匹配的断言
	for _, pattern := range []string{`\b`, `\B`, `^`, `$`, `\A`, `\z`, `(?m)^`, `(?m)$`, `x*\b`} {
		if _, err := CompileRule(store.RewriteRule{Match: pattern, IsRegex: true}); !errors.Is(err, ErrEmptyMatch) {
			t.Errorf("zero-width pattern %q must be rejected, got %v", pattern, err)
		}
		if err := ValidateRuleText(pattern, true, false); err == nil {
			t.Errorf("ValidateRuleText should reject zero-width pattern %q", pattern)
		}
	}

	// 对照：带实际字符的模式不应被误拒
	for _, pattern := range []string{`\bfoo\b`, `OpenClaw`, `^OpenClaw`, `OpenClaw$`} {
		if _, err := CompileRule(store.RewriteRule{Match: pattern, IsRegex: true}); err != nil {
			t.Errorf("pattern %q should be accepted, got %v", pattern, err)
		}
	}
}

func TestCompileRejectsInvalidRegex(t *testing.T) {
	if _, err := CompileRule(store.RewriteRule{Match: "(unclosed", IsRegex: true}); err == nil {
		t.Error("invalid regex must be rejected")
	}
	if _, err := CompileRule(store.RewriteRule{Match: "ok", Scope: "bogus"}); err == nil {
		t.Error("invalid scope must be rejected")
	}
}

func TestValidateRuleText(t *testing.T) {
	if err := ValidateRuleText("OpenClaw", false, false); err != nil {
		t.Errorf("literal should pass: %v", err)
	}
	if err := ValidateRuleText("", false, false); err == nil {
		t.Error("empty should fail")
	}
	if err := ValidateRuleText("a*", true, false); err == nil {
		t.Error("empty-matching regex should fail")
	}
	if err := ValidateRuleText("(bad", true, false); err == nil {
		t.Error("invalid regex should fail")
	}
}

// ---- 作用域矩阵 -----------------------------------------------------------

func chatBody(msgs ...map[string]any) map[string]any {
	arr := make([]any, len(msgs))
	for i, m := range msgs {
		arr[i] = m
	}
	return map[string]any{"model": "m", "messages": arr}
}

func TestScopeSystemOnlyTouchesSystemRole(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem, Enabled: true,
	})
	body := chatBody(
		map[string]any{"role": "system", "content": "You are OpenClaw."},
		map[string]any{"role": "user", "content": "Tell me about OpenClaw"},
		map[string]any{"role": "assistant", "content": "OpenClaw is ..."},
	)
	res := e.Apply("zen", "chat", body)
	if res.Total != 1 {
		t.Fatalf("expected 1 replacement, got %d", res.Total)
	}
	msgs := body["messages"].([]any)
	if got := str(t, msgs[0].(map[string]any)["content"]); got != "You are OpenCode." {
		t.Errorf("system content = %q", got)
	}
	if got := str(t, msgs[1].(map[string]any)["content"]); got != "Tell me about OpenClaw" {
		t.Errorf("user content must be untouched with scope=system, got %q", got)
	}
	if got := str(t, msgs[2].(map[string]any)["content"]); got != "OpenClaw is ..." {
		t.Errorf("assistant content must be untouched with scope=system, got %q", got)
	}
}

func TestScopeSystemFirstUserIncludesFirstUserOnly(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystemFirstUser, Enabled: true,
	})
	body := chatBody(
		map[string]any{"role": "system", "content": "You are OpenClaw."},
		map[string]any{"role": "user", "content": "first: OpenClaw"},
		map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "second: OpenClaw"},
	)
	res := e.Apply("zen", "chat", body)
	if res.Total != 2 {
		t.Fatalf("expected 2 replacements (system + first user), got %d", res.Total)
	}
	msgs := body["messages"].([]any)
	if got := str(t, msgs[1].(map[string]any)["content"]); got != "first: OpenCode" {
		t.Errorf("first user = %q", got)
	}
	if got := str(t, msgs[3].(map[string]any)["content"]); got != "second: OpenClaw" {
		t.Errorf("later user messages must be untouched, got %q", got)
	}
}

func TestScopeMessagesTouchesEverything(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeMessages, Enabled: true,
	})
	body := chatBody(
		map[string]any{"role": "system", "content": "OpenClaw"},
		map[string]any{"role": "user", "content": "OpenClaw"},
		map[string]any{"role": "assistant", "content": "OpenClaw"},
		map[string]any{"role": "tool", "content": "OpenClaw"},
	)
	res := e.Apply("zen", "chat", body)
	if res.Total != 4 {
		t.Fatalf("expected 4 replacements, got %d", res.Total)
	}
}

func TestDeveloperRoleIsSystemRole(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem, Enabled: true,
	})
	body := chatBody(map[string]any{"role": "developer", "content": "You are OpenClaw"})
	if res := e.Apply("zen", "chat", body); res.Total != 1 {
		t.Fatalf("developer role must be treated as system, got %d", res.Total)
	}
}

// ---- content 结构 ---------------------------------------------------------

func TestContentPartsArray(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem, Enabled: true,
	})
	body := chatBody(map[string]any{
		"role": "system",
		"content": []any{
			map[string]any{"type": "text", "text": "You are OpenClaw"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x/y.png"}},
		},
	})
	res := e.Apply("zen", "chat", body)
	if res.Total != 1 {
		t.Fatalf("expected 1 replacement in parts array, got %d", res.Total)
	}
	parts := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if got := str(t, parts[0].(map[string]any)["text"]); got != "You are OpenCode" {
		t.Errorf("text part = %q", got)
	}
	// 图片 part 必须完全不变
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("image part must be preserved: %+v", img)
	}
	if u := str(t, img["image_url"].(map[string]any)["url"]); u != "http://x/y.png" {
		t.Errorf("image url changed: %q", u)
	}
}

func TestInputTextAndOutputTextParts(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem, Enabled: true,
	})
	body := chatBody(map[string]any{
		"role": "system",
		"content": []any{
			map[string]any{"type": "input_text", "text": "in OpenClaw"},
			map[string]any{"type": "output_text", "text": "out OpenClaw"},
		},
	})
	if res := e.Apply("zen", "chat", body); res.Total != 2 {
		t.Fatalf("input_text/output_text should be rewritten, got %d", res.Total)
	}
}

// ---- responses 结构 -------------------------------------------------------

func TestResponsesInstructions(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem, Enabled: true,
	})
	body := mustJSON(t, map[string]any{
		"model":        "m",
		"instructions": "You are OpenClaw, be helpful.",
		"input":        "hello",
	})
	res := e.Apply("zen", "responses", body)
	if res.Total != 1 {
		t.Fatalf("instructions must be rewritten, got %d", res.Total)
	}
	if got := str(t, body["instructions"]); got != "You are OpenCode, be helpful." {
		t.Errorf("instructions = %q", got)
	}
	// input 是纯字符串且 scope=system → 不改
	if got := str(t, body["input"]); got != "hello" {
		t.Errorf("plain string input must be untouched with scope=system, got %q", got)
	}
}

func TestResponsesPlainStringInputWithFirstUserScope(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystemFirstUser, Enabled: true,
	})
	body := mustJSON(t, map[string]any{"model": "m", "input": "about OpenClaw"})
	if res := e.Apply("zen", "responses", body); res.Total != 1 {
		t.Fatalf("plain string input should be rewritten under system_first_user, got %d", res.Total)
	}
	if got := str(t, body["input"]); got != "about OpenCode" {
		t.Errorf("input = %q", got)
	}
}

func TestResponsesInputItems(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystemFirstUser, Enabled: true,
	})
	body := mustJSON(t, map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"role": "system", "content": "sys OpenClaw"},
			map[string]any{"role": "user", "content": "first OpenClaw"},
			map[string]any{"role": "user", "content": "second OpenClaw"},
		},
	})
	res := e.Apply("zen", "responses", body)
	if res.Total != 2 {
		t.Fatalf("expected 2 replacements, got %d", res.Total)
	}
	items := body["input"].([]any)
	if got := str(t, items[0].(map[string]any)["content"]); got != "sys OpenCode" {
		t.Errorf("system item = %q", got)
	}
	if got := str(t, items[1].(map[string]any)["content"]); got != "first OpenCode" {
		t.Errorf("first user item = %q", got)
	}
	if got := str(t, items[2].(map[string]any)["content"]); got != "second OpenClaw" {
		t.Errorf("second user item must be untouched: %q", got)
	}
}

func TestResponsesInstructionsPartsArray(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem, Enabled: true,
	})
	body := mustJSON(t, map[string]any{
		"model": "m",
		"instructions": []any{
			map[string]any{"type": "text", "text": "You are OpenClaw"},
			map[string]any{"type": "image_url", "image_url": "http://x"},
		},
	})
	e.Apply("zen", "responses", body)
	parts := body["instructions"].([]any)
	if got := str(t, parts[0].(map[string]any)["text"]); got != "You are OpenCode" {
		t.Errorf("instructions part = %q", got)
	}
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Error("non-text instructions part must be preserved")
	}
}

// ---- tools 描述 -----------------------------------------------------------

func TestToolDescriptionsOnlyWhenEnabled(t *testing.T) {
	bodyWithTools := func() map[string]any {
		return chatBody(map[string]any{"role": "system", "content": "sys"})
	}

	// 默认关闭：描述不改
	b1 := bodyWithTools()
	b1["tools"] = []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "bash", "description": "Run OpenClaw commands",
			"parameters": map[string]any{"type": "object"},
		},
	}}
	e1 := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem, Enabled: true,
	})
	if res := e1.Apply("zen", "chat", b1); res.Total != 0 {
		t.Fatalf("tools description must not change by default, got %d", res.Total)
	}
	got := b1["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["description"]
	if str(t, got) != "Run OpenClaw commands" {
		t.Errorf("description changed unexpectedly: %q", got)
	}

	// 开启后：只改 description，不动 name 与 parameters
	b2 := bodyWithTools()
	b2["tools"] = []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "bash", "description": "Run OpenClaw commands",
			"parameters": map[string]any{"type": "object", "description": "OpenClaw param"},
		},
	}}
	e2 := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem,
		IncludeTools: true, Enabled: true,
	})
	if res := e2.Apply("zen", "chat", b2); res.Total != 1 {
		t.Fatalf("expected 1 tools description replacement, got %d", res.Total)
	}
	fn := b2["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if str(t, fn["description"]) != "Run OpenCode commands" {
		t.Errorf("description = %q", str(t, fn["description"]))
	}
	if str(t, fn["name"]) != "bash" {
		t.Error("tool name must never change")
	}
	// parameters 内部文本不应被改（只改顶层 description）
	pd := fn["parameters"].(map[string]any)["description"]
	if str(t, pd) != "OpenClaw param" {
		t.Errorf("parameters must not be rewritten, got %q", str(t, pd))
	}
}

func TestResponsesFlatToolDescription(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem,
		IncludeTools: true, Enabled: true,
	})
	body := mustJSON(t, map[string]any{
		"model": "m",
		"input": "hi",
		"tools": []any{map[string]any{
			"type": "function", "name": "read", "description": "Read OpenClaw files",
		}},
	})
	if res := e.Apply("zen", "responses", body); res.Total != 1 {
		t.Fatalf("expected 1 replacement, got %d", res.Total)
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if str(t, tool["description"]) != "Read OpenCode files" {
		t.Errorf("description = %q", str(t, tool["description"]))
	}
	if str(t, tool["name"]) != "read" {
		t.Error("tool name must never change")
	}
}

// ---- 匹配语义 -------------------------------------------------------------

// TestCaseInsensitiveWhenFlagIsOff 验证 CaseSensitive=false 时忽略大小写。
// 注意：这是逐规则可选的开关，默认值为 true（区分大小写）。
func TestCaseInsensitiveWhenFlagIsOff(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "openclaw", Replace: "OpenCode", Scope: store.ScopeSystem,
		CaseSensitive: false, Enabled: true,
	})
	body := chatBody(map[string]any{"role": "system", "content": "OpenClaw and OPENCLAW and openclaw"})
	res := e.Apply("zen", "chat", body)
	if res.Total != 3 {
		t.Fatalf("case-insensitive should match all 3 variants, got %d", res.Total)
	}
	if got := str(t, body["messages"].([]any)[0].(map[string]any)["content"]); got != "OpenCode and OpenCode and OpenCode" {
		t.Errorf("content = %q", got)
	}
}

// TestCaseSensitiveMatchesExactly 验证 CaseSensitive=true 时只匹配大小写完全一致的文本
// （这是默认值：避免忽略大小写带来额外误伤）。
func TestCaseSensitiveMatchesExactly(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem,
		CaseSensitive: true, Enabled: true,
	})
	body := chatBody(map[string]any{"role": "system", "content": "OpenClaw openclaw OPENCLAW"})
	res := e.Apply("zen", "chat", body)
	if res.Total != 1 {
		t.Errorf("case-sensitive should match once, got %d", res.Total)
	}
	if got := str(t, body["messages"].([]any)[0].(map[string]any)["content"]); got != "OpenCode openclaw OPENCLAW" {
		t.Errorf("content = %q, want only the exact-case occurrence replaced", got)
	}
}

func TestRegexWithCaptureGroup(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: `OpenClaw[- ]?(\w+)?`, Replace: "OpenCode$1", IsRegex: true,
		Scope: store.ScopeSystem, Enabled: true,
	})
	body := chatBody(map[string]any{"role": "system", "content": "OpenClaw CLI and OpenClaw-Code"})
	e.Apply("zen", "chat", body)
	got := str(t, body["messages"].([]any)[0].(map[string]any)["content"])
	if !strings.Contains(got, "OpenCode") || strings.Contains(got, "OpenClaw") {
		t.Errorf("regex replacement = %q", got)
	}
}

func TestSpecialCharsInLiteralAreEscaped(t *testing.T) {
	// 字面量中含正则元字符时必须按普通字符处理
	e := newEngine(t, store.RewriteRule{
		Match: "a.b(c)", Replace: "X", Scope: store.ScopeSystem, Enabled: true,
	})
	body := chatBody(map[string]any{"role": "system", "content": "a.b(c) and axbxc"})
	e.Apply("zen", "chat", body)
	got := str(t, body["messages"].([]any)[0].(map[string]any)["content"])
	if got != "X and axbxc" {
		t.Errorf("literal with metachars = %q, want %q", got, "X and axbxc")
	}
}

func TestNoSelfAmplification(t *testing.T) {
	// 规则 A 把 X 换成 XX：单次替换不得递归放大
	e := newEngine(t, store.RewriteRule{
		Match: "X", Replace: "XX", Scope: store.ScopeSystem, Enabled: true,
	})
	body := chatBody(map[string]any{"role": "system", "content": "X"})
	e.Apply("zen", "chat", body)
	if got := str(t, body["messages"].([]any)[0].(map[string]any)["content"]); got != "XX" {
		t.Errorf("content = %q, want XX (no recursive expansion)", got)
	}
}

func TestChainedRulesApplyInOrder(t *testing.T) {
	// 规则按 sort_order 依次执行，允许链式效果
	e := newEngine(t,
		store.RewriteRule{ID: 1, Match: "A", Replace: "B", Scope: store.ScopeSystem, Enabled: true, SortOrder: 1},
		store.RewriteRule{ID: 2, Match: "B", Replace: "C", Scope: store.ScopeSystem, Enabled: true, SortOrder: 2},
	)
	body := chatBody(map[string]any{"role": "system", "content": "A"})
	res := e.Apply("zen", "chat", body)
	if got := str(t, body["messages"].([]any)[0].(map[string]any)["content"]); got != "C" {
		t.Errorf("chained rules = %q, want C", got)
	}
	if res.Total != 2 {
		t.Errorf("total = %d, want 2", res.Total)
	}
	if res.Hits[1] != 1 || res.Hits[2] != 1 {
		t.Errorf("hits = %+v", res.Hits)
	}
}

// ---- 隔离与健壮性 ---------------------------------------------------------

func TestModuleIsolation(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Module: "zen", Match: "OpenClaw", Replace: "OpenCode",
		Scope: store.ScopeSystem, Enabled: true,
	})
	zenBody := chatBody(map[string]any{"role": "system", "content": "OpenClaw"})
	if res := e.Apply("zen", "chat", zenBody); res.Total != 1 {
		t.Errorf("zen rule should apply to zen, got %d", res.Total)
	}
	clineBody := chatBody(map[string]any{"role": "system", "content": "OpenClaw"})
	if res := e.Apply("cline", "chat", clineBody); res.Total != 0 {
		t.Errorf("zen rule must not affect cline, got %d", res.Total)
	}
}

func TestGlobalRuleAppliesToBothModules(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Module: store.ModuleAll, Match: "OpenClaw", Replace: "OpenCode",
		Scope: store.ScopeSystem, Enabled: true,
	})
	for _, module := range []string{"zen", "cline"} {
		body := chatBody(map[string]any{"role": "system", "content": "OpenClaw"})
		if res := e.Apply(module, "chat", body); res.Total != 1 {
			t.Errorf("global rule should apply to %s, got %d", module, res.Total)
		}
	}
}

func TestNilAndEdgeValuesDoNotPanic(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: "X", Replace: "Y", Scope: store.ScopeMessages, IncludeTools: true, Enabled: true,
	})
	cases := []map[string]any{
		nil,
		{},
		{"messages": nil},
		{"messages": "not-an-array"},
		{"messages": []any{"not-an-object", 42, nil}},
		{"messages": []any{map[string]any{"role": "system"}}},
		{"messages": []any{map[string]any{"role": "system", "content": nil}}},
		{"messages": []any{map[string]any{"role": "system", "content": 42}}},
		{"messages": []any{map[string]any{"role": "system", "content": []any{"raw-string"}}}},
		{"input": 42, "instructions": 7},
		{"tools": []any{"bad", nil, 3}},
		{"tools": []any{map[string]any{"function": "not-a-map"}}},
	}
	for i, body := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("case %d panicked: %v", i, r)
				}
			}()
			e.Apply("zen", "chat", body)
			e.Apply("zen", "responses", body)
		}()
	}
}

func TestNoRulesMeansNoChange(t *testing.T) {
	e := newEngine(t)
	body := chatBody(map[string]any{"role": "system", "content": "OpenClaw"})
	res := e.Apply("zen", "chat", body)
	if res.Total != 0 {
		t.Errorf("no rules should mean no change, got %d", res.Total)
	}
	if got := str(t, body["messages"].([]any)[0].(map[string]any)["content"]); got != "OpenClaw" {
		t.Errorf("content = %q", got)
	}
}

func TestHitsAccumulateAndFlush(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		ID: 9, Match: "X", Replace: "Y", Scope: store.ScopeSystem, Enabled: true,
	})
	for i := 0; i < 3; i++ {
		body := chatBody(map[string]any{"role": "system", "content": "X X"})
		e.Apply("zen", "chat", body)
	}
	hits := e.FlushHits()
	if hits[9] != 6 {
		t.Errorf("hits = %d, want 6", hits[9])
	}
	// Flush 后应清空
	if again := e.FlushHits(); len(again) != 0 {
		t.Errorf("second flush should be empty, got %+v", again)
	}
}

func TestReloadPicksUpChanges(t *testing.T) {
	loader := &fakeLoader{rules: map[string][]store.RewriteRule{}}
	e := New(loader, nil)

	body := chatBody(map[string]any{"role": "system", "content": "OpenClaw"})
	if res := e.Apply("zen", "chat", body); res.Total != 0 {
		t.Fatal("no rules yet")
	}

	loader.rules["zen"] = []store.RewriteRule{{
		ID: 1, Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem, Enabled: true,
	}}
	e.Reload()

	body2 := chatBody(map[string]any{"role": "system", "content": "OpenClaw"})
	if res := e.Apply("zen", "chat", body2); res.Total != 1 {
		t.Errorf("reload should pick up new rule, got %d", res.Total)
	}
}

func TestInvalidRuleIsSkippedButOthersWork(t *testing.T) {
	e := newEngine(t,
		store.RewriteRule{ID: 1, Match: "a*", IsRegex: true, Scope: store.ScopeSystem, Enabled: true},
		store.RewriteRule{ID: 2, Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem, Enabled: true},
	)
	body := chatBody(map[string]any{"role": "system", "content": "OpenClaw"})
	res := e.Apply("zen", "chat", body)
	if res.Total != 1 {
		t.Fatalf("valid rule should still apply, got %d", res.Total)
	}
	if got := str(t, body["messages"].([]any)[0].(map[string]any)["content"]); got != "OpenCode" {
		t.Errorf("content = %q", got)
	}
	if len(e.Rules("zen")) != 1 {
		t.Errorf("invalid rule should be dropped, got %d rules", len(e.Rules("zen")))
	}
}

func TestPreview(t *testing.T) {
	e := newEngine(t,
		store.RewriteRule{ID: 1, Name: "品牌", Match: "OpenClaw", Replace: "OpenCode",
			Scope: store.ScopeSystem, Enabled: true},
		store.RewriteRule{ID: 2, Name: "版本", Match: `v(\d+)`, Replace: "ver$1", IsRegex: true,
			Scope: store.ScopeSystem, Enabled: true},
	)
	out, hits := e.Preview("zen", "You are OpenClaw v2 built on OpenClaw.")
	if out != "You are OpenCode ver2 built on OpenCode." {
		t.Errorf("preview = %q", out)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hit entries, got %+v", hits)
	}
	if hits[0].RuleID != 1 || hits[0].Count != 2 {
		t.Errorf("first hit = %+v", hits[0])
	}
	// Preview 不得污染命中统计
	if h := e.FlushHits(); len(h) != 0 {
		t.Errorf("preview must not record hits, got %+v", h)
	}
}

func TestExpansionGuard(t *testing.T) {
	// 构造一个会大幅膨胀的规则：把单个字符替换成很长的文本
	long := strings.Repeat("Z", 100)
	e := newEngine(t, store.RewriteRule{
		Match: `a`, Replace: long, IsRegex: true, Scope: store.ScopeSystem, Enabled: true,
	})
	content := strings.Repeat("a", 50) // 50 次替换 × 100 字符 = 5000，远超 10 倍上限
	body := chatBody(map[string]any{"role": "system", "content": content})
	e.Apply("zen", "chat", body)
	got := str(t, body["messages"].([]any)[0].(map[string]any)["content"])
	if got != content {
		t.Errorf("expansion guard should keep original text when expansion is excessive")
	}
}

func TestApplyIsConcurrencySafe(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		ID: 1, Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeMessages, Enabled: true,
	})
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				body := chatBody(map[string]any{"role": "system", "content": "OpenClaw"})
				e.Apply("zen", "chat", body)
				e.Reload()
				e.FlushHits()
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}

// TestConcurrentApplyDoesNotLoseHits 验证并发改写不会丢失命中计数。
//
// 回归保护：命中累加曾是 atomic.Pointer 上的「读-改-写」序列，
// 并发下会丢失更新（实测 32 并发 × 100 次时丢失 36%）。现由互斥锁保护。
func TestConcurrentApplyDoesNotLoseHits(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		ID: 1, Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeMessages, Enabled: true,
	})

	const workers = 32
	const perWorker = 100
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				body := chatBody(map[string]any{"role": "system", "content": "OpenClaw"})
				e.Apply("zen", "chat", body)
			}
		}()
	}
	wg.Wait()

	want := int64(workers * perWorker)
	if got := e.FlushHits()[1]; got != want {
		t.Errorf("lost hits under concurrency: got %d, want %d", got, want)
	}
}

// TestConcurrentFlushAndApply 覆盖「后台定期落盘」与「请求路径累加」并发的情形。
func TestConcurrentFlushAndApply(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		ID: 1, Match: "X", Replace: "Y", Scope: store.ScopeMessages, Enabled: true,
	})

	var wg sync.WaitGroup
	var total int64
	var mu sync.Mutex

	// 消费者：模拟周期落盘
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			for _, n := range e.FlushHits() {
				mu.Lock()
				total += n
				mu.Unlock()
			}
		}
	}()

	// 生产者：模拟并发请求
	const workers = 16
	const perWorker = 100
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				body := chatBody(map[string]any{"role": "system", "content": "X"})
				e.Apply("zen", "chat", body)
			}
		}()
	}
	wg.Wait()

	// 收尾：把剩余的也取出
	for _, n := range e.FlushHits() {
		total += n
	}

	want := int64(workers * perWorker)
	if total != want {
		t.Errorf("flush+apply race lost hits: got %d, want %d", total, want)
	}
}

// TestLiteralModeTreatsReplacementAsLiteral 验证字面量匹配时，替换文本中的 $ 不被解释。
//
// 回归保护：此前统一用 ReplaceAllString，导致字面量规则的替换文本里
// "$1" 被当作（不存在的）捕获组引用而被吃掉，实测 "ver$1" 变成 "ver"。
func TestLiteralModeTreatsReplacementAsLiteral(t *testing.T) {
	cases := []struct {
		name    string
		match   string
		replace string
		input   string
		want    string
	}{
		{"美元+数字", "OpenClaw", "$100", "OpenClaw", "$100"},
		{"捕获组引用", "v(\\d+)", "ver$1", "v(\\d+)", "ver$1"},
		{"命名捕获组", "X", "${name}", "X", "${name}"},
		{"双美元", "X", "$$", "X", "$$"},
		{"美元在中间", "X", "a$1b", "X", "a$1b"},
		{"反斜杠", "X", `\1`, "X", `\1`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEngine(t, store.RewriteRule{
				Match: c.match, Replace: c.replace, IsRegex: false,
				Scope: store.ScopeSystem, Enabled: true,
			})
			body := chatBody(map[string]any{"role": "system", "content": c.input})
			e.Apply("zen", "chat", body)
			got := str(t, body["messages"].([]any)[0].(map[string]any)["content"])
			if got != c.want {
				t.Errorf("literal replace: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRegexModeStillSupportsCaptureGroups 确认修复没有破坏正则模式的捕获组功能。
func TestRegexModeStillSupportsCaptureGroups(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		Match: `v(\d+)\.(\d+)`, Replace: "ver$1-$2", IsRegex: true,
		Scope: store.ScopeSystem, Enabled: true,
	})
	body := chatBody(map[string]any{"role": "system", "content": "version v4.1 here"})
	e.Apply("zen", "chat", body)
	got := str(t, body["messages"].([]any)[0].(map[string]any)["content"])
	if got != "version ver4-1 here" {
		t.Errorf("regex capture groups: got %q, want %q", got, "version ver4-1 here")
	}
}

// TestResetHitsDiscardsPending 验证 ResetHits 会丢弃尚未落库的计数，
// 避免管理端「清零」后被内存缓冲回弹（回归保护）。
func TestResetHitsDiscardsPending(t *testing.T) {
	e := newEngine(t, store.RewriteRule{
		ID: 1, Match: "X", Replace: "Y", Scope: store.ScopeSystem, Enabled: true,
	})
	for i := 0; i < 5; i++ {
		body := chatBody(map[string]any{"role": "system", "content": "X"})
		e.Apply("zen", "chat", body)
	}
	if got := e.FlushHits()[1]; got != 5 {
		t.Fatalf("hits = %d, want 5", got)
	}

	// 再累加后清零：应彻底丢弃
	body := chatBody(map[string]any{"role": "system", "content": "X"})
	e.Apply("zen", "chat", body)
	e.ResetHits()
	if got := e.FlushHits(); len(got) != 0 {
		t.Errorf("after reset, pending hits must be discarded, got %+v", got)
	}
}
