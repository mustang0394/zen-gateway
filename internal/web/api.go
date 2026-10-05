// Package web 提供管理端：REST API + 内嵌 SPA 静态资源。
//
// 鉴权：单一管理口令（环境变量 ZEN_ADMIN_TOKEN）。除 /admin/api/login 外，
// 所有 /admin/api/* 与页面均需携带口令（Authorization: Bearer <token> 或 ?token=）。
// 通过 /admin/api/login 校验后可获得 HttpOnly 会话 cookie，浏览器端无需保存口令。
package web

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"zengateway/internal/cooldown"
	"zengateway/internal/inject"
	"zengateway/internal/store"
	"zengateway/internal/upstream"
	"zengateway/internal/version"
)

//go:embed dist
var distFS embed.FS

const cookieName = "zen_admin_session"

// maxSessions 是并发有效会话上限（单用户管理端，仅作内存保护）。
const maxSessions = 64

// Server 是管理端 HTTP 处理器。
type Server struct {
	store  *store.Store
	vers   *version.Manager
	cool   *cooldown.Service
	inj    *inject.Engine
	log    *slog.Logger
	token  string
	zenUp  string
	clinUp string

	mu       sync.RWMutex
	sessions map[string]time.Time
}

// New 创建管理端。token 为空时返回 nil（调用方据此禁用管理端）。
func New(st *store.Store, vers *version.Manager, cool *cooldown.Service, inj *inject.Engine,
	log *slog.Logger, token, zenUpstream, clineUpstream string) *Server {

	if strings.TrimSpace(token) == "" {
		return nil
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{
		store: st, vers: vers, cool: cool, inj: inj, log: log,
		token: token, zenUp: zenUpstream, clinUp: clineUpstream,
		sessions: map[string]time.Time{},
	}
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// 登录接口不需要会话
	if path == "/admin/api/login" {
		s.handleLogin(w, r)
		return
	}
	if !s.authorized(r) {
		if strings.HasPrefix(path, "/admin/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "unauthorized: provide ZEN_ADMIN_TOKEN",
			})
			return
		}
		// 页面请求返回登录页（前端会根据 401 跳转）
		s.serveStatic(w, r)
		return
	}

	if strings.HasPrefix(path, "/admin/api/") {
		s.serveAPI(w, r)
		return
	}
	s.serveStatic(w, r)
}

// ---- 鉴权 ----------------------------------------------------------------

