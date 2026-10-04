package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"zengateway/internal/store"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// createRule 通过 API 创建规则并返回 id。
func createRule(t *testing.T, srv *Server, body string) int64 {
	t.Helper()
	rec := call(t, srv, http.MethodPost, "/admin/api/rewrites", body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create rule = %d body=%s", rec.Code, rec.Body.String())
	}
	rule := decode(t, rec)["rule"].(map[string]any)
	return int64(rule["ID"].(float64))
}

func TestRewriteRuleCRUDViaAPI(t *testing.T) {
	srv, st := newServer(t)

	// 列表初始为空，且带作用域选项
	rec := call(t, srv, http.MethodGet, "/admin/api/rewrites", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	payload := decode(t, rec)
	if len(payload["rules"].([]any)) != 0 {
		t.Error("expected no rules initially")
	}
	if len(payload["scopes"].([]any)) != 3 {
		t.Errorf("expected 3 scope options, got %v", payload["scopes"])
	}

	// 创建
	id := createRule(t, srv, `{"module":"zen","name":"品牌词","match":"OpenClaw","replace":"OpenCode","scope":"system"}`)

	// 引擎应已热重载（无需等周期重载）
	rules := srv.rw.Rules("zen")
	if len(rules) != 1 || rules[0].Match != "OpenClaw" {
		t.Fatalf("engine not reloaded: %+v", rules)
	}

	// 读取列表
	rec = call(t, srv, http.MethodGet, "/admin/api/rewrites?module=zen", "", true)
	items := decode(t, rec)["rules"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(items))
	}
	first := items[0].(map[string]any)
	if first["Scope"] != "system" || first["Replace"] != "OpenCode" {
		t.Errorf("rule fields wrong: %+v", first)
	}
	if first["compileError"] != "" {
		t.Errorf("valid rule should have empty compileError, got %v", first["compileError"])
	}

	// 更新
	rec = call(t, srv, http.MethodPut, "/admin/api/rewrites/"+itoa(id),
		`{"replace":"OpenCode AI","scope":"messages","includeTools":true}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d body=%s", rec.Code, rec.Body.String())
	}
	updated := decode(t, rec)["rule"].(map[string]any)
	if updated["Replace"] != "OpenCode AI" || updated["Scope"] != "messages" || updated["IncludeTools"] != true {
		t.Errorf("update not applied: %+v", updated)
	}
	// 未提供的字段应保持原值
	if updated["Match"] != "OpenClaw" || updated["Module"] != "zen" {
		t.Errorf("partial update clobbered fields: %+v", updated)
	}

	// 删除
	rec = call(t, srv, http.MethodDelete, "/admin/api/rewrites/"+itoa(id), "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	if _, err := st.GetRewriteRule(id); err != store.ErrNotFound {
		t.Errorf("rule should be deleted, got %v", err)
	}
	if len(srv.rw.Rules("zen")) != 0 {
		t.Error("engine should drop deleted rule after reload")
	}
}

func TestRewriteRuleValidation(t *testing.T) {
	srv, _ := newServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"空匹配", `{"module":"zen","match":"","replace":"x"}`},
		{"正则可匹配空串", `{"module":"zen","match":"a*","replace":"x","isRegex":true}`},
		{"非法正则", `{"module":"zen","match":"(bad","replace":"x","isRegex":true}`},
		{"非法模块", `{"module":"other","match":"a","replace":"b"}`},
		{"非法作用域", `{"module":"zen","match":"a","replace":"b","scope":"bogus"}`},
	}
	for _, c := range cases {
		rec := call(t, srv, http.MethodPost, "/admin/api/rewrites", c.body, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d body=%s", c.name, rec.Code, rec.Body.String())
		}
	}

	// 合法：全局模块 + 默认作用域
	id := createRule(t, srv, `{"module":"*","match":"OpenClaw","replace":"OpenCode"}`)
	rec := call(t, srv, http.MethodGet, "/admin/api/rewrites", "", true)
	rule := decode(t, rec)["rules"].([]any)[0].(map[string]any)
	if rule["Scope"] != store.ScopeSystemFirstUser {
		t.Errorf("default scope = %v, want %s", rule["Scope"], store.ScopeSystemFirstUser)
	}
	_ = id
}

func TestRewriteListFlagsInvalidStoredRule(t *testing.T) {
	srv, st := newServer(t)
	// 绕过 API 校验直接写入危险规则，列表应标记为不可编译
	if _, err := st.CreateRewriteRule(store.RewriteRule{
		Module: "zen", Match: "x*", IsRegex: true, Scope: store.ScopeSystem, Enabled: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := call(t, srv, http.MethodGet, "/admin/api/rewrites", "", true)
	rule := decode(t, rec)["rules"].([]any)[0].(map[string]any)
	if rule["compileError"] == "" {
		t.Error("invalid stored rule must be flagged with compileError")
	}
}

func TestRewriteReorderAndResetHits(t *testing.T) {
	srv, _ := newServer(t)
	a := createRule(t, srv, `{"module":"zen","match":"A","replace":"B"}`)
	b := createRule(t, srv, `{"module":"zen","match":"B","replace":"C"}`)

	rec := call(t, srv, http.MethodPost, "/admin/api/rewrites/reorder",
		`{"ids":[`+itoa(b)+`,`+itoa(a)+`]}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d body=%s", rec.Code, rec.Body.String())
	}
	rules := srv.rw.Rules("zen")
	if len(rules) != 2 || rules[0].ID != b {
		t.Fatalf("reorder not applied: %+v", rules)
	}

	// 命中计数清零
	if err := srv.store.AddRewriteHits(map[int64]int64{a: 7}); err != nil {
		t.Fatalf("add hits: %v", err)
	}
	rec = call(t, srv, http.MethodPost, "/admin/api/rewrites/"+itoa(a)+"/reset-hits", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d", rec.Code)
	}
	got, _ := srv.store.GetRewriteRule(a)
	if got.Hits != 0 {
		t.Errorf("hits = %d, want 0", got.Hits)
	}
}

