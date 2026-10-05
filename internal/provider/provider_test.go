package provider_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zengateway/internal/provider"
	"zengateway/internal/provider/cline"
	"zengateway/internal/provider/zen"
	"zengateway/internal/store"
)

// fakeUpstream 是按脚本返回响应序列的假上游。
type fakeUpstream struct {
	mu       sync.Mutex
	script   []scripted
	requests []capturedRequest
	srv      *httptest.Server
}

type scripted struct {
	status int
	body   string
}

type capturedRequest struct {
	Path    string
	Header  http.Header
	Body    []byte
	Method  string
	Query   string
	Counter int
}

func newFakeUpstream(t *testing.T, script ...scripted) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, capturedRequest{
			Path: r.URL.Path, Header: r.Header.Clone(), Body: body, Method: r.Method,
		})
		idx := len(f.requests) - 1
		var step scripted
		if idx < len(f.script) {
			step = f.script[idx]
		} else if len(f.script) > 0 {
			step = f.script[len(f.script)-1]
		} else {
			step = scripted{status: http.StatusOK, body: `{"ok":true}`}
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.status)
		io.WriteString(w, step.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) calls() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedRequest(nil), f.requests...)
}

func newHandler(t *testing.T, p provider.Provider, st *store.Store) *testAPI {
	t.Helper()
	h := provider.NewHandler(p, provider.Runtime{
		Store: st,
		Log:   slog.New(slog.DiscardHandler),
		Versions: func() provider.Versions {
			return provider.Versions{Zen: "1.18.31", ClineCLI: "4.1.22", ClineSDK: "0.0.90"}
		},
		RetryBackoff: []time.Duration{0, 0, 0}, // 测试中不等待
	})
	api := &testAPI{tokens: map[string]string{"cline": "gw-token", "zen": ""}}
	if p.Name() == "cline" {
		api.clineH = h
		api.zenH = provider.NewHandler(zen.New("http://127.0.0.1:1", nil), provider.Runtime{
			Store: st, Log: slog.New(slog.DiscardHandler),
			Versions: func() provider.Versions { return provider.Versions{Zen: "1.18.31"} },
		})
	} else {
		api.zenH = h
		api.clineH = provider.NewHandler(cline.New("http://127.0.0.1:1", time.Hour, nil), provider.Runtime{
			Store: st, Log: slog.New(slog.DiscardHandler),
			Versions: func() provider.Versions { return provider.Versions{ClineCLI: "4.1.22", ClineSDK: "0.0.90"} },
		})
	}
	return api
}

// testAPI 把模块处理器接到与管理端一致的路径分发上，
// 使测试走真实的 /zen/... 与 /cline/... 路由。
type testAPI struct {
	zenH   *provider.Handler
	clineH *provider.Handler
	tokens map[string]string
}

func newTestAPI(zenH, clineH *provider.Handler, tokens map[string]string) *testAPI {
	return &testAPI{zenH: zenH, clineH: clineH, tokens: tokens}
}

