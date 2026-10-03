package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"zengateway/internal/provider"
)

type stubHandler struct {
	called bool
	kind   provider.Kind
	token  string
}

func (s *stubHandler) Serve(w http.ResponseWriter, r *http.Request, kind provider.Kind, accessToken string) {
	s.called = true
	s.kind = kind
	s.token = accessToken
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func newTestRouter() (map[string]*stubHandler, *Router) {
	zenH := &stubHandler{}
	clineH := &stubHandler{}
	rt := New(map[string]ModuleHandler{"zen": zenH, "cline": clineH},
		func(module string) string {
			if module == "cline" {
				return "cline-token"
			}
			return ""
		}, nil)
	return map[string]*stubHandler{"zen": zenH, "cline": clineH}, rt
}

func TestRoutesToCorrectModuleAndKind(t *testing.T) {
	handlers, rt := newTestRouter()

	cases := []struct {
		path string
		mod  string
		kind provider.Kind
	}{
		{"/zen/v1/chat/completions", "zen", provider.KindChat},
		{"/zen/v1/responses", "zen", provider.KindResponses},
		{"/zen/v1/models", "zen", provider.KindModels},
		{"/cline/v1/chat/completions", "cline", provider.KindChat},
		{"/cline/v1/responses", "cline", provider.KindResponses},
		{"/cline/v1/models", "cline", provider.KindModels},
		// 省略 /v1 前缀也接受
		{"/zen/chat/completions", "zen", provider.KindChat},
		{"/cline/responses", "cline", provider.KindResponses},
	}
	for _, c := range cases {
		for _, h := range handlers {
			h.called = false
		}
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, c.path, nil))

		if rec.Code != 200 {
			t.Errorf("%s: status = %d", c.path, rec.Code)
			continue
		}
		if !handlers[c.mod].called {
			t.Errorf("%s: handler %s not called", c.path, c.mod)
			continue
		}
		if handlers[c.mod].kind != c.kind {
			t.Errorf("%s: kind = %s, want %s", c.path, handlers[c.mod].kind, c.kind)
		}
	}
}

func TestTokenLookupPerModule(t *testing.T) {
	handlers, rt := newTestRouter()

	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/cline/v1/chat/completions", nil))
	if handlers["cline"].token != "cline-token" {
		t.Errorf("cline token = %q", handlers["cline"].token)
	}

	rt.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/zen/v1/chat/completions", nil))
	if handlers["zen"].token != "" {
		t.Errorf("zen token should be empty (anonymous allowed), got %q", handlers["zen"].token)
	}
}

func TestUnknownPathsReturn404JSON(t *testing.T) {
	_, rt := newTestRouter()
	for _, path := range []string{
		"/v1/chat/completions", // 无模块前缀
		"/openai/v1/chat/completions",
		"/zen/v1/unknown",
		"/zen",
	} {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: content-type = %q", path, ct)
		}
	}
}

func TestHealthz(t *testing.T) {
	_, rt := newTestRouter()
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAdminDisabledWithoutHandler(t *testing.T) {
	_, rt := newTestRouter()
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("admin without handler should be 501, got %d", rec.Code)
	}
}

func TestAdminDelegatesWhenConfigured(t *testing.T) {
	called := false
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	rt := New(map[string]ModuleHandler{}, func(string) string { return "" }, admin)
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/versions", nil))
	if !called || rec.Code != 200 {
		t.Errorf("admin handler not delegated: called=%v status=%d", called, rec.Code)
	}
}

func TestSplitModuleAndKind(t *testing.T) {
	if m, rest, ok := splitModule("/zen/v1/chat/completions"); !ok || m != "zen" || rest != "/chat/completions" {
		t.Errorf("splitModule = %q %q %v", m, rest, ok)
	}
	if _, _, ok := splitModule("/other/v1/models"); ok {
		t.Error("unknown module must not match")
	}
	if k, ok := kindFor("/chat/completions"); !ok || k != provider.KindChat {
		t.Error("kindFor chat failed")
	}
	if _, ok := kindFor("/embeddings"); ok {
		t.Error("unknown kind must not match")
	}
}
