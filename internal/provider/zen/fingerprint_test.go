package zen

import (
	"net/http"
	"regexp"
	"testing"

	"zengateway/internal/provider"
	"zengateway/internal/store"
)

// 上游按官方客户端指纹判请求来源：UA 必须是 opencode/<channel>/<version>/<name>，
// project 必须是裸 40 位小写 hex。这些用例把形状钉死，避免以后回归。
var (
	hex40          = regexp.MustCompile(`^[0-9a-f]{40}$`)
	userAgentShape = regexp.MustCompile(`^opencode/[^/]+/[^/]+/[^/]+$`)
)

func TestUserAgentMirrorsClientShape(t *testing.T) {
	cases := map[string]string{
		"2.0.29": "opencode/latest/2.0.29/cli",
		"":       "opencode/latest/" + fallbackVersion + "/cli",
	}
	for version, want := range cases {
		got := userAgent(version)
		if got != want {
			t.Errorf("userAgent(%q) = %q, want %q", version, got, want)
		}
		if !userAgentShape.MatchString(got) {
			t.Errorf("userAgent(%q) = %q is not opencode/<channel>/<version>/<name>", version, got)
		}
	}
}

// TestBuildHeadersMirrorsClientFingerprint 锁定完整指纹：
// UA 形状、两个会话头同值、project 形状、client 名，且不带已废弃的 x-opencode-request。
func TestBuildHeadersMirrorsClientFingerprint(t *testing.T) {
	down := http.Header{}
	down.Set("User-Agent", "python-requests/2.0")
	down.Set("Cookie", "secret=1")
	down.Set("x-stainless-arch", "arm64")
	down.Set("x-opencode-request", "msg_ef5920720ffc83HCMpIIJDrDdM")
	down.Set("x-opencode-project", "prj_0123456789abcdef01234567") // canonical 值会 403

	p := New("https://zen.example/v1", "", nil)
	h := p.BuildHeaders(down, store.APIKey{APIKey: "sk-test"}, provider.Versions{Zen: "2.0.29"})

	if got := h.Get("User-Agent"); got != "opencode/latest/2.0.29/cli" {
		t.Errorf("User-Agent = %q", got)
	}
	if got := h.Get("x-opencode-client"); got != "cli" {
		t.Errorf("x-opencode-client = %q, want cli", got)
	}
	session := h.Get("x-opencode-session")
	for _, header := range []string{"x-opencode-session-id", "x-session-affinity", "X-Session-Id"} {
		if got := h.Get(header); got != session {
			t.Errorf("%s = %q, want %q", header, got, session)
		}
	}
	if got := h.Get("x-opencode-project"); !hex40.MatchString(got) {
		t.Errorf("x-opencode-project = %q, want 40 lowercase hex", got)
	}
	if got := h.Get("x-opencode-request"); got != "" {
		t.Errorf("x-opencode-request = %q, want absent", got)
	}
	for _, header := range []string{"Cookie", "x-stainless-arch"} {
		if got := h.Get(header); got != "" {
			t.Errorf("%s must not be forwarded, got %q", header, got)
		}
	}
	if got := h.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
}

// TestProjectHeaderIsStableAndBareHex 验证同一 project 信号稳定映射到同一 40hex，
// 且下游的 prj_* 原值不会被直接发出。
func TestProjectHeaderIsStableAndBareHex(t *testing.T) {
	p := New("", "", nil)
	project := func(v string) string {
		down := http.Header{}
		if v != "" {
			down.Set("x-opencode-project", v)
		}
		return p.BuildHeaders(down, store.APIKey{APIKey: "public", IsAnonymous: true},
			provider.Versions{Zen: "2.0.29"}).Get("x-opencode-project")
	}

	first := project("prj_0123456789abcdef01234567")
	if !hex40.MatchString(first) {
		t.Fatalf("project = %q, want 40 lowercase hex", first)
	}
	if got := project("prj_0123456789abcdef01234567"); got != first {
		t.Errorf("project mapping unstable: %q vs %q", got, first)
	}
	if got := project("another-project"); got == first {
		t.Error("different project signals should map to different ids")
	}
	if got := project(""); got == first || !hex40.MatchString(got) {
		t.Errorf("default project = %q, want a distinct 40hex id", got)
	}
}

// TestAnonymousLaneUsesInferenceBase 验证匿名免费层与 Key 层使用不同上游。
func TestAnonymousLaneUsesInferenceBase(t *testing.T) {
	p := New("https://zen.example/v1", "https://inference.example/v1", nil)

	if got := p.UpstreamBaseFor(store.APIKey{APIKey: "public", IsAnonymous: true}); got != "https://inference.example/v1" {
		t.Errorf("anonymous base = %q", got)
	}
	if got := p.UpstreamBaseFor(store.APIKey{APIKey: "sk-test"}); got != "https://zen.example/v1" {
		t.Errorf("keyed base = %q", got)
	}

	// 未显式指定时回落默认值
	def := New("", "", nil)
	if got := def.UpstreamBase(); got != DefaultUpstream {
		t.Errorf("UpstreamBase = %q, want %q", got, DefaultUpstream)
	}
	if got := def.UpstreamBaseFor(store.APIKey{IsAnonymous: true}); got != DefaultAnonymousUpstream {
		t.Errorf("anonymous base = %q, want %q", got, DefaultAnonymousUpstream)
	}
}

