package provider_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zengateway/internal/provider"
	"zengateway/internal/provider/zen"
)

// TestAnonymousFallbackRespectsCooldown 验证 key 池为空时回落到内置 public 条目，
// 该条目在 429 冷却后也必须被跳过，而不是无脑继续请求上游。
func TestAnonymousFallbackRespectsCooldown(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer up.Close()

	st := newStore(t) // 故意不添加任何 key
	h := provider.NewHandler(zen.New(up.URL, nil), provider.Runtime{
		Store:    st,
		Versions: func() provider.Versions { return provider.Versions{Zen: "1.18.34"} },
	})
	api := newTestAPI(h, nil, map[string]string{"zen": ""})

	// 第一次请求：触发 429，匿名条目应进入冷却
	req := httptest.NewRequest(http.MethodPost, "/zen/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.5-free"}`))
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("first request = %d, want 429", rec.Code)
	}

	mu.Lock()
	afterFirst := calls
	mu.Unlock()
	if afterFirst != 1 {
		t.Fatalf("upstream calls after first = %d, want 1", afterFirst)
	}

	// 第二次请求：匿名条目已在冷却中，不应再打上游
	req = httptest.NewRequest(http.MethodPost, "/zen/v1/chat/completions",
		strings.NewReader(`{"model":"mimo-v2.5-free"}`))
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, req)

	mu.Lock()
	afterSecond := calls
	mu.Unlock()
	if afterSecond != afterFirst {
		t.Errorf("cooled-down anonymous key must not hit upstream again: calls %d -> %d",
			afterFirst, afterSecond)
	}

	list, _ := st.ListCooldowns("zen", time.Now())
	if len(list) != 1 {
		t.Fatalf("expected 1 cooldown for anonymous entry, got %d", len(list))
	}
}