func (s *Server) authorized(r *http.Request) bool {
	if tok := bearerToken(r.Header.Get("Authorization")); tok != "" && subtleEqual(tok, s.token) {
		return true
	}
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.RLock()
		exp, ok := s.sessions[c.Value]
		s.mu.RUnlock()
		if ok && time.Now().Before(exp) {
			return true
		}
	}
	return false
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}
	if !subtleEqual(strings.TrimSpace(body.Token), s.token) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid admin token"})
		return
	}

	sid := randomID()
	exp := time.Now().Add(7 * 24 * time.Hour)
	s.mu.Lock()
	// 顺手清理过期会话，避免 map 随登录次数无限增长
	now := time.Now()
	for k, e := range s.sessions {
		if now.After(e) {
			delete(s.sessions, k)
		}
	}
	// 极端情况下（口令泄露被反复登录）仍设置上限，防止内存无界增长
	if len(s.sessions) >= maxSessions {
		for k := range s.sessions {
			delete(s.sessions, k)
			if len(s.sessions) < maxSessions {
				break
			}
		}
	}
	s.sessions[sid] = exp
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: sid, Path: "/admin",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Expires: exp, MaxAge: int((7 * 24 * time.Hour).Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- API 路由 -------------------------------------------------------------

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	parts := strings.Split(strings.Trim(path, "/"), "/")

	// 路径形态：
	//   settings                                        [settings]
	//   versions | versions/refresh                     [versions, ...]
	//   cooldowns | cooldowns/{id}                      [cooldowns, ...]
	//   stats                                           [stats]
	//   {module}/keys                                   [module, keys]
	//   {module}/keys/reorder                           [module, keys, reorder]
	//   {module}/keys/{id}                              [module, keys, id]
	//   {module}/keys/{id}/probe                        [module, keys, id, probe]
	switch {
	case len(parts) == 1 && parts[0] == "settings":
		s.handleSettings(w, r)

	case len(parts) == 1 && parts[0] == "versions":
		s.handleVersions(w, r)
	case len(parts) == 2 && parts[0] == "versions" && parts[1] == "refresh":
		s.handleVersionRefresh(w, r)

	case len(parts) == 1 && parts[0] == "cooldowns":
		s.handleCooldowns(w, r)
	case len(parts) == 2 && parts[0] == "cooldowns":
		s.handleCooldownItem(w, r, parts[1])

	case len(parts) == 1 && parts[0] == "stats":
		s.handleStats(w, r)

	case len(parts) == 1 && parts[0] == "keywords":
		s.handleKeywords(w, r)
	case len(parts) == 2 && parts[0] == "keywords" && parts[1] == "test":
		s.handleKeywordTest(w, r)
	case len(parts) == 2 && parts[0] == "keywords" && parts[1] == "reorder":
		s.handleKeywordReorder(w, r)
	case len(parts) == 2 && parts[0] == "keywords":
		s.handleKeywordItem(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "keywords" && parts[2] == "reset-hits":
		s.handleKeywordResetHits(w, r, parts[1])

	case len(parts) == 1 && parts[0] == "prompts":
		s.handlePromptOverrides(w, r)

	case len(parts) >= 2 && parts[1] == "keys":
		module := parts[0]
		switch {
		case len(parts) == 2:
			s.handleKeys(w, r, module)
		case len(parts) == 3 && parts[2] == "reorder":
			s.handleKeyReorder(w, r, module)
		case len(parts) == 3:
			s.handleKeyItem(w, r, module, parts[2])
		case len(parts) == 4 && parts[3] == "probe":
			s.handleKeyProbe(w, r, module, parts[2])
		default:
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown endpoint " + r.URL.Path})
		}

	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown endpoint " + r.URL.Path})
	}
}

var errBadModule = errors.New("module must be zen or cline")

func validModule(m string) error {
	if m != "zen" && m != "cline" {
		return errBadModule
	}
	return nil
}

// ---- keys ----------------------------------------------------------------

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request, module string) {
	if err := validModule(module); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	switch r.Method {
	case http.MethodGet:
		keys, err := s.store.ListKeys(module)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		cooldowns, _ := s.store.ListCooldowns(module, time.Now())
		coolByKey := map[int64][]map[string]any{}
		for _, c := range cooldowns {
			coolByKey[c.KeyID] = append(coolByKey[c.KeyID], map[string]any{
				"model":     c.Model,
				"until":     c.Until,
				"remaining": cooldown.HumanDuration(time.Until(c.Until)),
				"reason":    c.Reason,
			})
		}
		type row struct {
			store.APIKey
			Cooldowns []map[string]any `json:"cooldowns"`
		}
		out := make([]row, 0, len(keys))
		for _, k := range keys {
			out = append(out, row{APIKey: k, Cooldowns: coolByKey[k.ID]})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"module": module, "keys": out,
			"anonymousSupported": module == "zen",
			"anonymousHint":      "免费层无需上游 Key；填写 public 即使用匿名模式（上游按 IP 限流）。",
		})

	case http.MethodPost:
		var in struct {
			Label       string `json:"label"`
			Note        string `json:"note"`
			APIKey      string `json:"apiKey"`
			Proxy       string `json:"proxy"`
			Enabled     *bool  `json:"enabled"`
			IsAnonymous bool   `json:"isAnonymous"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if strings.TrimSpace(in.APIKey) == "" {
			if module == "zen" && in.IsAnonymous {
				in.APIKey = "public"
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "apiKey is required"})
				return
			}
		}
		if len(in.Note) > 500 {
			in.Note = in.Note[:500]
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		k, err := s.store.CreateKey(store.APIKey{
			Module: module, Label: in.Label, Note: in.Note, APIKey: in.APIKey,
			Proxy: strings.TrimSpace(in.Proxy), Enabled: enabled,
			IsAnonymous: in.IsAnonymous || in.APIKey == "public",
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"key": k})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *Server) handleKeyItem(w http.ResponseWriter, r *http.Request, module, idRaw string) {
	if err := validModule(module); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid key id"})
		return
	}

	switch r.Method {
	case http.MethodPut:
		cur, err := s.store.GetKey(id)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		var in struct {
			Label       *string `json:"label"`
			Note        *string `json:"note"`
			APIKey      *string `json:"apiKey"`
			Proxy       *string `json:"proxy"`
			Enabled     *bool   `json:"enabled"`
			IsAnonymous *bool   `json:"isAnonymous"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if in.Label != nil {
			cur.Label = *in.Label
		}
		if in.Note != nil {
			n := *in.Note
			if len(n) > 500 {
				n = n[:500]
			}
			cur.Note = n
		}
		if in.APIKey != nil && strings.TrimSpace(*in.APIKey) != "" {
			cur.APIKey = strings.TrimSpace(*in.APIKey)
		}
		if in.Proxy != nil {
			cur.Proxy = strings.TrimSpace(*in.Proxy)
		}
		if in.Enabled != nil {
			cur.Enabled = *in.Enabled
		}
		if in.IsAnonymous != nil {
			cur.IsAnonymous = *in.IsAnonymous
		}
		updated, err := s.store.UpdateKey(cur)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"key": updated})

	case http.MethodDelete:
		// 先清理该 key 的冷却记录，再软删除
		_ = s.store.DeleteCooldownsByKey(id)
		if err := s.store.DeleteKey(id); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *Server) handleKeyReorder(w http.ResponseWriter, r *http.Request, module string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if err := validModule(module); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.store.ReorderKeys(module, in.IDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleKeyProbe 用该 key 发起一次最小校验，确认 key + 代理配置可用。
func (s *Server) handleKeyProbe(w http.ResponseWriter, r *http.Request, module, idRaw string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if err := validModule(module); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// idRaw 由路由从路径解析（/admin/api/{module}/keys/{id}/probe）。
	// 保留对 ?id= 的兼容：仅当路径未给出 id 时才采用 query 值。
	idRaw = strings.TrimSpace(idRaw)
	if idRaw == "" {
		idRaw = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if idRaw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key id is required"})
		return
	}
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid key id"})
		return
	}
	key, err := s.store.GetKey(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	if key.Module != module {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key does not belong to module " + module})
		return
	}
	if _, err := upstream.ClientFor(key.Proxy); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "invalid proxy: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "proxy configuration accepted (connectivity not tested)",
	})
}

