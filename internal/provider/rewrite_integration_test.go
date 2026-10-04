package provider_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zengateway/internal/provider"
	"zengateway/internal/provider/cline"
	"zengateway/internal/provider/zen"
	"zengateway/internal/rewrite"
	"zengateway/internal/store"
)

// captureUpstream 记录上游收到的请求体。
type captureUpstream struct {
	mu     sync.Mutex
	bodies []map[string]any
	srv    *httptest.Server
}

func newCaptureUpstream(t *testing.T) *captureUpstream {
	t.Helper()
	c := &captureUpstream{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				body = map[string]any{"_raw": string(raw)}
			}
		}
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *captureUpstream) last(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("upstream received no request")
	}
	return c.bodies[len(c.bodies)-1]
}

// testVersions 提供固定的版本号（BuildHeaders 依赖它，不能为 nil）。
func testVersions() provider.Versions {
	return provider.Versions{Zen: "1.18.34", ClineCLI: "4.1.22", ClineSDK: "0.0.90"}
}

// newRewriteEngine 用 store 创建带规则的引擎。
func newRewriteEngine(t *testing.T, st *store.Store, rules ...store.RewriteRule) *rewrite.Engine {
	t.Helper()
	for _, r := range rules {
		if _, err := st.CreateRewriteRule(r); err != nil {
			t.Fatalf("create rule: %v", err)
		}
	}
	return rewrite.New(st, nil)
}

