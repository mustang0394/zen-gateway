package web

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zengateway/internal/cooldown"
	"zengateway/internal/rewrite"
	"zengateway/internal/store"
	"zengateway/internal/version"
)

const testToken = "admin-secret-token"

func newServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "web.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.DiscardHandler)
	vers := version.New(st, log, version.DefaultTargets())
	cool := cooldown.New(st, log)
	rw := rewrite.New(st, log)
	srv := New(st, vers, cool, rw, log, testToken, "https://zen.example/v1", "https://cline.example/api")
	if srv == nil {
		t.Fatal("server must not be nil when token configured")
	}
	return srv, st
}

func call(t *testing.T, srv *Server, method, path, body string, authorized bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorized {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return out
}

func TestDisabledWithoutToken(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if got := New(st, nil, nil, nil, nil, "", "", ""); got != nil {
		t.Error("web server must be nil when no admin token set")
	}
	if got := New(st, nil, nil, nil, nil, "   ", "", ""); got != nil {
		t.Error("blank token must not enable admin")
	}
}

func TestAuthRequired(t *testing.T) {
	srv, _ := newServer(t)

	for _, path := range []string{
		"/admin/api/settings", "/admin/api/versions", "/admin/api/cooldowns",
		"/admin/api/stats", "/admin/api/zen/keys", "/admin/api/cline/keys",
	} {
		rec := call(t, srv, http.MethodGet, path, "", false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", path, rec.Code)
		}
	}

	// 错误 token
	req := httptest.NewRequest(http.MethodGet, "/admin/api/settings", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", rec.Code)
	}
}

func TestLoginIssuesSessionCookie(t *testing.T) {
	srv, _ := newServer(t)

	// 错误口令
	rec := call(t, srv, http.MethodPost, "/admin/api/login", `{"token":"nope"}`, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d, want 401", rec.Code)
	}

	// 正确口令 → 下发 cookie
	rec = call(t, srv, http.MethodPost, "/admin/api/login", `{"token":"`+testToken+`"}`, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d body=%s", rec.Code, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("login must set session cookie")
	}
	if !cookie.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}

	// 用 cookie 访问 API
	req := httptest.NewRequest(http.MethodGet, "/admin/api/versions", nil)
	req.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Errorf("cookie auth = %d, want 200", rec2.Code)
	}
}