// TestInjectedCoreToolsUseGenuineDefinitions 验证注入的核心工具带官方真实定义
// （上游逐字节比对同名工具，stub 会被判伪客户端而 403）。
func TestInjectedCoreToolsUseGenuineDefinitions(t *testing.T) {
	p := New("", "", nil)

	body := map[string]any{"model": "m", "stream": true}
	p.PrepareBody(provider.KindChat, body)

	tools, _ := body["tools"].([]any)
	if len(tools) != len(coreTools) {
		t.Fatalf("injected %d tools, want %d", len(tools), len(coreTools))
	}
	seen := map[string]bool{}
	for _, item := range tools {
		fn, ok := item.(map[string]any)["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool is not chat-shaped: %+v", item)
		}
		name, _ := fn["name"].(string)
		seen[name] = true
		def, known := knownTool(name)
		if !known {
			t.Errorf("injected tool %q has no genuine definition", name)
			continue
		}
		if fn["description"] != def.Description {
			t.Errorf("tool %q did not use the genuine description", name)
		}
		if params, _ := fn["parameters"].(map[string]any); len(params) == 0 {
			t.Errorf("tool %q has empty parameters", name)
		}
	}
	for _, name := range coreTools {
		if !seen[name] {
			t.Errorf("core tool %q was not injected", name)
		}
	}
	// 1.x 的 bash 名字上游认识但没有真实定义，绝不能注入。
	if seen["bash"] {
		t.Error("bash must not be injected: a synthesised definition is rejected")
	}
}

// TestEnsureToolsRewritesRecognisedNames 验证下游声明的同名工具被真实定义改写，
// 而未识别的工具名原样保留。
func TestEnsureToolsRewritesRecognisedNames(t *testing.T) {
	p := New("", "", nil)
	body := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{
			"name": "read", "description": "my own reader",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		map[string]any{"type": "function", "function": map[string]any{
			"name": "get_weather", "description": "Weather lookup",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	}}
	p.PrepareBody(provider.KindChat, body)

	byName := map[string]map[string]any{}
	for _, item := range body["tools"].([]any) {
		fn := item.(map[string]any)["function"].(map[string]any)
		byName[fn["name"].(string)] = fn
	}
	def, _ := knownTool("read")
	if byName["read"]["description"] != def.Description {
		t.Error("recognised tool name kept its downstream definition")
	}
	if byName["get_weather"]["description"] != "Weather lookup" {
		t.Error("unknown tool name was rewritten")
	}
	for _, name := range coreTools {
		if _, ok := byName[name]; !ok {
			t.Errorf("core tool %q was not injected", name)
		}
	}
}

// TestEnsureToolsResponsesShape 验证 responses 协议使用平铺的 function 形状，
// 并同样携带真实定义。
func TestEnsureToolsResponsesShape(t *testing.T) {
	p := New("", "", nil)
	body := map[string]any{"model": "m"}
	p.PrepareBody(provider.KindResponses, body)

	tools, _ := body["tools"].([]any)
	if len(tools) != len(coreTools) {
		t.Fatalf("injected %d tools, want %d", len(tools), len(coreTools))
	}
	for _, item := range tools {
		m := item.(map[string]any)
		if m["type"] != "function" {
			t.Errorf("responses tool type = %v", m["type"])
		}
		if _, nested := m["function"]; nested {
			t.Error("responses tools must not use the chat nesting")
		}
		name, _ := m["name"].(string)
		def, known := knownTool(name)
		if !known || m["description"] != def.Description {
			t.Errorf("responses tool %q did not use the genuine definition", name)
		}
	}
}

// TestPartialToolListOnlyFillsGaps 验证已有部分核心工具时只补缺失项。
func TestPartialToolListOnlyFillsGaps(t *testing.T) {
	p := New("", "", nil)
	def, _ := knownTool("shell")
	body := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{
			"name": "shell", "description": def.Description, "parameters": def.Parameters,
		}},
	}}
	p.PrepareBody(provider.KindChat, body)

	tools, _ := body["tools"].([]any)
	if len(tools) != len(coreTools) {
		t.Fatalf("tools = %d, want %d", len(tools), len(coreTools))
	}
	if name := chatToolName(tools[0].(map[string]any)); name != "shell" {
		t.Errorf("existing shell tool must be kept in place, got %q", name)
	}
}
