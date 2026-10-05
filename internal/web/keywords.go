package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zengateway/internal/prompt"
	"zengateway/internal/store"
)

// 关键词长度上限。
const maxKeywordBytes = 200

// keywordRow 是管理端的关键词视图。
//
// 显式列出字段（而非内嵌 store.Keyword）以固定 JSON 契约：
// 内嵌会把 Go 字段名直接序列化（大写驼峰），与其余 API 的小写风格不一致，
// 前端容易踩坑；这里统一输出小写字段名。
type keywordRow struct {
	ID        int64  `json:"id"`
	Module    string `json:"module"`
	Keyword   string `json:"keyword"`
	Note      string `json:"note"`
	Enabled   bool   `json:"enabled"`
	SortOrder int    `json:"sortOrder"`
	Hits      int64  `json:"hits"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	IsGlobal  bool   `json:"isGlobal"`
}

func newKeywordRow(k store.Keyword) keywordRow {
	return keywordRow{
		ID: k.ID, Module: k.Module, Keyword: k.Keyword, Note: k.Note,
		Enabled: k.Enabled, SortOrder: k.SortOrder, Hits: k.Hits,
		CreatedAt: k.CreatedAt.Format(time.RFC3339),
		UpdatedAt: k.UpdatedAt.Format(time.RFC3339),
		IsGlobal:  k.Module == store.ModuleAll,
	}
}

// handleKeywords 处理 /admin/api/keywords
//
//	GET  ?module=zen  → 列出关键词
//	POST             → 新增
func (s *Server) handleKeywords(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		module := r.URL.Query().Get("module")
		if module != "" && module != store.ModuleAll {
			if err := validModule(module); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
		}
		rows, err := s.store.ListKeywords(module)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]keywordRow, 0, len(rows))
		for _, k := range rows {
			out = append(out, newKeywordRow(k))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"keywords": out,
			"modules": []map[string]string{
				{"value": "zen", "label": "Zen"},
				{"value": "cline", "label": "Cline"},
				{"value": store.ModuleAll, "label": "全部模块"},
			},
			"note": "系统提示词中出现关键词（忽略大小写）时，整条系统提示词会被替换为对应模块的原生提示词。",
		})

	case http.MethodPost:
		var in keywordPatch
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		k, errMsg := in.toKeyword(store.Keyword{Enabled: true})
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": errMsg})
			return
		}
		created, err := s.store.CreateKeyword(k)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.reloadInject()
		writeJSON(w, http.StatusOK, map[string]any{"keyword": newKeywordRow(created)})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// handleKeywordItem 处理 /admin/api/keywords/{id}
func (s *Server) handleKeywordItem(w http.ResponseWriter, r *http.Request, idRaw string) {
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid keyword id"})
		return
	}

	switch r.Method {
	case http.MethodPut:
		cur, err := s.store.GetKeyword(id)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		var in keywordPatch
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		updated, errMsg := in.toKeyword(cur)
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": errMsg})
			return
		}
		saved, err := s.store.UpdateKeyword(updated)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.reloadInject()
		writeJSON(w, http.StatusOK, map[string]any{"keyword": newKeywordRow(saved)})

	case http.MethodDelete:
		if err := s.store.DeleteKeyword(id); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, store.ErrNotFound) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		s.reloadInject()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// handleKeywordReorder 处理 /admin/api/keywords/reorder
func (s *Server) handleKeywordReorder(w http.ResponseWriter, r *http.Request) {
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
	if err := s.store.ReorderKeywords(in.IDs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.reloadInject()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleKeywordResetHits 处理 /admin/api/keywords/{id}/reset-hits
func (s *Server) handleKeywordResetHits(w http.ResponseWriter, r *http.Request, idRaw string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	id, err := strconv.ParseInt(idRaw, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid keyword id"})
		return
	}
	if err := s.store.ResetKeywordHits(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 同步丢弃内存中未落盘的计数，避免清零后回弹
	if s.inj != nil {
		s.inj.ResetHits()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleKeywordTest 处理 /admin/api/keywords/test
// 对样例文本执行检测，报告命中的关键词与将写入的提示词，不落库、不影响请求。
func (s *Server) handleKeywordTest(w http.ResponseWriter, r *http.Request) {
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
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "样例文本过长（上限 200000 字节）"})
		return
	}

	result := map[string]any{
		"input":        in.Text,
		"hit":          false,
		"promptSource": prompt.Source(in.Module),
		"promptLength": len(prompt.Resolve(in.Module)),
		"hasPrompt":    prompt.Resolve(in.Module) != "",
	}
	if s.inj != nil {
		if rule, hit := s.inj.Detect(in.Module, in.Text); hit {
			result["hit"] = true
			result["matchedKeyword"] = rule.Keyword
			result["ruleId"] = rule.ID
			result["ruleNote"] = rule.Note
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// handlePromptOverrides 处理 /admin/api/prompts
//
//	GET  ?module=zen  → 返回当前生效的提示词（含来源与内置值，便于对比/恢复）
//	PUT              → 设置或清除覆盖
func (s *Server) handlePromptOverrides(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		module := r.URL.Query().Get("module")
		if err := validModule(module); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		override, err := s.store.PromptOverride(module)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"module":     module,
			"effective":  prompt.Resolve(module),
			"builtin":    prompt.Module(module),
			"override":   override,
			"source":     prompt.Source(module),
			"hasBuiltin": prompt.HasBuiltin(module),
		})

	case http.MethodPut:
		var in struct {
			Module string  `json:"module"`
			Text   *string `json:"text"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := validModule(in.Module); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if in.Text == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "text is required"})
			return
		}
		if len(*in.Text) > 1<<20 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "提示词过长（上限 1MiB）"})
			return
		}
		if err := s.store.SetPromptOverride(in.Module, *in.Text); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.reloadInject()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"source": prompt.Source(in.Module),
		})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// reloadInject 让注入引擎立即加载最新配置。
func (s *Server) reloadInject() {
	if s.inj != nil {
		s.inj.Reload()
	}
}

// keywordPatch 是创建/更新关键词的请求体（指针字段用于区分「未提供」与「置空」）。
type keywordPatch struct {
	Module  *string `json:"module"`
	Keyword *string `json:"keyword"`
	Note    *string `json:"note"`
	Enabled *bool   `json:"enabled"`
}

func (p keywordPatch) toKeyword(cur store.Keyword) (store.Keyword, string) {
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
	if p.Keyword != nil {
		cur.Keyword = *p.Keyword
	}
	if strings.TrimSpace(cur.Keyword) == "" {
		return cur, "关键词不能为空（空关键词会匹配一切）"
	}
	if len(cur.Keyword) > maxKeywordBytes {
		return cur, fmt.Sprintf("关键词过长（上限 %d 字节）", maxKeywordBytes)
	}
	if p.Note != nil {
		cur.Note = truncateRunes(strings.TrimSpace(*p.Note), 200)
	}
	if p.Enabled != nil {
		cur.Enabled = *p.Enabled
	}
	return cur, ""
}

// truncateRunes 按字符（而非字节）截断，避免把多字节字符切成非法 UTF-8。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
