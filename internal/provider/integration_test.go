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
	"zengateway/internal/router"
	"zengateway/internal/store"
)

// TestStreamingStatsEndToEnd 验证流式响应下的 token 用量与 TTFT 会被正确采集，
// 并且 SSE 内容被完整透传给下游（转发与统计互不影响）。
func TestStreamingStatsEndToEnd(t *testing.T) {
	const sse = "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120,\"prompt_tokens_details\":{\"cached_tokens\":64}}}\n\n" +
		"data: [DONE]\n\n"

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 分块写出并 flush，模拟真实流式
		for _, part := range []string{sse[:40], sse[40:120], sse[120:]} {
			io.WriteString(w, part)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer up.Close()

	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "zen", Label: "A", APIKey: "public", Enabled: true, IsAnonymous: true})

	h := provider.NewHandler(zen.New(up.URL), provider.Runtime{
		Store: st,
		Versions: func() provider.Versions {
			return provider.Versions{Zen: "1.18.34"}
		},
	})
	api := newTestAPI(h, nil, map[string]string{"zen": ""})

	req := httptest.NewRequest(http.MethodPost, "/zen/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.5-free","stream":true,"messages":[]}`))
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	// 下游必须收到完整 SSE
	body := rec.Body.String()
	for _, want := range []string{"你", "好", "[DONE]", "prompt_tokens"} {
		if !strings.Contains(body, want) {
			t.Errorf("downstream body missing %q", want)
		}
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("content-type = %q", rec.Header().Get("Content-Type"))
	}

	if err := st.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	day := time.Now().Format(store.DayLayout)
	sum, err := st.SummaryFor(day, "zen")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.PromptTokens != 100 || sum.CompletionTokens != 20 {
		t.Errorf("tokens not recorded: %+v", sum)
	}
	if sum.CachedTokens != 64 {
		t.Errorf("cached tokens = %d, want 64", sum.CachedTokens)
	}
	// 本地假上游响应可能在亚毫秒级完成，因此断言"至少采集到一次 TTFT"，
	// 而不是断言数值大于 0（真实上游为数百毫秒）。
	if sum.TTFTCount != 1 {
		t.Errorf("TTFT samples = %d, want 1: %+v", sum.TTFTCount, sum)
	}
	if sum.CacheHitRate != 64 {
		t.Errorf("cache hit rate = %v, want 64", sum.CacheHitRate)
	}
}

// TestZenCooldownUntilNextMidnightEndToEnd 验证 zen 在 429 后切换到下一个 key，
// 且冷却时间落在次日 0 点附近。
func TestZenCooldownUntilNextMidnightEndToEnd(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"rate limited"}`)
			return
		}
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer up.Close()

	st := newStore(t)
	a, _ := st.CreateKey(store.APIKey{Module: "zen", Label: "A", APIKey: "public", Enabled: true, IsAnonymous: true})
	b, _ := st.CreateKey(store.APIKey{Module: "zen", Label: "B", APIKey: "public", Enabled: true, IsAnonymous: true})

	h := provider.NewHandler(zen.New(up.URL), provider.Runtime{
		Store: st,
		Versions: func() provider.Versions {
			return provider.Versions{Zen: "1.18.34"}
		},
	})
	api := newTestAPI(h, nil, map[string]string{"zen": ""})

	req := httptest.NewRequest(http.MethodPost, "/zen/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.5-free","messages":[]}`))
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected success after switching key, got %d body=%s", rec.Code, rec.Body.String())
	}

	list, err := st.ListCooldowns("zen", time.Now())
	if err != nil {
		t.Fatalf("list cooldowns: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 cooldown, got %d", len(list))
	}
	if list[0].KeyID != a.ID {
		t.Errorf("cooldown should apply to key A (%d), got %d", a.ID, list[0].KeyID)
	}
	// 冷却应到次日 0 点：剩余时间 <= 24h 且 > 0
	remaining := time.Until(list[0].Until)
	if remaining <= 0 || remaining > 24*time.Hour {
		t.Errorf("zen cooldown should end at next midnight, remaining = %v", remaining)
	}
	midnight := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, 1)
	if d := list[0].Until.Sub(midnight); d > time.Minute || d < -time.Minute {
		t.Errorf("cooldown until %v, expected ~%v", list[0].Until, midnight)
	}
	_ = b
}

// TestFullRouterIntegration 通过真实 router 验证路径分发、鉴权与统计串联。
func TestFullRouterIntegration(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+"|"+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "zen", Label: "z", APIKey: "public", Enabled: true, IsAnonymous: true})
	st.CreateKey(store.APIKey{Module: "cline", Label: "c", APIKey: "sk-cline", Enabled: true})

	versions := func() provider.Versions {
		return provider.Versions{Zen: "1.18.34", ClineCLI: "4.1.22", ClineSDK: "0.0.90"}
	}
	zenH := provider.NewHandler(zen.New(up.URL), provider.Runtime{Store: st, Versions: versions})
	clineH := provider.NewHandler(cline.New(up.URL, time.Hour), provider.Runtime{Store: st, Versions: versions})

	tokens := map[string]string{"zen": "", "cline": "gw-cline"}
	rt := router.New(map[string]router.ModuleHandler{"zen": zenH, "cline": clineH},
		func(m string) string { return tokens[m] }, nil)

	// cline 需要 token
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/cline/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	rt.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("cline without token = %d, want 401", rec.Code)
	}

	// cline 带 token → 走上游 chat/completions，并带池中 key
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/cline/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer gw-cline")
	rt.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("cline with token = %d", rec.Code)
	}

	// zen 匿名 → 走上游 /chat/completions
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/zen/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	rt.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("zen anonymous = %d", rec.Code)
	}

	// zen responses → 上游 /responses
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/zen/v1/responses",
		strings.NewReader(`{"model":"m","input":"hi"}`))
	rt.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("zen responses = %d", rec.Code)
	}

	// cline responses → 上游 responses
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/cline/v1/responses",
		strings.NewReader(`{"model":"m","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer gw-cline")
	rt.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("cline responses = %d", rec.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	// cline 的 httptest 服务器只看到业务路径（/v1 已在 Provider 基址中），
	// zen 同理。两个模块对同一 kind 的路径在此层完全一致。
	want := []string{
		"/chat/completions|Bearer sk-cline",
		"/chat/completions|Bearer public",
		"/responses|Bearer public",
		"/responses|Bearer sk-cline",
	}
	if len(seen) != len(want) {
		t.Fatalf("upstream calls = %d, want %d: %v", len(seen), len(want), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, seen[i], want[i])
		}
	}

	// 统计应记录到两个模块
	if err := st.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	day := time.Now().Format(store.DayLayout)
	z, _ := st.SummaryFor(day, "zen")
	c, _ := st.SummaryFor(day, "cline")
	if z.Requests != 2 || c.Requests != 2 {
		t.Errorf("stats: zen=%d cline=%d, want 2 / 2", z.Requests, c.Requests)
	}
}

