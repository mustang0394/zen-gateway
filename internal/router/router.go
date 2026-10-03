// Package router 把下游路径分发到对应模块，并注入该模块的接入 token。
//
// 路径约定：
//
//	/zen/v1/chat/completions    → zen 模块 chat
//	/zen/v1/responses           → zen 模块 responses
//	/zen/v1/models              → zen 模块 models
//	/cline/v1/...               → cline 模块（同上三类）
//	/admin/...                  → Web 管理端
//	/healthz                    → 健康检查
package router

import (
	"net/http"
	"strings"

	"zengateway/internal/provider"
	"zengateway/internal/store"
)

// ModuleHandler 是单个模块的处理入口。
type ModuleHandler interface {
	Serve(w http.ResponseWriter, r *http.Request, kind provider.Kind, accessToken string)
}

// Tokens 提供各模块的下游接入 token（空表示允许匿名）。
type Tokens func(module string) string

// Router 组合各模块处理器与静态资源。
type Router struct {
	handlers   map[string]ModuleHandler
	tokens     Tokens
	admin      http.Handler
	healthText string
}

// New 创建路由器。admin 可为 nil（未启用管理端）。
func New(handlers map[string]ModuleHandler, tokens Tokens, admin http.Handler) *Router {
	return &Router{
		handlers:   handlers,
		tokens:     tokens,
		admin:      admin,
		healthText: "ok\n",
	}
}

// ServeHTTP 实现 http.Handler。
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case path == "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(rt.healthText))
		return
	case strings.HasPrefix(path, "/admin"):
		if rt.admin == nil {
			writeError(w, http.StatusNotImplemented,
				"admin UI disabled: set ZEN_ADMIN_TOKEN to enable it")
			return
		}
		rt.admin.ServeHTTP(w, r)
		return
	}

	module, rest, ok := splitModule(path)
	if !ok {
		writeError(w, http.StatusNotFound,
			"unknown path "+path+": use /zen/v1/... or /cline/v1/...")
		return
	}
	h, ok := rt.handlers[module]
	if !ok {
		writeError(w, http.StatusNotFound, "module not enabled: "+module)
		return
	}
	kind, ok := kindFor(rest)
	if !ok {
		writeError(w, http.StatusNotFound,
			"unsupported endpoint "+path+": use /v1/chat/completions, /v1/responses or /v1/models")
		return
	}
	h.Serve(w, r, kind, rt.tokens(module))
}

// splitModule 拆分出模块名与剩余路径；同时兼容省略 /v1 前缀的写法。
func splitModule(path string) (module, rest string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		return "", "", false
	}
	module = parts[0]
	if module != "zen" && module != "cline" {
		return "", "", false
	}
	rest = "/"
	if len(parts) == 2 {
		rest = "/" + parts[1]
	}
	return module, strings.TrimPrefix(rest, "/v1"), true
}

// kindFor 把剩余路径映射为请求类型。
func kindFor(rest string) (provider.Kind, bool) {
	switch rest {
	case "/chat/completions":
		return provider.KindChat, true
	case "/responses":
		return provider.KindResponses, true
	case "/models":
		return provider.KindModels, true
	}
	return "", false
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(`{"error":{"message":` + quote(msg) + `,"type":"invalid_request_error"}}`))
}

func quote(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\n':
			b = append(b, '\\', 'n')
		default:
			b = append(b, c)
		}
	}
	b = append(b, '"')
	return string(b)
}

// ModuleTokens 从 store 读取模块接入 token 的便捷实现。
func ModuleTokens(st *store.Store, log interface{ Warn(string, ...any) }) Tokens {
	return func(module string) string {
		key := store.SettingAccessTokenZen
		if module == "cline" {
			key = store.SettingAccessTokenCline
		}
		v, err := st.GetSetting(key, "")
		if err != nil {
			log.Warn("read access token", "module", module, "err", err)
			return ""
		}
		return v
	}
}