func TestRewritePreview(t *testing.T) {
	srv, _ := newServer(t)
	createRule(t, srv, `{"module":"zen","match":"OpenClaw","replace":"OpenCode","scope":"system"}`)
	createRule(t, srv, `{"module":"zen","match":"v(\\d+)","replace":"ver$1","isRegex":true}`)

	body, _ := json.Marshal(map[string]any{
		"module": "zen",
		"text":   "You are OpenClaw v2, built on OpenClaw.",
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/rewrites/preview", bytesReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d body=%s", rec.Code, rec.Body.String())
	}
	out := decode(t, rec)
	want := "You are OpenCode ver2, built on OpenCode."
	if out["result"] != want {
		t.Errorf("preview result = %v, want %q", out["result"], want)
	}
	if out["changed"] != true {
		t.Error("changed should be true")
	}
	hits := out["hits"].([]any)
	if len(hits) != 2 {
		t.Fatalf("expected 2 hit entries, got %v", hits)
	}
	if hits[0].(map[string]any)["count"].(float64) != 2 {
		t.Errorf("first rule should hit twice: %v", hits[0])
	}

	// 预览不得写入命中统计
	rules, _ := srv.store.ListRewriteRules("zen")
	for _, r := range rules {
		if r.Hits != 0 {
			t.Errorf("preview must not record hits, rule %d hits=%d", r.ID, r.Hits)
		}
	}
}