// TestClineRequestHeadersReachUpstream 验证 cline 上游实际收到的请求头与要求逐项一致，
// 且下游传入的自定义头不会泄漏到上游。
func TestClineRequestHeadersReachUpstream(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk-up", Enabled: true})

	h := provider.NewHandler(cline.New(up.URL, time.Hour), provider.Runtime{
		Store: st,
		Versions: func() provider.Versions {
			return provider.Versions{ClineCLI: "4.1.22", ClineSDK: "0.0.90"}
		},
	})
	api := newTestAPI(nil, h, map[string]string{"cline": "t"})

	req := httptest.NewRequest(http.MethodPost, "/cline/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("User-Agent", "evil/1.0")
	req.Header.Set("X-Leak", "should-not-appear")
	req.Header.Set("Cookie", "session=secret")

	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}

	for k, want := range map[string]string{
		"Authorization":      "Bearer sk-up",
		"Http-Referer":       "https://cline.bot",
		"X-Title":            "Cline",
		"X-Is-Multiroot":     "false",
		"X-Client-Version":   "4.1.22",
		"X-Platform":         "cli",
		"X-Platform-Version": "4.1.22",
		"X-Core-Version":     "0.0.90",
		"User-Agent":         "Cline/4.1.22",
		"X-Client-Type":      "cline-cli",
	} {
		if v := got.Get(k); v != want {
			t.Errorf("upstream header %s = %q, want %q", k, v, want)
		}
	}
	if got.Get("X-Leak") != "" {
		t.Error("downstream X-Leak must not leak upstream")
	}
	if got.Get("Cookie") != "" {
		t.Error("downstream Cookie must not leak upstream")
	}
}

