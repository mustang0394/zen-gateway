package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"zengateway/internal/rewrite"
	"zengateway/internal/store"
)

// handleRewrites 处理 /admin/api/rewrites
//
//	GET  ?module=zen  → 列出规则（含编译状态，便于发现被跳过的无效规则）
//	POST             → 新增规则
func (s *Server) handleRewrites(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		module := r.URL.Query().Get("module")
		if module != "" && module != store.ModuleAll {
			if err := validModule(module); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
		}
		rules, err := s.store.ListRewriteRules(module)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		type row struct {
			store.RewriteRule
			CompileError string `json:"compileError"`
		}
		out := make([]row, 0, len(rules))
		for _, rule := range rules {
			item := row{RewriteRule: rule}
			// 校验规则是否可编译：无效规则会被引擎跳过，需要让用户看见
			if _, err := rewrite.CompileRule(rule); err != nil {
				item.CompileError = err.Error()
			}
			out = append(out, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"rules":  out,
			"scopes": rewriteScopeOptions(),
			"modules": []map[string]string{
				{"value": "zen", "label": "Zen"},
				{"value": "cline", "label": "Cline"},
				{"value": store.ModuleAll, "label": "全部模块"},
			},
			"note": "改写会改变发往上游的提示词文本；同一提示词前缀变化可能导致上游缓存失效。",
		})

	case http.MethodPost:
		var in rewritePatch
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		// 新建规则的默认值：
		//   Enabled 默认 true（否则零值 false 会让规则静默失效）
		//   CaseSensitive 默认 true（区分大小写；忽略大小写会扩大匹配面、提高误伤概率）
		rule, errMsg := in.toRule(store.RewriteRule{Enabled: true, CaseSensitive: true})
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": errMsg})
			return
		}
		created, err := s.store.CreateRewriteRule(rule)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.reloadRewrites()
		writeJSON(w, http.StatusOK, map[string]any{"rule": created})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// handleRewriteItem 处理 /admin/api/rewrites/{id}
func (s *Server) handleRewriteItem(w http.ResponseWriter, r *http.Request, idRaw string) {
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid rule id"})
		return
	}

	switch r.Method {
	case http.MethodPut:
		cur, err := s.store.GetRewriteRule(id)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		var in rewritePatch
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		updated, errMsg := in.toRule(cur)
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": errMsg})
			return
		}
		saved, err := s.store.UpdateRewriteRule(updated)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.reloadRewrites()
		writeJSON(w, http.StatusOK, map[string]any{"rule": saved})

	case http.MethodDelete:
		if err := s.store.DeleteRewriteRule(id); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		s.reloadRewrites()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// handleRewriteReorder 处理 /admin/api/rewrites/reorder（顺序决定链式替换结果）
func (s *Server) handleRewriteReorder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.store.ReorderRewriteRules(in.IDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.reloadRewrites()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRewriteResetHits 处理 /admin/api/rewrites/{id}/reset-hits
func (s *Server) handleRewriteResetHits(w http.ResponseWriter, r *http.Request, idRaw string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid rule id"})
		return
	}
	if err := s.store.ResetRewriteHits(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 同步丢弃引擎内存中尚未落库的计数，否则下一轮落盘会把旧值加回
	if s.rw != nil {
		s.rw.ResetHits()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRewritePreview 处理 /admin/api/rewrites/preview
// 对样例文本试跑规则并返回结果与命中列表，不落库、不影响线上规则。
func (s *Server) handleRewritePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var in struct {
		Module string `json:"module"`
		Text   string `json:"text"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := validModule(in.Module); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(in.Text) > 200_000 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "sample text too long (max 200000 chars)"})
		return
	}
	if s.rw == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"input": in.Text, "result": in.Text, "hits": []any{}, "changed": false,
		})
		return
	}
	out, hits := s.rw.Preview(in.Module, in.Text)
	if hits == nil {
		hits = []rewrite.Hit{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"input":   in.Text,
		"result":  out,
		"hits":    hits,
		"changed": out != in.Text,
	})
}

// reloadRewrites 让引擎立即加载最新规则（写入后调用，避免等周期重载）。
func (s *Server) reloadRewrites() {
	if s.rw != nil {
		s.rw.Reload()
	}
}

// 规则字段长度上限。match 同样需要限制：它会在每次 Reload 与管理端列表查询时
// 被重新编译，并在每个转发请求的每段文本上参与匹配。
const (
	maxRuleNameRunes    = 100
	maxRuleMatchBytes   = 1_000
	maxRuleReplaceBytes = 10_000
)

// truncateRunes 按字符（而非字节）截断，避免把多字节字符切成非法 UTF-8。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// rewritePatch 是创建/更新规则的请求体。指针字段用于区分「未提供」与「显式置零」。
type rewritePatch struct {
	Module        *string `json:"module"`
	Name          *string `json:"name"`
	Match         *string `json:"match"`
	Replace       *string `json:"replace"`
	IsRegex       *bool   `json:"isRegex"`
	CaseSensitive *bool   `json:"caseSensitive"`
	Scope         *string `json:"scope"`
	IncludeTools  *bool   `json:"includeTools"`
	Enabled       *bool   `json:"enabled"`
}

// toRule 基于 cur 合并补丁并校验，返回可直接入库的规则。
func (p rewritePatch) toRule(cur store.RewriteRule) (store.RewriteRule, string) {
	if p.Module != nil {
		m := strings.TrimSpace(*p.Module)
		if m != store.ModuleAll {
			if err := validModule(m); err != nil {
				return cur, err.Error()
			}
		}
		cur.Module = m
	}
	if cur.Module == "" {
		return cur, "module 必须是 zen、cline 或 *"
	}
	if p.Name != nil {
		cur.Name = truncateRunes(strings.TrimSpace(*p.Name), maxRuleNameRunes)
	}
	if p.Match != nil {
		cur.Match = *p.Match
		if len(cur.Match) > maxRuleMatchBytes {
			return cur, fmt.Sprintf("匹配内容过长（上限 %d 字节）", maxRuleMatchBytes)
		}
	}
	if p.Replace != nil {
		cur.Replace = *p.Replace
		if len(cur.Replace) > maxRuleReplaceBytes {
			return cur, fmt.Sprintf("替换内容过长（上限 %d 字节）", maxRuleReplaceBytes)
		}
	}
	if p.IsRegex != nil {
		cur.IsRegex = *p.IsRegex
	}
	if p.CaseSensitive != nil {
		cur.CaseSensitive = *p.CaseSensitive
	}
	if p.Scope != nil {
		cur.Scope = strings.TrimSpace(*p.Scope)
	}
	if cur.Scope == "" {
		cur.Scope = store.ScopeSystemFirstUser
	}
	if !store.IsValidScope(cur.Scope) {
		return cur, "scope 必须是 " + strings.Join(store.ValidScopes(), " / ")
	}
	if p.IncludeTools != nil {
		cur.IncludeTools = *p.IncludeTools
	}
	if p.Enabled != nil {
		cur.Enabled = *p.Enabled
	}

	// 保存前校验：拒绝空匹配与非法正则，避免把坏规则写进库
	if err := rewrite.ValidateRuleText(cur.Match, cur.IsRegex, cur.CaseSensitive); err != nil {
		return cur, err.Error()
	}
	return cur, ""
}

func rewriteScopeOptions() []map[string]string {
	return []map[string]string{
		{"value": store.ScopeSystem, "label": "仅系统提示词"},
		{"value": store.ScopeSystemFirstUser, "label": "系统提示词 + 首条用户消息"},
		{"value": store.ScopeMessages, "label": "全部消息"},
	}
}