// TestZenRewritesSystemPromptAndKeepsInjectedStubs 验证：
//  1. 下游 system 提示词中的关键词被替换
//  2. 网关注入的 bash/read stub 描述不被用户规则误改（改写发生在前）
func TestZenRewritesSystemPromptAndKeepsInjectedStubs(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	// 规则刻意匹配 "shell"，它出现在网关 stub 的描述 "Run a shell command." 中
	eng := newRewriteEngine(t, st,
		store.RewriteRule{
			Module: "zen", Match: "OpenClaw", Replace: "OpenCode",
			Scope: store.ScopeSystem, Enabled: true,
		},
		store.RewriteRule{
			Module: "zen", Match: "shell", Replace: "TERMINAL",
			Scope: store.ScopeSystem, IncludeTools: true, Enabled: true,
		},
	)
	h := newTestAPI(provider.NewHandler(zen.New(up.srv.URL, eng), provider.Runtime{Store: st, Versions: testVersions}),
		nil, map[string]string{"zen": ""})

	rec := postJSON(t, h, "/zen/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"system","content":"You are OpenClaw, use the shell."}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	sent := up.last(t)
	msgs := sent["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"]; got != "You are OpenCode, use the TERMINAL." {
		t.Errorf("system prompt = %q", got)
	}

	// 注入的 stub 描述必须保持原样（"Run a shell command." 不能被改成 TERMINAL）
	tools := sent["tools"].([]any)
	for _, tv := range tools {
		fn := tv.(map[string]any)["function"].(map[string]any)
		if fn["name"] == "bash" {
			if got := fn["description"]; got != "Run a shell command." {
				t.Errorf("injected bash stub must not be rewritten, got %q", got)
			}
		}
	}
	// stream 仍被强制为 true
	if sent["stream"] != true {
		t.Error("stream must still be forced")
	}
}

// TestClineRewritesSystemPrompt 验证 cline 模块同样生效。
func TestClineRewritesSystemPrompt(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk", Enabled: true})
	eng := newRewriteEngine(t, st, store.RewriteRule{
		Module: "cline", Match: "OpenClaw", Replace: "OpenCode",
		Scope: store.ScopeSystemFirstUser, Enabled: true,
	})
	h := newTestAPI(nil, provider.NewHandler(cline.New(up.srv.URL, time.Hour, eng), provider.Runtime{Store: st, Versions: testVersions}),
		map[string]string{"cline": "gw-token"})

	rec := postJSON(t, h, "/cline/v1/chat/completions", "gw-token",
		`{"model":"m","messages":[{"role":"system","content":"You are OpenClaw"},{"role":"user","content":"help me with OpenClaw"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}

	sent := up.last(t)
	msgs := sent["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"]; got != "You are OpenCode" {
		t.Errorf("cline system = %q", got)
	}
	// 默认作用域含首条 user
	if got := msgs[1].(map[string]any)["content"]; got != "help me with OpenCode" {
		t.Errorf("cline first user = %q", got)
	}
}

// TestRewriteRespectsModuleIsolation 验证 zen 规则不影响 cline 请求。
func TestRewriteRespectsModuleIsolation(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk", Enabled: true})
	eng := newRewriteEngine(t, st, store.RewriteRule{
		Module: "zen", Match: "OpenClaw", Replace: "OpenCode",
		Scope: store.ScopeMessages, Enabled: true,
	})
	h := newTestAPI(nil, provider.NewHandler(cline.New(up.srv.URL, time.Hour, eng), provider.Runtime{Store: st, Versions: testVersions}),
		map[string]string{"cline": "gw-token"})

	postJSON(t, h, "/cline/v1/chat/completions", "gw-token",
		`{"model":"m","messages":[{"role":"system","content":"OpenClaw"}]}`)

	sent := up.last(t)
	got := sent["messages"].([]any)[0].(map[string]any)["content"]
	if got != "OpenClaw" {
		t.Errorf("zen-only rule must not affect cline, got %q", got)
	}
}

// TestResponsesInstructionsRewritten 验证 responses 的 instructions 被改写。
func TestResponsesInstructionsRewritten(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	eng := newRewriteEngine(t, st, store.RewriteRule{
		Module: store.ModuleAll, Match: "OpenClaw", Replace: "OpenCode",
		Scope: store.ScopeSystem, Enabled: true,
	})
	h := newTestAPI(provider.NewHandler(zen.New(up.srv.URL, eng), provider.Runtime{Store: st, Versions: testVersions}),
		nil, map[string]string{"zen": ""})

	rec := postJSON(t, h, "/zen/v1/responses", "",
		`{"model":"m","instructions":"You are OpenClaw.","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	sent := up.last(t)
	if got := sent["instructions"]; got != "You are OpenCode." {
		t.Errorf("instructions = %q", got)
	}
	// 纯字符串 input 在 scope=system 下不改
	if got := sent["input"]; got != "hi" {
		t.Errorf("input must be untouched, got %q", got)
	}
}

// TestRewriteDisabledWhenNoRules 验证无规则时请求体不被改动（除 zen 的必要改写）。
func TestRewriteDisabledWhenNoRules(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	eng := newRewriteEngine(t, st) // 无规则
	h := newTestAPI(provider.NewHandler(zen.New(up.srv.URL, eng), provider.Runtime{Store: st, Versions: testVersions}),
		nil, map[string]string{"zen": ""})

	postJSON(t, h, "/zen/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"system","content":"OpenClaw stays"}]}`)

	sent := up.last(t)
	if got := sent["messages"].([]any)[0].(map[string]any)["content"]; got != "OpenClaw stays" {
		t.Errorf("with no rules content must be unchanged, got %q", got)
	}
}

// TestStructuredFieldsNeverRewritten 验证 model、工具名等结构字段绝不参与替换。
func TestStructuredFieldsNeverRewritten(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	eng := newRewriteEngine(t, st, store.RewriteRule{
		Module: store.ModuleAll, Match: "OpenClaw", Replace: "OpenCode",
		Scope: store.ScopeMessages, IncludeTools: true, Enabled: true,
	})
	h := newTestAPI(provider.NewHandler(zen.New(up.srv.URL, eng), provider.Runtime{Store: st, Versions: testVersions}),
		nil, map[string]string{"zen": ""})

	body := `{"model":"OpenClaw-model","messages":[{"role":"system","content":"x"}],
		"tools":[{"type":"function","function":{"name":"OpenClaw_tool","description":"Use OpenClaw"}}]}`
	postJSON(t, h, "/zen/v1/chat/completions", "", body)

	sent := up.last(t)
	// model 绝不改写（否则路由/计费会错）
	if got := sent["model"]; got != "OpenClaw-model" {
		t.Errorf("model must never be rewritten, got %q", got)
	}
	fn := sent["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if got := fn["name"]; got != "OpenClaw_tool" {
		t.Errorf("tool name must never be rewritten, got %q", got)
	}
	if got := fn["description"]; got != "Use OpenCode" {
		t.Errorf("tool description should be rewritten when enabled, got %q", got)
	}
}

// TestRewriteSurvivesInvalidRuleInStore 验证库中存在无效规则时不影响转发。
func TestRewriteSurvivesInvalidRuleInStore(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	// 绕过 API 校验直接写入一条「匹配空串」的危险规则
	if err := st.SetSetting("_", "_"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := st.CreateRewriteRule(store.RewriteRule{
		Module: "zen", Match: "x*", IsRegex: true, Scope: store.ScopeMessages, Enabled: true,
	}); err != nil {
		t.Fatalf("create dangerous rule: %v", err)
	}
	eng := newRewriteEngine(t, st, store.RewriteRule{
		Module: "zen", Match: "OpenClaw", Replace: "OpenCode", Scope: store.ScopeSystem, Enabled: true,
	})
	h := newTestAPI(provider.NewHandler(zen.New(up.srv.URL, eng), provider.Runtime{Store: st, Versions: testVersions}),
		nil, map[string]string{"zen": ""})

	rec := postJSON(t, h, "/zen/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"system","content":"OpenClaw x x x"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	sent := up.last(t)
	got := sent["messages"].([]any)[0].(map[string]any)["content"].(string)
	// 无效规则被跳过，有效规则照常生效，文本未被逐字符插入
	if got != "OpenCode x x x" {
		t.Errorf("content = %q, want %q", got, "OpenCode x x x")
	}
	if strings.Count(got, "x") != 3 {
		t.Errorf("dangerous rule must be skipped, content = %q", got)
	}
}

// TestPrepareBodyNilSafety 验证两个模块的 PrepareBody 都能安全处理 nil body。
// 回归保护：zen.PrepareBody 内部会写 body["stream"]/body["tools"]，
// 在 nil map 上写入会 panic（曾实测触发 "assignment to entry in nil map"）。
func TestPrepareBodyNilSafety(t *testing.T) {
	t.Run("zen", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("zen.PrepareBody(nil) panicked: %v", r)
			}
		}()
		if changed := zen.New("", nil).PrepareBody(provider.KindChat, nil); changed {
			t.Error("nil body should report no change")
		}
		if changed := zen.New("", nil).PrepareBody(provider.KindResponses, nil); changed {
			t.Error("nil body should report no change")
		}
	})
	t.Run("cline", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("cline.PrepareBody(nil) panicked: %v", r)
			}
		}()
		if changed := cline.New("", time.Hour, nil).PrepareBody(provider.KindChat, nil); changed {
			t.Error("nil body should report no change")
		}
	})
}

// TestPrepareBodyEmptyMapSafety 验证空对象（非 nil）仍按预期补齐 stream 与工具。
func TestPrepareBodyEmptyMapSafety(t *testing.T) {
	body := map[string]any{}
	if changed := zen.New("", nil).PrepareBody(provider.KindChat, body); !changed {
		t.Error("empty body should be changed (stream + tools injected)")
	}
	if body["stream"] != true {
		t.Error("stream should be forced true")
	}
	if tools, _ := body["tools"].([]any); len(tools) != 2 {
		t.Errorf("expected 2 injected tools, got %d", len(tools))
	}
}