// TestZenHeadersReachUpstream 验证 zen 送出的身份要素完整（决定能否通过上游校验）。
func TestZenHeadersReachUpstream(t *testing.T) {
	var got http.Header
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	st := newStore(t)
	st.CreateKey(store.APIKey{Module: "zen", APIKey: "public", Enabled: true, IsAnonymous: true})

	h := provider.NewHandler(zen.New(up.URL), provider.Runtime{
		Store: st,
		Versions: func() provider.Versions {
			return provider.Versions{Zen: "1.18.34"}
		},
	})
	api := newTestAPI(h, nil, map[string]string{"zen": ""})

	req := httptest.NewRequest(http.MethodPost, "/zen/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":false}`))
	req.Header.Set("User-Agent", "openai-python/1.0")
	req.Header.Set("X-Stainless-Lang", "python")
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ua := got.Get("User-Agent"); ua != "opencode/1.18.34" {
		t.Errorf("User-Agent = %q, want opencode/1.18.34", ua)
	}
	for _, k := range []string{"X-Opencode-Session", "X-Opencode-Request"} {
		if got.Get(k) == "" {
			t.Errorf("%s must be injected", k)
		}
	}
	if got.Get("X-Opencode-Client") != "cli" || got.Get("X-Opencode-Project") != "global" {
		t.Error("x-opencode-client/project defaults missing")
	}
	if got.Get("X-Stainless-Lang") != "" {
		t.Error("x-stainless-* must be dropped")
	}

	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if body["stream"] != true {
		t.Error("stream must be forced true upstream")
	}
	tools, _ := body["tools"].([]any)
	names := map[string]bool{}
	for _, tr := range tools {
		m := tr.(map[string]any)
		fn := m["function"].(map[string]any)
		names[fn["name"].(string)] = true
	}
	if !names["bash"] || !names["read"] {
		t.Errorf("bash and read tools must be injected, got %v", names)
	}
}

// TestCline429ParsesModelFromError 验证冷却用的是上游错误信息中的模型名。
func TestCline429ParsesModelFromError(t *testing.T) {
	msg := `{"error":"Daily free limit reached on model z-ai/glm-5.3-flash. Try again in 45m"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, msg)
	}))
	defer up.Close()

	st := newStore(t)
	k, _ := st.CreateKey(store.APIKey{Module: "cline", APIKey: "sk", Enabled: true})

	h := provider.NewHandler(cline.New(up.URL, time.Hour), provider.Runtime{
		Store: st,
		Versions: func() provider.Versions {
			return provider.Versions{ClineCLI: "4.1.22", ClineSDK: "0.0.90"}
		},
		MaxKeySwitches: 1, // 只有一个 key，冷却后立即返回 429
	})
	api := newTestAPI(nil, h, map[string]string{"cline": "t"})

	req := httptest.NewRequest(http.MethodPost, "/cline/v1/chat/completions",
		strings.NewReader(`{"model":"z-ai/glm-5.3-flash"}`))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)

	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	list, _ := st.ListCooldowns("cline", time.Now())
	if len(list) != 1 {
		t.Fatalf("expected 1 cooldown, got %d", len(list))
	}
	if list[0].Model != "z-ai/glm-5.3-flash" {
		t.Errorf("cooled model = %q, want z-ai/glm-5.3-flash", list[0].Model)
	}
	remaining := time.Until(list[0].Until)
	if remaining < 45*time.Minute || remaining > 47*time.Minute {
		t.Errorf("cooldown remaining = %v, want ~46m (45m + 1m margin)", remaining)
	}
	if list[0].KeyID != k.ID {
		t.Errorf("cooldown key = %d, want %d", list[0].KeyID, k.ID)
	}
	if !strings.Contains(list[0].Detail, "45m") {
		t.Errorf("detail should keep upstream message, got %q", list[0].Detail)
	}
}