func (a *testAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	var (
		h      *provider.Handler
		module string
		kind   provider.Kind
	)
	switch {
	case strings.HasPrefix(path, "/zen/"):
		h, module, path = a.zenH, "zen", strings.TrimPrefix(path, "/zen")
	case strings.HasPrefix(path, "/cline/"):
		h, module, path = a.clineH, "cline", strings.TrimPrefix(path, "/cline")
	default:
		httptest.NewRecorder().WriteHeader(http.StatusNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	rest := strings.TrimPrefix(path, "/v1")
	switch rest {
	case "/chat/completions":
		kind = provider.KindChat
	case "/responses":
		kind = provider.KindResponses
	case "/models":
		kind = provider.KindModels
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	h.Serve(w, r, kind, a.tokens[module])
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func postJSON(t *testing.T, h http.Handler, path, auth string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---- zen 模块 -------------------------------------------------------------

func TestZenBuildHeadersRewriteAndDefaults(t *testing.T) {
	p := zen.New("", nil)
	down := http.Header{}
	down.Set("User-Agent", "evil-client/1.0")
	down.Set("Cookie", "secret=1")
	down.Set("x-stainless-arch", "arm64")
	down.Set("Accept", "text/event-stream")
	down.Set("x-opencode-project", "myproj")

	h := p.BuildHeaders(down, store.APIKey{APIKey: "public", IsAnonymous: true},
		provider.Versions{Zen: "1.18.31"})

	if got := h.Get("User-Agent"); got != "opencode/1.18.31" {
		t.Errorf("User-Agent must be overridden, got %q", got)
	}
	if h.Get("Cookie") != "" {
		t.Error("Cookie must not be forwarded")
	}
	if h.Get("x-stainless-arch") != "" {
		t.Error("x-stainless-* must not be forwarded")
	}
	if got := h.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept should pass through, got %q", got)
	}
	if got := h.Get("Authorization"); got != "Bearer public" {
		t.Errorf("anonymous key should use public, got %q", got)
	}
	if got := h.Get("x-opencode-client"); got != "cli" {
		t.Errorf("x-opencode-client default = %q", got)
	}
	if got := h.Get("x-opencode-project"); got != "myproj" {
		t.Errorf("existing x-opencode-project must be kept, got %q", got)
	}
	if h.Get("x-opencode-session") == "" || h.Get("x-opencode-request") == "" {
		t.Error("session/request IDs must be generated when missing")
	}
}

// TestZenAuthorizationIsNeverDownstreamValue 锁定下游 Authorization 不得透传给上游。
//
// 下游的 Authorization 是网关自己的接入 Token（或客户端随手带的值），
// 与上游凭据无关。早期实现会优先沿用它，导致配置接入 Token 后
// 网关 Token 被当作上游 key 发送，上游一律回 401 Invalid API key。
func TestZenAuthorizationIsNeverDownstreamValue(t *testing.T) {
	cases := []struct {
		name     string
		down     string // 下游 Authorization（空表示不带）
		key      store.APIKey
		wantAuth string
	}{
		{
			name:     "配了池中 key 且下游带接入 Token：必须用池中 key",
			down:     "Bearer my-gateway-token",
			key:      store.APIKey{APIKey: "sk-upstream", IsAnonymous: false},
			wantAuth: "Bearer sk-upstream",
		},
		{
			name:     "匿名 key 且下游带接入 Token：必须回落 public，不得泄露网关 Token",
			down:     "Bearer my-gateway-token",
			key:      store.APIKey{APIKey: "public", IsAnonymous: true},
			wantAuth: "Bearer public",
		},
		{
			name:     "匿名 key 且下游带任意值：仍回落 public",
			down:     "Bearer some-other-token",
			key:      store.APIKey{APIKey: "public", IsAnonymous: true},
			wantAuth: "Bearer public",
		},
		{
			name:     "无 key 且下游不带：回落 public",
			down:     "",
			key:      store.APIKey{},
			wantAuth: "Bearer public",
		},
		{
			name:     "池中 key 为空字符串：回落 public",
			down:     "Bearer my-gateway-token",
			key:      store.APIKey{APIKey: "", IsAnonymous: false},
			wantAuth: "Bearer public",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			down := http.Header{}
			if tc.down != "" {
				down.Set("Authorization", tc.down)
			}
			h := zen.New("", nil).BuildHeaders(down, tc.key, provider.Versions{Zen: "1.18.31"})

			if got := h.Get("Authorization"); got != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", got, tc.wantAuth)
			}
			// 下游值（可能是网关自身接入 Token）绝不得出现在发送给上游的头里
			if tc.down != "" && h.Get("Authorization") == tc.down {
				t.Errorf("downstream Authorization leaked upstream: %q", tc.down)
			}
		})
	}
}

func TestZenPrepareBodyForcesStreamAndTools(t *testing.T) {
	p := zen.New("", nil)
	body := map[string]any{"model": "m", "stream": false}
	p.PrepareBody(provider.KindChat, body)
	if body["stream"] != true {
		t.Error("stream must be forced to true")
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected 2 injected tools, got %d", len(tools))
	}

	// 已有 bash 时只补 read
	body2 := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "bash"}},
	}}
	p.PrepareBody(provider.KindChat, body2)
	tools2, _ := body2["tools"].([]any)
	if len(tools2) != 2 {
		t.Fatalf("expected 1 injected tool, got %d", len(tools2)-1)
	}
	names := []string{}
	for _, tr := range tools2 {
		names = append(names, tr.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if names[0] != "bash" || names[1] != "read" {
		t.Errorf("tools = %v, want [bash read]", names)
	}
}

func TestZenClassifyOnly429(t *testing.T) {
	p := zen.New("", nil)
	if d := p.Classify(http.StatusForbidden, []byte("nope")); d.Action != provider.ActionPass {
		t.Error("zen must not handle 403")
	}
	if d := p.Classify(http.StatusOK, nil); d.Action != provider.ActionPass {
		t.Error("200 must pass")
	}
	d := p.Classify(http.StatusTooManyRequests, []byte("whatever"))
	if d.Action != provider.ActionCooldownAndNext {
		t.Fatal("429 must trigger cooldown")
	}
	if d.Cooldown <= 0 || d.Cooldown > 24*time.Hour {
		t.Errorf("cooldown should be until next midnight, got %v", d.Cooldown)
	}
}

func TestZenEndToEndPassthroughAndStats(t *testing.T) {
	fake := newFakeUpstream(t, scripted{status: 200, body: `{"choices":[]}`})
	st := newStore(t)
	p := zen.New(fake.srv.URL, nil)
	h := newHandler(t, p, st)

	rec := postJSON(t, h, "/zen/v1/chat/completions", "",
		`{"model":"mimo-v2.5-free","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 upstream call, got %d", len(calls))
	}
	if calls[0].Path != "/chat/completions" {
		t.Errorf("upstream path = %q", calls[0].Path)
	}
	// 请求体必须已被改写为流式 + 工具
	var sent map[string]any
	json.Unmarshal(calls[0].Body, &sent)
	if sent["stream"] != true {
		t.Error("upstream body must have stream=true")
	}
	if _, ok := sent["tools"].([]any); !ok {
		t.Error("upstream body must have tools injected")
	}
	// 上游 Header 必须是 opencode UA
	if ua := calls[0].Header.Get("User-Agent"); ua != "opencode/1.18.31" {
		t.Errorf("upstream UA = %q", ua)
	}

	if err := st.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	sum, _ := st.SummaryFor(time.Now().Format(store.DayLayout), "zen")
	if sum.Requests != 1 {
		t.Errorf("stats requests = %d, want 1", sum.Requests)
	}
}

// ---- cline 模块 -----------------------------------------------------------

func TestClineHeadersAreFullySynthetic(t *testing.T) {
	p := cline.New("", time.Hour, nil)
	down := http.Header{}
	down.Set("Authorization", "Bearer downstream-token")
	down.Set("User-Agent", "python-requests/2.0")
	down.Set("X-Custom", "should-be-dropped")
	down.Set("HTTP-Referer", "https://evil.example.com")

	h := p.BuildHeaders(down, store.APIKey{APIKey: "sk-upstream"},
		provider.Versions{ClineCLI: "4.1.22", ClineSDK: "0.0.90"})

	expect := map[string]string{
		"Authorization":      "Bearer sk-upstream",
		"HTTP-Referer":       "https://cline.bot",
		"X-Title":            "Cline",
		"X-IS-MULTIROOT":     "false",
		"X-CLIENT-VERSION":   "4.1.22",
		"X-PLATFORM":         "cli",
		"X-PLATFORM-VERSION": "4.1.22",
		"X-CORE-VERSION":     "0.0.90",
		"User-Agent":         "Cline/4.1.22",
		"X-CLIENT-TYPE":      "cline-cli",
	}
	for k, want := range expect {
		if got := h.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if h.Get("X-Custom") != "" {
		t.Error("downstream headers must be fully ignored")
	}
	if got := h.Get("Accept"); got != "" {
		t.Errorf("Accept should not be set for cline, got %q", got)
	}
}

// TestClineUpstreamCarriesV1InBase 验证 /v1 位于基址中（与 zen 对称），
// PathFor 只返回业务路径，避免拼出 /api/v1/v1/... 这类重复前缀。
func TestClineUpstreamCarriesV1InBase(t *testing.T) {
	p := cline.New("", time.Hour, nil)

	if got := p.UpstreamBase(); got != "https://api.cline.bot/api/v1" {
		t.Errorf("UpstreamBase = %q, want https://api.cline.bot/api/v1", got)
	}
	if strings.Contains(p.PathFor(provider.KindChat), "/v1") {
		t.Errorf("PathFor must not repeat /v1, got %q", p.PathFor(provider.KindChat))
	}

	cases := map[provider.Kind]string{
		provider.KindChat:      "/chat/completions",
		provider.KindResponses: "/responses",
		provider.KindModels:    "/models",
	}
	for kind, want := range cases {
		if got := p.PathFor(kind); got != want {
			t.Errorf("PathFor(%s) = %q, want %q", kind, got, want)
		}
	}

	// 最终 URL 必须与上游约定一致，且不出现重复的 /v1
	for _, kind := range []provider.Kind{provider.KindChat, provider.KindResponses, provider.KindModels} {
		full := p.UpstreamBase() + p.PathFor(kind)
		if strings.Count(full, "/v1") != 1 {
			t.Errorf("full URL %q must contain exactly one /v1", full)
		}
		if !strings.HasPrefix(full, "https://api.cline.bot/api/v1/") {
			t.Errorf("full URL %q must be under /api/v1/", full)
		}
	}

	// 自定义基址同样不应重复 /v1
	custom := cline.New("http://127.0.0.1:9/api/v1", time.Hour, nil)
	if got := custom.UpstreamBase() + custom.PathFor(provider.KindChat); got != "http://127.0.0.1:9/api/v1/chat/completions" {
		t.Errorf("custom base URL = %q", got)
	}
	// 基址末尾斜杠需被规范化
	slash := cline.New("http://127.0.0.1:9/api/v1/", time.Hour, nil)
	if got := slash.UpstreamBase() + slash.PathFor(provider.KindChat); got != "http://127.0.0.1:9/api/v1/chat/completions" {
		t.Errorf("trailing slash base URL = %q", got)
	}
}

func TestParseCooldownVariants(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"Daily free limit reached on model z-ai/glm-5.3-flash. Try again in 22h 59m", 22*time.Hour + 59*time.Minute + time.Minute, true},
		{"Try again in 45m", 46 * time.Minute, true},
		{"Try again in 3h", 3*time.Hour + time.Minute, true},
		{"Try again in 1 hour 30 minutes", 90*time.Minute + time.Minute, true},
		{"no cooldown info here", 0, false},
		{"Try again in 0h 0m", 0, false},
	}
	for _, c := range cases {
		got, ok := cline.ParseCooldown(c.in)
		if ok != c.ok {
			t.Errorf("ParseCooldown(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("ParseCooldown(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestClineRetries403ProductSurfaceThreeTimes(t *testing.T) {
	hint := `{"error":"cline-free/mimo-v2.6-flash is only available via Cline product surfaces. If you are using an old version of Cline, please update to the latest version"}`
	// 前 3 次 403，第 4 次成功
	fake := newFakeUpstream(t,
		scripted{status: 403, body: hint},
		scripted{status: 403, body: hint},
		scripted{status: 403, body: hint},
		scripted{status: 200, body: `{"choices":[]}`},
	)
	st := newStore(t)
	if _, err := st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk-1", Enabled: true}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	rec := postJSON(t, h, "/cline/v1/chat/completions", "gw-token", `{"model":"mimo-v2.6-flash","stream":true}`)
	if rec.Code != 200 {
		t.Fatalf("expected success after retries, got %d body=%s", rec.Code, rec.Body.String())
	}
	// 首次 + 3 次重试 = 4 次上游调用
	if got := len(fake.calls()); got != 4 {
		t.Fatalf("expected 4 upstream calls (1 + 3 retries), got %d", got)
	}
	// 重试必须使用同一个 key
	for i, c := range fake.calls() {
		if got := c.Header.Get("Authorization"); got != "Bearer sk-1" {
			t.Errorf("call %d used auth %q, want same key", i, got)
		}
	}
}

func TestClineReturns403WhenRetriesExhausted(t *testing.T) {
	hint := `{"error":"cline-free/x is only available via Cline product surfaces."}`
	fake := newFakeUpstream(t, scripted{status: 403, body: hint})

	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk-1", Enabled: true})
	keys := 1
	for i := 0; i < keys; i++ {
		// 只有 1 个 key，重试后仍失败
	}
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	rec := postJSON(t, h, "/cline/v1/chat/completions", "gw-token", `{"model":"x","stream":true}`)
	if rec.Code != 403 {
		t.Fatalf("expected 403 after exhausting retries, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "product surfaces") {
		t.Errorf("original error must be returned to downstream, got %s", rec.Body.String())
	}
	if got := len(fake.calls()); got != 4 {
		t.Fatalf("expected 1 + 3 retries = 4 calls, got %d", got)
	}
}

func TestCline403WithoutHintIsNotRetried(t *testing.T) {
	fake := newFakeUpstream(t, scripted{status: 403, body: `{"error":"some other 403"}`})
	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk-1", Enabled: true})
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	rec := postJSON(t, h, "/cline/v1/chat/completions", "gw-token", `{"model":"x"}`)
	if rec.Code != 403 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := len(fake.calls()); got != 1 {
		t.Fatalf("unrelated 403 must not be retried, got %d calls", got)
	}
}

func TestCline429CoolsDownKeyForModelAndSwitches(t *testing.T) {
	limitMsg := `{"error":"Daily free limit reached on model z-ai/glm-5.3-flash. Try again in 22h 59m"}`
	fake := newFakeUpstream(t,
		scripted{status: 429, body: limitMsg}, // 第一个 key 429
		scripted{status: 200, body: `{"choices":[]}`},
	)
	st := newStore(t)
	a, _ := st.CreateKey(store.APIKey{Module: "cline", Label: "A", APIKey: "sk-a", Enabled: true})
	b, _ := st.CreateKey(store.APIKey{Module: "cline", Label: "B", APIKey: "sk-b", Enabled: true})
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	rec := postJSON(t, h, "/cline/v1/chat/completions", "gw-token", `{"model":"z-ai/glm-5.3-flash"}`)
	if rec.Code != 200 {
		t.Fatalf("expected switch to key B and success, got %d", rec.Code)
	}
	calls := fake.calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (A then B), got %d", len(calls))
	}
	if calls[0].Header.Get("Authorization") != "Bearer sk-a" {
		t.Errorf("first call should use key A, got %q", calls[0].Header.Get("Authorization"))
	}
	if calls[1].Header.Get("Authorization") != "Bearer sk-b" {
		t.Errorf("second call should switch to key B, got %q", calls[1].Header.Get("Authorization"))
	}

	// 冷却时长应来自上游文本（22h59m + 1min）
	list, _ := st.ListCooldowns("cline", time.Now())
	if len(list) != 1 {
		t.Fatalf("expected 1 cooldown, got %d", len(list))
	}
	cd := list[0]
	if cd.KeyID != a.ID || cd.Model != "z-ai/glm-5.3-flash" {
		t.Errorf("cooldown target = key %d model %q", cd.KeyID, cd.Model)
	}
	remaining := time.Until(cd.Until)
	if remaining < 22*time.Hour || remaining > 23*time.Hour {
		t.Errorf("cooldown remaining = %v, want ~23h", remaining)
	}

	// key A 对同一模型不可用，但对其他模型仍可用
	if k, err := st.PickKey("cline", "z-ai/glm-5.3-flash", time.Now()); err != nil || k.ID != b.ID {
		t.Errorf("expected only B available for cooled model, got %+v err=%v", k, err)
	}
	if k, err := st.PickKey("cline", "other-model", time.Now()); err != nil || k.ID != a.ID {
		t.Errorf("key A should still serve other models, got %+v err=%v", k, err)
	}
	_ = a
}

func TestClineAllKeysCoolingReturns429(t *testing.T) {
	limitMsg := `{"error":"Daily free limit reached. Try again in 1h 0m"}`
	fake := newFakeUpstream(t, scripted{status: 429, body: limitMsg})
	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk-a", Enabled: true})
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk-b", Enabled: true})
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	rec := postJSON(t, h, "/cline/v1/chat/completions", "gw-token", `{"model":"m"}`)
	if rec.Code != 429 {
		t.Fatalf("expected 429 when all keys cooling, got %d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("error body must be JSON: %v", err)
	}
	if _, ok := payload["error"]; !ok {
		t.Errorf("error body shape invalid: %s", rec.Body.String())
	}
	if got := len(fake.calls()); got != 2 {
		t.Fatalf("expected 2 attempts (one per key), got %d", got)
	}
}

// ---- 下游鉴权 -------------------------------------------------------------

func TestAccessTokenEnforced(t *testing.T) {
	fake := newFakeUpstream(t)
	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk-1", Enabled: true})
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	// 错误 token
	rec := postJSON(t, h, "/cline/v1/chat/completions", "wrong", `{"model":"m"}`)
	if rec.Code != 401 {
		t.Errorf("wrong token should be 401, got %d", rec.Code)
	}
	if len(fake.calls()) != 0 {
		t.Error("unauthorized request must not reach upstream")
	}

	// 正确 token
	rec = postJSON(t, h, "/cline/v1/chat/completions", "gw-token", `{"model":"m"}`)
	if rec.Code != 200 {
		t.Errorf("valid token should pass, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestZenAllowsAnonymousWhenNoToken(t *testing.T) {
	fake := newFakeUpstream(t)
	st := newStore(t)
	h := newHandler(t, zen.New(fake.srv.URL, nil), st)

	rec := postJSON(t, h, "/zen/v1/chat/completions", "", `{"model":"m"}`)
	if rec.Code != 200 {
		t.Errorf("zen without token should be allowed, got %d", rec.Code)
	}
}

func TestClineRequiresKeyInPool(t *testing.T) {
	fake := newFakeUpstream(t)
	st := newStore(t) // 不添加任何 key
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	rec := postJSON(t, h, "/cline/v1/chat/completions", "gw-token", `{"model":"m"}`)
	if rec.Code != 429 && rec.Code != 503 {
		t.Fatalf("expected failure without keys, got %d", rec.Code)
	}
	if len(fake.calls()) != 0 {
		t.Error("no key means no upstream call")
	}
}

// ---- responses 路径 -------------------------------------------------------

func TestResponsesPathBothModules(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mod      string
		wantPath string
	}{
		{"zen", "zen", "/responses"},
		{"cline", "cline", "/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeUpstream(t, scripted{status: 200, body: `{"id":"resp"}`})
			st := newStore(t)
			st.CreateKey(store.APIKey{Module: tc.mod, APIKey: "sk", Enabled: true})

			var p provider.Provider
			if tc.mod == "zen" {
				p = zen.New(fake.srv.URL, nil)
			} else {
				p = cline.New(fake.srv.URL, time.Hour, nil)
			}
			h := newHandler(t, p, st)

			rec := postJSON(t, h, "/"+tc.mod+"/v1/responses", "gw-token", `{"model":"m","input":"hi"}`)
			if rec.Code != 200 {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			calls := fake.calls()
			if len(calls) != 1 || calls[0].Path != tc.wantPath {
				t.Fatalf("upstream path = %q, want %q", calls[0].Path, tc.wantPath)
			}
		})
	}
}

func TestModelsEndpointUsesKeyAndPath(t *testing.T) {
	fake := newFakeUpstream(t, scripted{status: 200, body: `{"free":[
		{"id":"cline-free/mimo-v2.6-flash","name":"Mimo V2.6 Flash","description":"MoE","tags":[]},
		{"id":"stealth/space-bunny-alpha","name":"space-bunny-alpha","description":"fast","tags":[]}
	]}`})
	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk-1", Enabled: true})
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	req := httptest.NewRequest(http.MethodGet, "/cline/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	// cline 没有 OpenAI 风格的 /v1/models，必须改打官方 recommended-models
	calls := fake.calls()
	if len(calls) != 1 || calls[0].Path != "/ai/cline/recommended-models" || calls[0].Method != http.MethodGet {
		t.Fatalf("unexpected upstream call: %+v", calls)
	}
	if got := calls[0].Header.Get("X-CLIENT-VERSION"); got != "4.1.22" {
		t.Errorf("cline models request must carry version headers, got %q", got)
	}
	// 必须用池中配置的 key（而非一律用匿名占位）：key 还决定了 clientFor 选哪个代理。
	if got := calls[0].Header.Get("Authorization"); got != "Bearer sk-1" {
		t.Errorf("cline models must use the configured key, got %q", got)
	}

	// 响应必须已转换为 OpenAI /v1/models 格式，且只含 free 桶
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
			Name    string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	if out.Object != "list" {
		t.Errorf("object = %q, want list", out.Object)
	}
	if len(out.Data) != 2 {
		t.Fatalf("data = %+v, want 2 free models", out.Data)
	}
	if out.Data[0].ID != "cline-free/mimo-v2.6-flash" || out.Data[0].Object != "model" ||
		out.Data[0].OwnedBy != "cline-free" || out.Data[0].Created <= 0 || out.Data[0].Name != "Mimo V2.6 Flash" {
		t.Errorf("entry not in OpenAI shape: %+v", out.Data[0])
	}
	if out.Data[1].OwnedBy != "stealth" {
		t.Errorf("owned_by = %q, want stealth", out.Data[1].OwnedBy)
	}
}

// /cline/v1/models 无需 key：官方 recommended-models 公开可读，
// 因此尚未配置 key（或 key 已全冷却）时也应能查看模型列表，而不是 503。
func TestModelsEndpointWorksWithoutKey(t *testing.T) {
	fake := newFakeUpstream(t, scripted{status: 200,
		body: `{"free":[{"id":"cline-free/mimo-v2.6-flash","name":"Mimo","description":"","tags":[]}]}`})
	st := newStore(t) // 故意不创建任何 key
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	req := httptest.NewRequest(http.MethodGet, "/cline/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s (want 200: models must work without a key)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cline-free/mimo-v2.6-flash") {
		t.Errorf("body = %s", rec.Body.String())
	}
	// 匿名占位 key 仍应带上 Cline 产品头
	calls := fake.calls()
	if len(calls) != 1 || calls[0].Header.Get("X-CLIENT-TYPE") != "cline-cli" {
		t.Errorf("anonymous models request must still carry Cline headers: %+v", calls)
	}
	// 无 key 时应回落到 public，而不能把下游的网关 Token 泄露给上游
	if got := calls[0].Header.Get("Authorization"); got != "Bearer public" {
		t.Errorf("anonymous models request must use Bearer public, got %q", got)
	}
}

// 非 2xx 必须原样透传，让下游看到上游真实错误，而不是被包装成 200 空列表。
func TestModelsEndpointPassesThroughUpstreamError(t *testing.T) {
	fake := newFakeUpstream(t, scripted{status: 503, body: `{"error":"upstream down"}`})
	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk-1", Enabled: true})
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	req := httptest.NewRequest(http.MethodGet, "/cline/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 passthrough", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "upstream down") {
		t.Errorf("upstream error body lost: %s", rec.Body.String())
	}
}

// 上游返回 2xx 但不是合法 recommended-models JSON 时必须报错，
// 不能静默返回空列表把故障掩盖成"没有模型"。
func TestModelsEndpointRejectsMalformedUpstreamBody(t *testing.T) {
	fake := newFakeUpstream(t, scripted{status: 200, body: `<html>not json</html>`})
	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk-1", Enabled: true})
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	req := httptest.NewRequest(http.MethodGet, "/cline/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "convert models response") {
		t.Errorf("body = %s", rec.Body.String())
	}
	// 上游 200 但转换失败时，统计必须与下游实际收到的 502 一致，
	// 否则故障在日志/统计里会被掩盖成一次成功请求。
	if err := st.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	sum, err := st.SummaryFor(time.Now().Format(store.DayLayout), "cline")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Requests != 1 || sum.Errors != 1 {
		t.Errorf("stats = %+v, want requests=1 errors=1 (conversion failure)", sum)
	}
}

// zen 未实现 ModelsProvider，必须保持原来的直通行为（路径与响应均不变）。
func TestZenModelsEndpointStillPassesThrough(t *testing.T) {
	raw := `{"object":"list","data":[{"id":"mimo-v2.5-free","object":"model","created":1,"owned_by":"opencode"}]}`
	fake := newFakeUpstream(t, scripted{status: 200, body: raw})
	st := newStore(t)
	h := newHandler(t, zen.New(fake.srv.URL, nil), st)

	req := httptest.NewRequest(http.MethodGet, "/zen/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	calls := fake.calls()
	if len(calls) != 1 || calls[0].Path != "/models" {
		t.Fatalf("zen models must still hit /models: %+v", calls)
	}
	if rec.Body.String() != raw {
		t.Errorf("zen models must pass through untouched:\n got %s\nwant %s", rec.Body.String(), raw)
	}
}

// ---- 顺序轮询 -------------------------------------------------------------

func TestSequentialKeyPreferenceKeepsCacheWarm(t *testing.T) {
	fake := newFakeUpstream(t)
	st := newStore(t)
	a, _ := st.CreateKey(store.APIKey{Module: "cline", Label: "A", APIKey: "sk-a", Enabled: true})
	st.CreateKey(store.APIKey{Module: "cline", Label: "B", APIKey: "sk-b", Enabled: true})
	h := newHandler(t, cline.New(fake.srv.URL, time.Hour, nil), st)

	// 连续 3 次请求都应优先使用 A（顺序优先，保证上游缓存命中）
	for i := 0; i < 3; i++ {
		rec := postJSON(t, h, "/cline/v1/chat/completions", "gw-token", `{"model":"m"}`)
		if rec.Code != 200 {
			t.Fatalf("request %d failed: %d", i, rec.Code)
		}
	}
	for i, c := range fake.calls() {
		if got := c.Header.Get("Authorization"); got != "Bearer sk-a" {
			t.Errorf("call %d should use key A, got %q", i, got)
		}
	}
	_ = a
}

func TestProxyPerKey(t *testing.T) {
	// 代理配置仅影响客户端构造，这里验证按 key 选择代理不会报错且直连可用
	st := newStore(t)
	k, err := st.CreateKey(store.APIKey{
		Module: "zen", APIKey: "public", Proxy: "http://127.0.0.1:1", Enabled: true, IsAnonymous: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k.Proxy == "" {
		t.Error("proxy must be persisted")
	}
	got, err := st.GetKey(k.ID)
	if err != nil || got.Proxy != "http://127.0.0.1:1" {
		t.Errorf("proxy not round-tripped: %+v err=%v", got, err)
	}
}

func TestLargeBodyLimit(t *testing.T) {
	fake := newFakeUpstream(t)
	st := newStore(t)
	h := newHandler(t, zen.New(fake.srv.URL, nil), st)

	big := bytes.Repeat([]byte("a"), provider.MaxBodyBytes+1024)
	body := `{"model":"m","x":"` + string(big) + `"}`
	rec := postJSON(t, h, "/zen/v1/chat/completions", "", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body should be rejected, got %d", rec.Code)
	}
}

// TestModelsRequestIsCountedInStats 验证 /v1/models 请求也会计入统计
// （此前该路径漏统计，导致 models 调用不可见）。
func TestModelsRequestIsCountedInStats(t *testing.T) {
	fake := newFakeUpstream(t, scripted{status: 200, body: `{"object":"list","data":[]}`})
	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "zen", APIKey: "public", Enabled: true, IsAnonymous: true})
	h := newHandler(t, zen.New(fake.srv.URL, nil), st)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/zen/v1/models", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("call %d: status = %d", i, rec.Code)
		}
	}

	if err := st.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	sum, err := st.SummaryFor(time.Now().Format(store.DayLayout), "zen")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Requests != 3 {
		t.Errorf("models requests = %d, want 3", sum.Requests)
	}
	if sum.Errors != 0 {
		t.Errorf("errors = %d, want 0", sum.Errors)
	}
}

// TestUpstreamFailureIsCountedAsError 验证上游不可达时被计为错误。
func TestUpstreamFailureIsCountedAsError(t *testing.T) {
	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "zen", APIKey: "public", Enabled: true, IsAnonymous: true})
	// 指向一个不监听的端口
	h := newHandler(t, zen.New("http://127.0.0.1:1", nil), st)

	rec := postJSON(t, h, "/zen/v1/chat/completions", "", `{"model":"m"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if err := st.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	sum, _ := st.SummaryFor(time.Now().Format(store.DayLayout), "zen")
	if sum.Errors != 1 {
		t.Errorf("errors = %d, want 1", sum.Errors)
	}
}
