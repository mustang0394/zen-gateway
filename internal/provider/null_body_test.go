package provider_test

import (
	"encoding/json"
	"testing"

	"zengateway/internal/provider/zen"
)

// TestNullJsonBodyDoesNotPanic 覆盖 body 为字面量 null 的边界场景。
func TestNullJsonBodyDoesNotPanic(t *testing.T) {
	fake := newFakeUpstream(t, scripted{status: 200, body: `{"ok":true}`})
	st := newStore(t)
	h := newHandler(t, zen.New(fake.srv.URL), st)

	// JSON null 与 {} 均可解码为 map（null 得到 nil map，已回退为空对象），
	// 因此应正常转发；数组/字符串/数字无法解码为对象，应返回 400。
	// 关键在于：任何一种都不能让 handler panic（panic 会导致没有响应写入）。
	cases := map[string]int{
		"null":  200,
		"{}":    200,
		"[]":    400,
		`"str"`: 400,
		"123":   400,
	}
	for body, want := range cases {
		rec := postJSON(t, h, "/zen/v1/chat/completions", "", body)
		if rec.Code != want {
			t.Errorf("body %q -> %d, want %d", body, rec.Code, want)
		}
	}

	// 规范化后的 upstream body 必须是合法对象（而不是 null）
	calls := fake.calls()
	if len(calls) == 0 {
		t.Fatal("no upstream calls recorded")
	}
	var sent map[string]any
	if err := json.Unmarshal(calls[0].Body, &sent); err != nil {
		t.Fatalf("upstream body is not a JSON object: %v (%s)", err, calls[0].Body)
	}
	if sent["stream"] != true {
		t.Errorf("null body should still be normalized with stream=true, got %s", calls[0].Body)
	}
}