func TestRewritePreviewValidation(t *testing.T) {
	srv, _ := newServer(t)

	// 非法模块
	rec := call(t, srv, http.MethodPost, "/admin/api/rewrites/preview",
		`{"module":"bogus","text":"x"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid module should be 400, got %d", rec.Code)
	}

	// 无规则时应原样返回
	rec = call(t, srv, http.MethodPost, "/admin/api/rewrites/preview",
		`{"module":"zen","text":"OpenClaw"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d", rec.Code)
	}
	out := decode(t, rec)
	if out["result"] != "OpenClaw" || out["changed"] != false {
		t.Errorf("no rules should leave text unchanged: %+v", out)
	}
}

func TestRewriteAPIRequiresAuth(t *testing.T) {
	srv, _ := newServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/api/rewrites"},
		{http.MethodPost, "/admin/api/rewrites"},
		{http.MethodPost, "/admin/api/rewrites/preview"},
		{http.MethodPost, "/admin/api/rewrites/reorder"},
		{http.MethodPut, "/admin/api/rewrites/1"},
		{http.MethodDelete, "/admin/api/rewrites/1"},
		{http.MethodPost, "/admin/api/rewrites/1/reset-hits"},
	} {
		rec := call(t, srv, tc.method, tc.path, "{}", false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestRewriteMethodNotAllowed(t *testing.T) {
	srv, _ := newServer(t)
	rec := call(t, srv, http.MethodDelete, "/admin/api/rewrites", "", true)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /rewrites = %d, want 405", rec.Code)
	}
}

// TestRewritePatchNullValuesAreIgnored 验证请求体中的 null 字段被当作「未提供」处理，
// 不会把已有规则改坏（JSON null 解码为 nil 指针，合并逻辑应跳过）。
func TestRewritePatchNullValuesAreIgnored(t *testing.T) {
	srv, _ := newServer(t)
	id := createRule(t, srv,
		`{"module":"zen","name":"n","match":"OpenClaw","replace":"OpenCode","scope":"system"}`)

	for _, body := range []string{
		`{"module":null}`, `{"match":null}`, `{"replace":null}`,
		`{"scope":null}`, `{"isRegex":null}`, `{"includeTools":null}`, `{"enabled":null}`, `{}`,
	} {
		rec := call(t, srv, http.MethodPut, "/admin/api/rewrites/"+itoa(id), body, true)
		if rec.Code != http.StatusOK {
			t.Errorf("body %s -> %d, want 200", body, rec.Code)
		}
	}

	rec := call(t, srv, http.MethodGet, "/admin/api/rewrites", "", true)
	rules := decode(t, rec)["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	r := rules[0].(map[string]any)
	if r["Match"] != "OpenClaw" || r["Replace"] != "OpenCode" || r["Scope"] != "system" {
		t.Errorf("null patch corrupted the rule: %+v", r)
	}
	if r["Enabled"] != true {
		t.Errorf("null patch must not disable the rule: %+v", r)
	}
}

// TestRewritePatchOversizedFields 验证超长输入被拒绝（避免规则表被写入超大内容）。
func TestRewritePatchOversizedFields(t *testing.T) {
	srv, _ := newServer(t)

	long := strings.Repeat("a", 20000)
	body, _ := json.Marshal(map[string]any{"module": "zen", "match": "x", "replace": long})
	rec := call(t, srv, http.MethodPost, "/admin/api/rewrites", string(body), true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized replace = %d, want 400", rec.Code)
	}

	// 超长 name 应被截断而非拒绝
	name, _ := json.Marshal(strings.Repeat("n", 300))
	body, _ = json.Marshal(map[string]any{"module": "zen", "match": "a", "replace": "b", "name": string(name)})
	rec = call(t, srv, http.MethodPost, "/admin/api/rewrites", string(body), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("long name should be accepted (truncated), got %d", rec.Code)
	}
	rule := decode(t, rec)["rule"].(map[string]any)
	if n := len([]rune(rule["Name"].(string))); n > 100 {
		t.Errorf("name should be truncated to 100 runes, got %d", n)
	}

	// match 与 replace 对称：超长匹配内容应被拒绝
	longMatch, _ := json.Marshal(strings.Repeat("m", 5000))
	body, _ = json.Marshal(map[string]any{"module": "zen", "match": string(longMatch), "replace": "b"})
	rec = call(t, srv, http.MethodPost, "/admin/api/rewrites", string(body), true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized match = %d, want 400", rec.Code)
	}
}

// TestRewriteNameTruncationKeepsValidUTF8 验证按字符截断，不会把中文切成非法 UTF-8。
// 回归保护：此前用 Name[:100] 按字节截断，中文约 34 字即切碎多字节字符。
func TestRewriteNameTruncationKeepsValidUTF8(t *testing.T) {
	srv, _ := newServer(t)

	// 200 个中文字符（远超 100），若按字节截断会产生非法 UTF-8
	cn := strings.Repeat("品", 200)
	body, _ := json.Marshal(map[string]any{
		"module": "zen", "match": "a", "replace": "b", "name": cn,
	})
	rec := call(t, srv, http.MethodPost, "/admin/api/rewrites", string(body), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d body=%s", rec.Code, rec.Body.String())
	}
	name := decode(t, rec)["rule"].(map[string]any)["Name"].(string)

	if !utf8.ValidString(name) {
		t.Errorf("name must stay valid UTF-8, got %q", name)
	}
	if n := len([]rune(name)); n != 100 {
		t.Errorf("expected 100 runes, got %d", n)
	}
	if strings.ContainsRune(name, utf8.RuneError) {
		t.Errorf("name contains replacement char (truncated mid-rune): %q", name)
	}
}

// TestRewriteInvalidIDs 验证各类非法 id 不导致 panic 或误操作。
func TestRewriteInvalidIDs(t *testing.T) {
	srv, _ := newServer(t)
	cases := map[string]int{
		"-1":                    http.StatusNotFound,
		"0":                     http.StatusNotFound,
		"abc":                   http.StatusBadRequest,
		"999999999999999999999": http.StatusBadRequest,
	}
	for id, want := range cases {
		rec := call(t, srv, http.MethodPut, "/admin/api/rewrites/"+id, `{"match":"a"}`, true)
		if rec.Code != want {
			t.Errorf("PUT id=%s -> %d, want %d", id, rec.Code, want)
		}
		rec = call(t, srv, http.MethodDelete, "/admin/api/rewrites/"+id, "", true)
		if rec.Code != want {
			t.Errorf("DELETE id=%s -> %d, want %d", id, rec.Code, want)
		}
	}
}

// TestRewriteMatchMayNotBeClearedToEmpty 验证不能把 match 更新为空串（否则规则会匹配一切）。
func TestRewriteMatchMayNotBeClearedToEmpty(t *testing.T) {
	srv, _ := newServer(t)
	id := createRule(t, srv, `{"module":"zen","match":"OpenClaw","replace":"OpenCode"}`)
	rec := call(t, srv, http.MethodPut, "/admin/api/rewrites/"+itoa(id),
		`{"match":""}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("clearing match to empty should be rejected, got %d", rec.Code)
	}
	// 原规则应保持不变
	rec = call(t, srv, http.MethodGet, "/admin/api/rewrites", "", true)
	rule := decode(t, rec)["rules"].([]any)[0].(map[string]any)
	if rule["Match"] != "OpenClaw" {
		t.Errorf("rule should be unchanged, got %v", rule["Match"])
	}
}

// TestRewriteDefaultsAreSafe 锁定新建规则的默认值：
//   - enabled=true（否则规则静默失效）
//   - caseSensitive=true（区分大小写；忽略大小写会扩大匹配面、提高误伤概率）
//
// 这两个默认值此前都被实测发现有问题（enabled 曾默认为 false），因此加入回归保护。
func TestRewriteDefaultsAreSafe(t *testing.T) {
	srv, _ := newServer(t)
	id := createRule(t, srv, `{"module":"zen","match":"OpenClaw","replace":"OpenCode"}`)

	rec := call(t, srv, http.MethodGet, "/admin/api/rewrites", "", true)
	rule := decode(t, rec)["rules"].([]any)[0].(map[string]any)

	if rule["Enabled"] != true {
		t.Errorf("new rule must default to enabled, got %v", rule["Enabled"])
	}
	if rule["CaseSensitive"] != true {
		t.Errorf("new rule must default to case-sensitive, got %v", rule["CaseSensitive"])
	}
	if rule["IncludeTools"] != false {
		t.Errorf("includeTools must default to false, got %v", rule["IncludeTools"])
	}

	// 引擎侧同样要体现：大小写不一致不应被替换
	body, _ := json.Marshal(map[string]any{
		"module": "zen",
		"text":   "OpenClaw openclaw OPENCLAW",
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/rewrites/preview", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	out := decode(t, rec)
	if out["result"] != "OpenCode openclaw OPENCLAW" {
		t.Errorf("default must be case-sensitive, preview = %v", out["result"])
	}

	// 显式关闭区分大小写后，才应全部替换
	rec = call(t, srv, http.MethodPut, "/admin/api/rewrites/"+itoa(id),
		`{"caseSensitive":false}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable case sensitivity = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/admin/api/rewrites/preview", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	out = decode(t, rec)
	if out["result"] != "OpenCode OpenCode OpenCode" {
		t.Errorf("with case sensitivity off, preview = %v", out["result"])
	}
}

// TestRewriteToggleRegexRevalidates 验证把已有字面量规则切换为正则时会重新校验：
// 字面量 "a*" 是安全的（按普通字符匹配），但作为正则会匹配空字符串，
// 必须在保存时被拒绝，否则会在提示词每个字符间插入替换文本。
func TestRewriteToggleRegexRevalidates(t *testing.T) {
	srv, _ := newServer(t)

	// 字面量模式：a* 作为普通文本，允许
	id := createRule(t, srv, `{"module":"zen","match":"a*","replace":"X","isRegex":false}`)

	// 切换为正则：应被拒绝
	rec := call(t, srv, http.MethodPut, "/admin/api/rewrites/"+itoa(id),
		`{"isRegex":true}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("toggling to regex should be rejected for empty-matching pattern, got %d", rec.Code)
	}

	// 规则应保持字面量模式且未被破坏
	rec = call(t, srv, http.MethodGet, "/admin/api/rewrites", "", true)
	rule := decode(t, rec)["rules"].([]any)[0].(map[string]any)
	if rule["IsRegex"] != false || rule["Match"] != "a*" {
		t.Errorf("rule should be unchanged after rejected update: %+v", rule)
	}

	// 引擎侧：字面量 "a*" 只能匹配字面文本 "a*"，不会逐字符插入
	body, _ := json.Marshal(map[string]any{
		"module": "zen",
		"text":   "aaa a* aaa",
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/rewrites/preview", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	out := decode(t, rec)
	if out["result"] != "aaa X aaa" {
		t.Errorf("literal a* should match only the literal occurrence, got %v", out["result"])
	}
}

// TestRewriteRegexToLiteralAlsoRevalidates 验证反向切换（正则→字面量）同样重新校验。
func TestRewriteRegexToLiteralAlsoRevalidates(t *testing.T) {
	srv, _ := newServer(t)
	id := createRule(t, srv, `{"module":"zen","match":"v(\\d+)","replace":"ver$1","isRegex":true}`)

	// 切换为字面量：v(\d+) 作为普通文本合法，且不再匹配空串
	rec := call(t, srv, http.MethodPut, "/admin/api/rewrites/"+itoa(id),
		`{"isRegex":false}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("switching to literal should be allowed, got %d body=%s", rec.Code, rec.Body.String())
	}

	// 切换后只应按字面量匹配
	body, _ := json.Marshal(map[string]any{"module": "zen", "text": "v2 and v(\\d+)"})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/rewrites/preview", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	out := decode(t, rec)
	if out["result"] != `v2 and ver$1` {
		t.Errorf("literal mode should only replace the literal text, got %v", out["result"])
	}
}