// ---- settings -------------------------------------------------------------

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		all, err := s.store.AllSettings()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		retention, _ := s.store.GetIntSetting(store.SettingRetentionDays, 30)
		writeJSON(w, http.StatusOK, map[string]any{
			"accessTokenZen":   all[store.SettingAccessTokenZen],
			"accessTokenCline": all[store.SettingAccessTokenCline],
			"retentionDays":    retention,
			"zenUpstream":      s.zenUp,
			"clineUpstream":    s.clinUp,
		})

	case http.MethodPut:
		var in struct {
			AccessTokenZen   *string `json:"accessTokenZen"`
			AccessTokenCline *string `json:"accessTokenCline"`
			RetentionDays    *int    `json:"retentionDays"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if in.AccessTokenZen != nil {
			if err := s.store.SetSetting(store.SettingAccessTokenZen, strings.TrimSpace(*in.AccessTokenZen)); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		if in.AccessTokenCline != nil {
			if err := s.store.SetSetting(store.SettingAccessTokenCline, strings.TrimSpace(*in.AccessTokenCline)); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		if in.RetentionDays != nil && *in.RetentionDays > 0 {
			if err := s.store.SetSetting(store.SettingRetentionDays,
				strconv.Itoa(*in.RetentionDays)); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// ---- versions -------------------------------------------------------------

func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	list, err := s.store.ListVersions()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	byName := map[string]store.Version{}
	for _, v := range list {
		byName[v.Name] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current": s.vers.Snapshot(),
		"records": list,
		"names":   version.Names(),
		"zenUses": "User-Agent: opencode/<version>",
		"clineUses": []string{
			"X-CLIENT-VERSION / X-PLATFORM-VERSION / User-Agent: Cline/<version>",
			"X-CORE-VERSION: <sdk version>",
		},
	})
}

func (s *Server) handleVersionRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	results := s.vers.Refresh(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// ---- cooldowns ------------------------------------------------------------

func (s *Server) handleCooldowns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		module := r.URL.Query().Get("module")
		items, err := s.cool.List(module)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if items == nil {
			items = []cooldown.View{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"cooldowns": items})

	case http.MethodPost:
		var in struct {
			Module    string `json:"module"`
			KeyID     int64  `json:"keyId"`
			Model     string `json:"model"`
			Minutes   int    `json:"minutes"`
			UntilUnix int64  `json:"untilUnix"`
			Reason    string `json:"reason"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		until := time.Unix(in.UntilUnix, 0)
		if in.UntilUnix == 0 {
			if in.Minutes <= 0 {
				in.Minutes = 60
			}
			until = time.Now().Add(time.Duration(in.Minutes) * time.Minute)
		}
		if err := s.cool.Add(in.Module, in.KeyID, in.Model, until, in.Reason); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (s *Server) handleCooldownItem(w http.ResponseWriter, r *http.Request, idRaw string) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid cooldown id"})
		return
	}
	if err := s.cool.Release(id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- stats ----------------------------------------------------------------

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	day := r.URL.Query().Get("day")
	if day == "" {
		day = time.Now().Format(store.DayLayout)
	}
	module := r.URL.Query().Get("module")
	if module != "" && validModule(module) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid module"})
		return
	}

	summary, err := s.store.SummaryFor(day, module)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	breakdown, err := s.store.BreakdownFor(day, module)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	days, err := s.store.AvailableDays(30)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if breakdown == nil {
		breakdown = []store.StatRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"day": day, "module": module, "summary": summary,
		"breakdown": breakdown, "days": days,
	})
}

// ---- 静态资源 -------------------------------------------------------------

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		http.Error(w, "assets unavailable", http.StatusInternalServerError)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin"), "/")
	if path == "" || !fileExists(sub, path) {
		path = "index.html" // SPA 前端路由回退
	}
	f, err := sub.Open(path)
	if err != nil {
		http.Error(w, "asset not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	if name, ok := f.(interface{ Stat() (fs.FileInfo, error) }); ok {
		if info, err := name.Stat(); err == nil {
			switch strings.ToLower(filepath.Ext(path)) {
			case ".css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			case ".js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			case ".html":
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
			case ".svg":
				w.Header().Set("Content-Type", "image/svg+xml")
			case ".json":
				w.Header().Set("Content-Type", "application/json")
			}
			_ = info
		}
	}
	io.Copy(w, f)
}

func fileExists(fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// ---- 工具 -----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(payload)
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid json body: " + err.Error())
	}
	return nil
}

func bearerToken(authz string) string {
	authz = strings.TrimSpace(authz)
	parts := strings.SplitN(authz, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

// subtleEqual 做定长比较，避免口令校验的时序侧信道。
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func randomID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}