func TestKeyCRUDWithNote(t *testing.T) {
	srv, st := newServer(t)

	// 新增（带备注）
	rec := call(t, srv, http.MethodPost, "/admin/api/cline/keys",
		`{"label":"主号","note":"2026-10-01 注册，日本出口","apiKey":"sk-abc","proxy":"socks5h://u:p@h:1080"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d body=%s", rec.Code, rec.Body.String())
	}
	created := decode(t, rec)["key"].(map[string]any)
	id := int64(created["ID"].(float64))
	if created["Note"] != "2026-10-01 注册，日本出口" {
		t.Errorf("note not returned: %v", created["Note"])
	}

	// 列表：不脱敏，且含匿名提示
	rec = call(t, srv, http.MethodGet, "/admin/api/cline/keys", "", true)
	list := decode(t, rec)
	keys := list["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	first := keys[0].(map[string]any)
	if first["APIKey"] != "sk-abc" {
		t.Errorf("key must not be masked, got %v", first["APIKey"])
	}
	if list["anonymousSupported"] != false {
		t.Error("cline must not advertise anonymous support")
	}

	// 更新备注为空
	rec = call(t, srv, http.MethodPut, "/admin/api/cline/keys/"+itoa(id),
		`{"note":"","label":"备用"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d body=%s", rec.Code, rec.Body.String())
	}
	updated := decode(t, rec)["key"].(map[string]any)
	if updated["Note"] != "" || updated["Label"] != "备用" {
		t.Errorf("update not applied: %+v", updated)
	}

	// 软删除
	rec = call(t, srv, http.MethodDelete, "/admin/api/cline/keys/"+itoa(id), "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = call(t, srv, http.MethodGet, "/admin/api/cline/keys", "", true)
	if got := len(decode(t, rec)["keys"].([]any)); got != 0 {
		t.Errorf("deleted key still listed: %d", got)
	}
	if _, err := st.GetKey(id); err != store.ErrNotFound {
		t.Errorf("key should be soft deleted, got %v", err)
	}
}

func TestZenAnonymousKeyCreationAndHint(t *testing.T) {
	srv, _ := newServer(t)

	// 勾选匿名但未填 key → 自动填 public
	rec := call(t, srv, http.MethodPost, "/admin/api/zen/keys",
		`{"label":"匿名","isAnonymous":true}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create anonymous = %d body=%s", rec.Code, rec.Body.String())
	}
	k := decode(t, rec)["key"].(map[string]any)
	if k["APIKey"] != "public" || k["IsAnonymous"] != true {
		t.Errorf("anonymous key should be public: %+v", k)
	}

	rec = call(t, srv, http.MethodGet, "/admin/api/zen/keys", "", true)
	list := decode(t, rec)
	if list["anonymousSupported"] != true {
		t.Error("zen must advertise anonymous support")
	}
	if !strings.Contains(list["anonymousHint"].(string), "public") {
		t.Errorf("anonymous hint should mention public, got %v", list["anonymousHint"])
	}

	// cline 不填 key 且不匿名 → 400
	rec = call(t, srv, http.MethodPost, "/admin/api/cline/keys", `{"label":"x"}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("cline without key should be 400, got %d", rec.Code)
	}
}

func TestReorderKeys(t *testing.T) {
	srv, st := newServer(t)
	a, _ := st.CreateKey(store.APIKey{Module: "zen", Label: "A", APIKey: "public", Enabled: true, IsAnonymous: true})
	b, _ := st.CreateKey(store.APIKey{Module: "zen", Label: "B", APIKey: "public", Enabled: true, IsAnonymous: true})

	rec := call(t, srv, http.MethodPost, "/admin/api/zen/keys/reorder",
		`{"ids":[`+itoa(b.ID)+`,`+itoa(a.ID)+`]}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d body=%s", rec.Code, rec.Body.String())
	}
	keys, _ := st.ListKeys("zen")
	if keys[0].ID != b.ID {
		t.Errorf("reorder not applied: first = %d, want %d", keys[0].ID, b.ID)
	}
}

func TestCooldownLifecycleAPI(t *testing.T) {
	srv, st := newServer(t)
	k, _ := st.CreateKey(store.APIKey{Module: "cline", Label: "主号", APIKey: "sk", Enabled: true})

	// 手动添加
	rec := call(t, srv, http.MethodPost, "/admin/api/cooldowns",
		`{"module":"cline","keyId":`+itoa(k.ID)+`,"model":"z-ai/glm-5.3-flash","minutes":120}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("add cooldown = %d body=%s", rec.Code, rec.Body.String())
	}

	// 列表
	rec = call(t, srv, http.MethodGet, "/admin/api/cooldowns", "", true)
	items := decode(t, rec)["cooldowns"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 cooldown, got %d", len(items))
	}
	first := items[0].(map[string]any)
	if first["keyLabel"] != "主号" {
		t.Errorf("keyLabel = %v", first["keyLabel"])
	}
	if first["remaining"] != "2h0m" {
		t.Errorf("remaining = %v, want 2h0m", first["remaining"])
	}
	id := int64(first["ID"].(float64))

	// 模块过滤
	rec = call(t, srv, http.MethodGet, "/admin/api/cooldowns?module=zen", "", true)
	if got := len(decode(t, rec)["cooldowns"].([]any)); got != 0 {
		t.Errorf("zen filter returned %d", got)
	}

	// 手动解除
	rec = call(t, srv, http.MethodDelete, "/admin/api/cooldowns/"+itoa(id), "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("release = %d", rec.Code)
	}
	rec = call(t, srv, http.MethodGet, "/admin/api/cooldowns", "", true)
	if got := len(decode(t, rec)["cooldowns"].([]any)); got != 0 {
		t.Errorf("cooldown not released: %d", got)
	}
}

func TestStatsAPI(t *testing.T) {
	srv, st := newServer(t)
	k, _ := st.CreateKey(store.APIKey{Module: "cline", Label: "主号", APIKey: "sk", Enabled: true})
	day := time.Now().Format(store.DayLayout)

	st.AddStat(store.StatDelta{
		Day: day, Module: "cline", KeyID: k.ID, Model: "m1",
		Requests: 4, PromptTokens: 400, CompletionTokens: 100,
		CachedTokens: 200, TotalTokens: 500, TTFTSumMs: 1200, TTFTCount: 4,
	})
	st.AddStat(store.StatDelta{Day: day, Module: "zen", KeyID: 1, Model: "m2", Requests: 1})
	if err := st.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	rec := call(t, srv, http.MethodGet, "/admin/api/stats?module=cline", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats = %d", rec.Code)
	}
	payload := decode(t, rec)
	if payload["day"] != day {
		t.Errorf("day = %v, want %v", payload["day"], day)
	}
	summary := payload["summary"].(map[string]any)
	if summary["requests"].(float64) != 4 {
		t.Errorf("requests = %v", summary["requests"])
	}
	if summary["cacheHitRate"].(float64) != 50 {
		t.Errorf("cacheHitRate = %v, want 50", summary["cacheHitRate"])
	}
	if summary["avgTtftMs"].(float64) != 300 {
		t.Errorf("avgTtftMs = %v, want 300", summary["avgTtftMs"])
	}
	bd := payload["breakdown"].([]any)
	if len(bd) != 1 {
		t.Fatalf("module filter should return 1 row, got %d", len(bd))
	}
	if bd[0].(map[string]any)["keyLabel"] != "主号" {
		t.Errorf("keyLabel = %v", bd[0].(map[string]any)["keyLabel"])
	}

	// 无模块过滤返回全部
	rec = call(t, srv, http.MethodGet, "/admin/api/stats", "", true)
	if got := len(decode(t, rec)["breakdown"].([]any)); got != 2 {
		t.Errorf("unfiltered breakdown = %d rows, want 2", got)
	}

	// 非法模块
	rec = call(t, srv, http.MethodGet, "/admin/api/stats?module=bogus", "", true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid module = %d, want 400", rec.Code)
	}
}

func TestSettingsAPI(t *testing.T) {
	srv, _ := newServer(t)

	rec := call(t, srv, http.MethodGet, "/admin/api/settings", "", true)
	got := decode(t, rec)
	if got["accessTokenZen"] != "" {
		t.Errorf("zen token default should be empty (anonymous), got %v", got["accessTokenZen"])
	}
	if got["retentionDays"].(float64) != 30 {
		t.Errorf("retentionDays default = %v", got["retentionDays"])
	}

	rec = call(t, srv, http.MethodPut, "/admin/api/settings",
		`{"accessTokenCline":"cline-tok","accessTokenZen":"zen-tok","retentionDays":15}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("put settings = %d", rec.Code)
	}

	rec = call(t, srv, http.MethodGet, "/admin/api/settings", "", true)
	got = decode(t, rec)
	if got["accessTokenCline"] != "cline-tok" || got["accessTokenZen"] != "zen-tok" {
		t.Errorf("settings not saved: %+v", got)
	}
	if got["retentionDays"].(float64) != 15 {
		t.Errorf("retentionDays = %v", got["retentionDays"])
	}
}

func TestVersionsAPI(t *testing.T) {
	srv, st := newServer(t)
	st.SaveVersion(store.Version{
		Name: version.NameClineCLI, Value: "4.1.22", FetchedAt: time.Now(), OK: true,
	})

	rec := call(t, srv, http.MethodGet, "/admin/api/versions", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("versions = %d", rec.Code)
	}
	payload := decode(t, rec)
	current := payload["current"].(map[string]any)
	if current["cline.cli"] != "4.1.22" {
		t.Errorf("current cline.cli = %v", current["cline.cli"])
	}
	if len(payload["records"].([]any)) != 1 {
		t.Errorf("records = %v", payload["records"])
	}
	if len(payload["names"].([]any)) != 3 {
		t.Errorf("names = %v", payload["names"])
	}
}

func TestKeyProbeValidatesProxy(t *testing.T) {
	srv, st := newServer(t)
	k, _ := st.CreateKey(store.APIKey{Module: "zen", Label: "A", APIKey: "public", Proxy: "ftp://bad", Enabled: true})

	rec := call(t, srv, http.MethodPost, "/admin/api/zen/keys/"+itoa(k.ID)+"/probe?id="+itoa(k.ID), "", true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid proxy should be 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	good, _ := st.CreateKey(store.APIKey{Module: "zen", Label: "B", APIKey: "public", Proxy: "socks5h://u:p@h:1080", Enabled: true})
	rec = call(t, srv, http.MethodPost, "/admin/api/zen/keys/"+itoa(good.ID)+"/probe?id="+itoa(good.ID), "", true)
	if rec.Code != http.StatusOK {
		t.Errorf("valid proxy should be accepted, got %d", rec.Code)
	}
}

func TestUnknownEndpointAndBadModule(t *testing.T) {
	srv, _ := newServer(t)
	rec := call(t, srv, http.MethodGet, "/admin/api/nope", "", true)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown endpoint = %d, want 404", rec.Code)
	}
	rec = call(t, srv, http.MethodGet, "/admin/api/bogus/keys", "", true)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad module = %d, want 400", rec.Code)
	}
}

func TestStaticPageServed(t *testing.T) {
	srv, _ := newServer(t)
	for _, path := range []string{"/admin", "/admin/", "/admin/keys", "/admin/anything"} {
		rec := call(t, srv, http.MethodGet, path, "", true)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", path, rec.Code)
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte("<html")) {
			t.Errorf("%s should serve the SPA shell", path)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv, _ := newServer(t)
	rec := call(t, srv, http.MethodDelete, "/admin/api/versions", "", true)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /versions = %d, want 405", rec.Code)
	}
}

func itoa(v int64) string {
	return strings.TrimSpace(strings.Join([]string{jsonNumber(v)}, ""))
}

func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
