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

	"zengateway/internal/inject"
	"zengateway/internal/prompt"
	"zengateway/internal/provider"
	"zengateway/internal/provider/cline"
	"zengateway/internal/provider/zen"
	"zengateway/internal/store"
)

// captureUpstream 记录上游收到的请求体。
type captureUpstream struct {
	mu     sync.Mutex
	bodies []map[string]any
	tools  [][]any
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
		if tools, ok := body["tools"].([]any); ok {
			c.tools = append(c.tools, tools)
		}
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

func testVersions() provider.Versions {
	return provider.Versions{Zen: "1.18.34", ClineCLI: "4.1.22", ClineSDK: "0.0.90"}
}

// newInjectEngine 用 store 创建带关键词的注入引擎。
func newInjectEngine(t *testing.T, st *store.Store, kws ...store.Keyword) *inject.Engine {
	t.Helper()
	for _, k := range kws {
		if _, err := st.CreateKeyword(k); err != nil {
			t.Fatalf("create keyword: %v", err)
		}
	}
	return inject.New(st, nil)
}

// TestZenReplacesSystemPromptOnKeywordHit 验证端到端：
// 下游 system 提示词命中关键词时，上游收到的是 zen 原生提示词，
// 且网关注入的 bash/read stub 与 stream 仍正常。
func TestZenReplacesSystemPromptOnKeywordHit(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	eng := newInjectEngine(t, st, store.Keyword{
		Module: "zen", Keyword: "Claude Code", Note: "第三方客户端", Enabled: true,
	})
	h := newTestAPI(provider.NewHandler(zen.New(up.srv.URL, eng),
		provider.Runtime{Store: st, Versions: testVersions}), nil, map[string]string{"zen": ""})

	rec := postJSON(t, h, "/zen/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"system","content":"You are Claude Code, a CLI tool."},{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	sent := up.last(t)
	msgs := sent["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)

	// 必须是内置 zen 提示词（逐字节相等）
	if sys != prompt.Module("zen") {
		t.Errorf("system prompt should be replaced by the builtin zen prompt, got %q", truncate(sys, 120))
	}
	if !strings.Contains(sys, "You are opencode") {
		t.Error("replaced prompt should be the opencode one")
	}
	// 用户消息保留
	if got, _ := msgs[1].(map[string]any)["content"].(string); got != "hi" {
		t.Errorf("user message must be preserved, got %q", got)
	}
	// stream 与工具注入仍正常
	if sent["stream"] != true {
		t.Error("stream must still be forced")
	}
	tools, _ := sent["tools"].([]any)
	names := map[string]bool{}
	for _, tv := range tools {
		fn := tv.(map[string]any)["function"].(map[string]any)
		names[fn["name"].(string)] = true
	}
	if !names["bash"] || !names["read"] {
		t.Errorf("bash/read stubs must still be injected, got %v", names)
	}
}

// TestZenNoKeywordHitKeepsPrompt 验证未命中时提示词保持原样。
func TestZenNoKeywordHitKeepsPrompt(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	eng := newInjectEngine(t, st, store.Keyword{Module: "zen", Keyword: "Claude Code", Enabled: true})
	h := newTestAPI(provider.NewHandler(zen.New(up.srv.URL, eng),
		provider.Runtime{Store: st, Versions: testVersions}), nil, map[string]string{"zen": ""})

	const original = "You are a helpful coding assistant."
	postJSON(t, h, "/zen/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"system","content":"`+original+`"}]}`)

	sent := up.last(t)
	msgs := sent["messages"].([]any)
	if got, _ := msgs[0].(map[string]any)["content"].(string); got != original {
		t.Errorf("prompt should be untouched without a keyword hit, got %q", got)
	}
}

// TestKeywordMatchingIsCaseInsensitive 验证忽略大小写。
func TestKeywordMatchingIsCaseInsensitive(t *testing.T) {
	for _, variant := range []string{
		"You are Claude Code", "you are claude code", "YOU ARE CLAUDE CODE",
	} {
		up := newCaptureUpstream(t)
		st := newStore(t)
		eng := newInjectEngine(t, st, store.Keyword{Module: "zen", Keyword: "Claude Code", Enabled: true})
		h := newTestAPI(provider.NewHandler(zen.New(up.srv.URL, eng),
			provider.Runtime{Store: st, Versions: testVersions}), nil, map[string]string{"zen": ""})

		postJSON(t, h, "/zen/v1/chat/completions", "",
			`{"model":"m","messages":[{"role":"system","content":"`+variant+`"}]}`)

		sent := up.last(t)
		got, _ := sent["messages"].([]any)[0].(map[string]any)["content"].(string)
		if got != prompt.Module("zen") {
			t.Errorf("variant %q should trigger injection", variant)
		}
		up.srv.Close()
	}
}

// TestModulesAreIsolatedForInjection 验证 zen 关键词不影响 cline。
func TestModulesAreIsolatedForInjection(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk", Enabled: true})
	eng := newInjectEngine(t, st, store.Keyword{Module: "zen", Keyword: "Claude Code", Enabled: true})

	// cline 无内置提示词，显式给个覆盖值以便对比
	if err := st.SetPromptOverride("cline", "cline native prompt"); err != nil {
		t.Fatalf("set override: %v", err)
	}
	eng.Reload()

	h := newTestAPI(nil, provider.NewHandler(cline.New(up.srv.URL, time.Hour, eng),
		provider.Runtime{Store: st, Versions: testVersions}), map[string]string{"cline": "gw"})

	const original = "You are Claude Code"
	postJSON(t, h, "/cline/v1/chat/completions", "gw",
		`{"model":"m","messages":[{"role":"system","content":"`+original+`"}]}`)

	sent := up.last(t)
	if got, _ := sent["messages"].([]any)[0].(map[string]any)["content"].(string); got != original {
		t.Errorf("zen keyword must not affect cline, got %q", got)
	}
}

// TestGlobalKeywordAppliesToBothModules 验证 module='*' 对两个模块都生效。
func TestGlobalKeywordAppliesToBothModules(t *testing.T) {
	st := newStore(t)
	eng := newInjectEngine(t, st, store.Keyword{Module: store.ModuleAll, Keyword: "Claude Code", Enabled: true})
	if err := st.SetPromptOverride("cline", "cline native prompt"); err != nil {
		t.Fatalf("set override: %v", err)
	}
	eng.Reload()

	// zen 走内置提示词
	upZen := newCaptureUpstream(t)
	zenH := newTestAPI(provider.NewHandler(zen.New(upZen.srv.URL, eng),
		provider.Runtime{Store: st, Versions: testVersions}), nil, map[string]string{"zen": ""})
	postJSON(t, zenH, "/zen/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"system","content":"You are Claude Code"}]}`)
	if got, _ := upZen.last(t)["messages"].([]any)[0].(map[string]any)["content"].(string); got != prompt.Module("zen") {
		t.Error("zen should use the builtin prompt for a global keyword")
	}

	// cline 走覆盖值
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk", Enabled: true})
	upCline := newCaptureUpstream(t)
	clineH := newTestAPI(nil, provider.NewHandler(cline.New(upCline.srv.URL, time.Hour, eng),
		provider.Runtime{Store: st, Versions: testVersions}), map[string]string{"cline": "gw"})
	postJSON(t, clineH, "/cline/v1/chat/completions", "gw",
		`{"model":"m","messages":[{"role":"system","content":"You are Claude Code"}]}`)
	if got, _ := upCline.last(t)["messages"].([]any)[0].(map[string]any)["content"].(string); got != "cline native prompt" {
		t.Errorf("cline should use its own prompt, got %q", got)
	}
}

// TestPromptOverrideTakesEffect 验证管理端覆盖值优先于内置提示词。
func TestPromptOverrideTakesEffect(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	eng := newInjectEngine(t, st, store.Keyword{Module: "zen", Keyword: "Claude Code", Enabled: true})

	if err := st.SetPromptOverride("zen", "MY CUSTOM PROMPT"); err != nil {
		t.Fatalf("set override: %v", err)
	}
	eng.Reload()

	h := newTestAPI(provider.NewHandler(zen.New(up.srv.URL, eng),
		provider.Runtime{Store: st, Versions: testVersions}), nil, map[string]string{"zen": ""})
	postJSON(t, h, "/zen/v1/chat/completions", "",
		`{"model":"m","messages":[{"role":"system","content":"You are Claude Code"}]}`)

	if got, _ := up.last(t)["messages"].([]any)[0].(map[string]any)["content"].(string); got != "MY CUSTOM PROMPT" {
		t.Errorf("override should win, got %q", got)
	}
}

// TestStructuredFieldsNeverInjected 验证 model、工具名等结构化字段绝不被改动。
func TestStructuredFieldsNeverInjected(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	eng := newInjectEngine(t, st, store.Keyword{Module: store.ModuleAll, Keyword: "Claude Code", Enabled: true})
	h := newTestAPI(provider.NewHandler(zen.New(up.srv.URL, eng),
		provider.Runtime{Store: st, Versions: testVersions}), nil, map[string]string{"zen": ""})

	body := `{"model":"ClaudeCode-model","temperature":0.3,
		"messages":[{"role":"system","content":"You are Claude Code"}],
		"tools":[{"type":"function","function":{"name":"CC_tool","description":"Claude Code helper"}}]}`
	postJSON(t, h, "/zen/v1/chat/completions", "", body)

	sent := up.last(t)
	if sent["model"] != "ClaudeCode-model" {
		t.Errorf("model must never change, got %v", sent["model"])
	}
	if sent["temperature"] != 0.3 {
		t.Error("temperature must be preserved")
	}
	fn := sent["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "CC_tool" || fn["description"] != "Claude Code helper" {
		t.Errorf("tools must never be modified, got %+v", fn)
	}
	if got, _ := sent["messages"].([]any)[0].(map[string]any)["content"].(string); got != prompt.Module("zen") {
		t.Error("system prompt should have been injected")
	}
}

// TestResponsesInstructionsInjected 验证 responses 的 instructions 被替换。
func TestResponsesInstructionsInjected(t *testing.T) {
	up := newCaptureUpstream(t)
	st := newStore(t)
	eng := newInjectEngine(t, st, store.Keyword{Module: "zen", Keyword: "Claude Code", Enabled: true})
	h := newTestAPI(provider.NewHandler(zen.New(up.srv.URL, eng),
		provider.Runtime{Store: st, Versions: testVersions}), nil, map[string]string{"zen": ""})

	postJSON(t, h, "/zen/v1/responses", "",
		`{"model":"m","instructions":"You are Claude Code.","input":"hi"}`)

	sent := up.last(t)
	if got, _ := sent["instructions"].(string); got != prompt.Module("zen") {
		t.Errorf("instructions should be replaced, got %q", truncate(got, 80))
	}
	if got, _ := sent["input"].(string); got != "hi" {
		t.Errorf("input must be untouched, got %q", got)
	}
}

// TestPrepareBodyNilSafety 验证两个模块的 PrepareBody 都能安全处理 nil body。
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
