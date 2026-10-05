package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zengateway/internal/prompt"
	"zengateway/internal/store"
)

func createKeyword(t *testing.T, srv *Server, body string) int64 {
	t.Helper()
	rec := call(t, srv, http.MethodPost, "/admin/api/keywords", body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create keyword = %d body=%s", rec.Code, rec.Body.String())
	}
	k := decode(t, rec)["keyword"].(map[string]any)
	return int64(k["id"].(float64))
}

func TestKeywordCRUDViaAPI(t *testing.T) {
	srv, st := newServer(t)

	// 初始为空 + 带模块选项
	rec := call(t, srv, http.MethodGet, "/admin/api/keywords", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	payload := decode(t, rec)
	if len(payload["keywords"].([]any)) != 0 {
		t.Error("expected no keywords initially")
	}
	if len(payload["modules"].([]any)) != 3 {
		t.Errorf("expected 3 module options, got %v", payload["modules"])
	}

	// 新增
	id := createKeyword(t, srv, `{"module":"zen","keyword":"Claude Code","note":"第三方客户端"}`)

	// 引擎热重载后应生效
	if kws := srv.inj.Keywords("zen"); len(kws) != 1 || kws[0].Keyword != "Claude Code" {
		t.Fatalf("engine not reloaded: %+v", kws)
	}

	// 读取
	rec = call(t, srv, http.MethodGet, "/admin/api/keywords?module=zen", "", true)
	items := decode(t, rec)["keywords"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 keyword, got %d", len(items))
	}
	first := items[0].(map[string]any)
	if first["keyword"] != "Claude Code" || first["enabled"] != true || first["isGlobal"] != false {
		t.Errorf("keyword fields wrong: %+v", first)
	}

	// 更新（部分字段）
	rec = call(t, srv, http.MethodPut, "/admin/api/keywords/"+itoa(id),
		`{"keyword":"Cursor","enabled":false}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d body=%s", rec.Code, rec.Body.String())
	}
	updated := decode(t, rec)["keyword"].(map[string]any)
	if updated["keyword"] != "Cursor" || updated["enabled"] != false {
		t.Errorf("update not applied: %+v", updated)
	}
	if updated["note"] != "第三方客户端" {
		t.Errorf("partial update clobbered note: %+v", updated)
	}
	// 停用后引擎不应再使用它
	if kws := srv.inj.Keywords("zen"); len(kws) != 0 {
		t.Errorf("disabled keyword should be dropped from engine, got %+v", kws)
	}

	// 删除
	rec = call(t, srv, http.MethodDelete, "/admin/api/keywords/"+itoa(id), "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	if _, err := st.GetKeyword(id); err != store.ErrNotFound {
		t.Errorf("keyword should be deleted, got %v", err)
	}
}

func TestKeywordValidation(t *testing.T) {
	srv, _ := newServer(t)
	cases := []struct{ name, body string }{
		{"空关键词", `{"module":"zen","keyword":""}`},
		{"纯空白关键词", `{"module":"zen","keyword":"   "}`},
		{"非法模块", `{"module":"other","keyword":"x"}`},
		{"超长关键词", `{"module":"zen","keyword":"` + strings.Repeat("x", 300) + `"}`},
		{"缺少模块", `{"keyword":"x"}`},
	}
	for _, c := range cases {
		rec := call(t, srv, http.MethodPost, "/admin/api/keywords", c.body, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d body=%s", c.name, rec.Code, rec.Body.String())
		}
	}

	// 全局模块合法
	id := createKeyword(t, srv, `{"module":"*","keyword":"Claude Code"}`)
	rec := call(t, srv, http.MethodGet, "/admin/api/keywords", "", true)
	rule := decode(t, rec)["keywords"].([]any)[0].(map[string]any)
	if rule["isGlobal"] != true {
		t.Errorf("global keyword should be flagged: %+v", rule)
	}
	_ = id
}

func TestKeywordPatchNullValuesIgnored(t *testing.T) {
	srv, _ := newServer(t)
	id := createKeyword(t, srv, `{"module":"zen","keyword":"Claude Code","note":"n"}`)

	for _, body := range []string{
		`{"module":null}`, `{"keyword":null}`, `{"note":null}`, `{"enabled":null}`, `{}`,
	} {
		rec := call(t, srv, http.MethodPut, "/admin/api/keywords/"+itoa(id), body, true)
		if rec.Code != http.StatusOK {
			t.Errorf("body %s -> %d, want 200", body, rec.Code)
		}
	}

	rec := call(t, srv, http.MethodGet, "/admin/api/keywords", "", true)
	k := decode(t, rec)["keywords"].([]any)[0].(map[string]any)
	if k["keyword"] != "Claude Code" || k["note"] != "n" || k["enabled"] != true {
		t.Errorf("null patch corrupted keyword: %+v", k)
	}
}

func TestKeywordReorderAndResetHits(t *testing.T) {
	srv, _ := newServer(t)
	a := createKeyword(t, srv, `{"module":"zen","keyword":"alpha"}`)
	b := createKeyword(t, srv, `{"module":"zen","keyword":"beta"}`)

	rec := call(t, srv, http.MethodPost, "/admin/api/keywords/reorder",
		`{"ids":[`+itoa(b)+`,`+itoa(a)+`]}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d body=%s", rec.Code, rec.Body.String())
	}
	kws := srv.inj.Keywords("zen")
	if len(kws) != 2 || kws[0].ID != b {
		t.Fatalf("reorder not applied: %+v", kws)
	}

	if err := srv.store.AddInjectHits(map[int64]int64{a: 5}); err != nil {
		t.Fatalf("add hits: %v", err)
	}
	rec = call(t, srv, http.MethodPost, "/admin/api/keywords/"+itoa(a)+"/reset-hits", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d", rec.Code)
	}
	got, _ := srv.store.GetKeyword(a)
	if got.Hits != 0 {
		t.Errorf("hits = %d, want 0", got.Hits)
	}
}

func TestKeywordTestPanel(t *testing.T) {
	srv, _ := newServer(t)
	createKeyword(t, srv, `{"module":"zen","keyword":"Claude Code","note":"第三方"}`)

	post := func(text string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"module": "zen", "text": text})
		req := httptest.NewRequest(http.MethodPost, "/admin/api/keywords/test", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("test panel = %d body=%s", rec.Code, rec.Body.String())
		}
		return decode(t, rec)
	}

	// 命中（含大小写变化）
	hit := post("I am using CLAUDE CODE today")
	if hit["hit"] != true {
		t.Errorf("should hit: %+v", hit)
	}
	if hit["matchedKeyword"] != "Claude Code" {
		t.Errorf("matchedKeyword = %v", hit["matchedKeyword"])
	}
	if hit["promptSource"] != "builtin" {
		t.Errorf("promptSource = %v, want builtin", hit["promptSource"])
	}
	if hit["hasPrompt"] != true || hit["promptLength"].(float64) < 5000 {
		t.Errorf("should report the builtin prompt: %+v", hit)
	}

	// 未命中
	miss := post("nothing relevant here")
	if miss["hit"] != false {
		t.Errorf("should not hit: %+v", miss)
	}

	// 测试不得写命中统计
	kws, _ := srv.store.ListKeywords("zen")
	for _, k := range kws {
		if k.Hits != 0 {
			t.Errorf("test panel must not record hits, keyword %d hits=%d", k.ID, k.Hits)
		}
	}

	// 非法模块
	rec := call(t, srv, http.MethodPost, "/admin/api/keywords/test", `{"module":"bogus","text":"x"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid module = %d, want 400", rec.Code)
	}
}

func TestPromptOverrideAPI(t *testing.T) {
	srv, _ := newServer(t)

	// 初始：内置提示词，无覆盖
	rec := call(t, srv, http.MethodGet, "/admin/api/prompts?module=zen", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	got := decode(t, rec)
	if got["source"] != "builtin" || got["hasBuiltin"] != true {
		t.Errorf("expected builtin source: %+v", got)
	}
	if got["override"] != "" {
		t.Errorf("override should be empty initially, got %v", got["override"])
	}
	builtinLen := len(got["builtin"].(string))
	if builtinLen < 5000 {
		t.Errorf("builtin prompt looks truncated: %d bytes", builtinLen)
	}
	if got["effective"] != got["builtin"] {
		t.Error("without override, effective should equal builtin")
	}

	// 设置覆盖
	rec = call(t, srv, http.MethodPut, "/admin/api/prompts",
		`{"module":"zen","text":"MY PROMPT"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d body=%s", rec.Code, rec.Body.String())
	}
	if prompt.Resolve("zen") != "MY PROMPT" {
		t.Error("engine should pick up the override")
	}

	rec = call(t, srv, http.MethodGet, "/admin/api/prompts?module=zen", "", true)
	got = decode(t, rec)
	if got["source"] != "override" || got["effective"] != "MY PROMPT" {
		t.Errorf("after override: %+v", got)
	}
	// 内置值仍应返回，便于前端「恢复默认」
	if got["builtin"] == "" {
		t.Error("builtin should still be reported for reset")
	}

	// 空串清除覆盖
	rec = call(t, srv, http.MethodPut, "/admin/api/prompts", `{"module":"zen","text":""}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d", rec.Code)
	}
	if prompt.Source("zen") != "builtin" {
		t.Error("clearing should fall back to builtin")
	}

	// 非法模块 / 缺少 text
	rec = call(t, srv, http.MethodGet, "/admin/api/prompts?module=bogus", "", true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid module = %d, want 400", rec.Code)
	}
	rec = call(t, srv, http.MethodPut, "/admin/api/prompts", `{"module":"zen"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing text = %d, want 400", rec.Code)
	}
}

func TestKeywordAPIRequiresAuth(t *testing.T) {
	srv, _ := newServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/api/keywords"},
		{http.MethodPost, "/admin/api/keywords"},
		{http.MethodPost, "/admin/api/keywords/test"},
		{http.MethodPost, "/admin/api/keywords/reorder"},
		{http.MethodPut, "/admin/api/keywords/1"},
		{http.MethodDelete, "/admin/api/keywords/1"},
		{http.MethodPost, "/admin/api/keywords/1/reset-hits"},
		{http.MethodGet, "/admin/api/prompts?module=zen"},
		{http.MethodPut, "/admin/api/prompts"},
	} {
		rec := call(t, srv, tc.method, tc.path, "{}", false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestKeywordInvalidIDs(t *testing.T) {
	srv, _ := newServer(t)
	cases := map[string]int{
		"-1":                    http.StatusNotFound,
		"0":                     http.StatusNotFound,
		"abc":                   http.StatusBadRequest,
		"999999999999999999999": http.StatusBadRequest,
	}
	for id, want := range cases {
		rec := call(t, srv, http.MethodPut, "/admin/api/keywords/"+id, `{"keyword":"x"}`, true)
		if rec.Code != want {
			t.Errorf("PUT id=%s -> %d, want %d", id, rec.Code, want)
		}
	}
}

func TestKeywordNoteTruncationKeepsValidUTF8(t *testing.T) {
	srv, _ := newServer(t)
	cn := strings.Repeat("注", 400)
	body, _ := json.Marshal(map[string]any{"module": "zen", "keyword": "k", "note": cn})
	rec := call(t, srv, http.MethodPost, "/admin/api/keywords", string(body), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d", rec.Code)
	}
	note := decode(t, rec)["keyword"].(map[string]any)["note"].(string)
	if n := len([]rune(note)); n != 200 {
		t.Errorf("note should be truncated to 200 runes, got %d", n)
	}
	if strings.ContainsRune(note, '\uFFFD') {
		t.Error("note must not contain invalid UTF-8")
	}
}
