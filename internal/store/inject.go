package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ModuleAll 表示规则对所有上游模块生效（管理端选择「全部模块」）。
const ModuleAll = "*"

// Keyword 是一条关键词检测规则。
//
// 语义：系统提示词中出现该关键词（忽略大小写）时，把整条系统提示词
// 替换为本模块的原生提示词（见 internal/prompt）。
type Keyword struct {
	ID        int64
	Module    string // zen | cline | *
	Keyword   string
	Note      string
	Enabled   bool
	SortOrder int
	Hits      int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

const keywordColumns = `id, module, keyword, note, enabled, sort_order, hits, created_at, updated_at`

func scanKeyword(sc interface{ Scan(...any) error }) (Keyword, error) {
	var k Keyword
	var enabled int
	var created, updated int64
	err := sc.Scan(&k.ID, &k.Module, &k.Keyword, &k.Note, &enabled, &k.SortOrder, &k.Hits,
		&created, &updated)
	if err != nil {
		return Keyword{}, err
	}
	k.Enabled = boolFromInt(int64(enabled))
	k.CreatedAt = timeFromUnix(created)
	k.UpdatedAt = timeFromUnix(updated)
	return k, nil
}

// ListKeywords 返回对指定模块有效的关键词，按 sort_order 升序。
//
// module 为空时返回全部；否则包含该模块自身与 module='*' 的全局关键词
// （与 ActiveInjectKeywords 口径一致，避免管理端展示与实际生效不一致）。
func (s *Store) ListKeywords(module string) ([]Keyword, error) {
	q := `SELECT ` + keywordColumns + ` FROM inject_keywords WHERE deleted_at IS NULL`
	args := []any{}
	if module != "" {
		q += ` AND (module = ? OR module = ?)`
		args = append(args, module, ModuleAll)
	}
	q += ` ORDER BY sort_order ASC, id ASC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list keywords: %w", err)
	}
	defer rows.Close()

	var out []Keyword
	for rows.Next() {
		k, err := scanKeyword(rows)
		if err != nil {
			return nil, fmt.Errorf("scan keyword: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ActiveInjectKeywords 返回对指定模块生效且已启用的关键词（含全局），供注入引擎构建快照。
func (s *Store) ActiveInjectKeywords(module string) ([]Keyword, error) {
	rows, err := s.db.Query(`SELECT `+keywordColumns+` FROM inject_keywords
		WHERE deleted_at IS NULL AND enabled = 1 AND (module = ? OR module = ?)
		ORDER BY sort_order ASC, id ASC`, module, ModuleAll)
	if err != nil {
		return nil, fmt.Errorf("active keywords: %w", err)
	}
	defer rows.Close()

	var out []Keyword
	for rows.Next() {
		k, err := scanKeyword(rows)
		if err != nil {
			return nil, fmt.Errorf("scan keyword: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetKeyword 按 id 读取未删除的关键词。
func (s *Store) GetKeyword(id int64) (Keyword, error) {
	row := s.db.QueryRow(`SELECT `+keywordColumns+` FROM inject_keywords
		WHERE id = ? AND deleted_at IS NULL`, id)
	k, err := scanKeyword(row)
	if err == sql.ErrNoRows {
		return Keyword{}, ErrNotFound
	}
	if err != nil {
		return Keyword{}, fmt.Errorf("get keyword: %w", err)
	}
	return k, nil
}

// CreateKeyword 新增关键词；SortOrder 为零时排到末尾。
func (s *Store) CreateKeyword(k Keyword) (Keyword, error) {
	now := time.Now()
	if k.SortOrder == 0 {
		var max sql.NullInt64
		if err := s.db.QueryRow(`SELECT MAX(sort_order) FROM inject_keywords
			WHERE deleted_at IS NULL`).Scan(&max); err != nil {
			return Keyword{}, fmt.Errorf("max sort_order: %w", err)
		}
		k.SortOrder = int(max.Int64) + 1
	}
	res, err := s.db.Exec(`INSERT INTO inject_keywords
		(module, keyword, note, enabled, sort_order, hits, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
		k.Module, k.Keyword, k.Note, intFromBool(k.Enabled), k.SortOrder, unix(now), unix(now))
	if err != nil {
		return Keyword{}, fmt.Errorf("insert keyword: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Keyword{}, fmt.Errorf("last insert id: %w", err)
	}
	return s.GetKeyword(id)
}

// UpdateKeyword 更新关键词的可变字段。
func (s *Store) UpdateKeyword(k Keyword) (Keyword, error) {
	res, err := s.db.Exec(`UPDATE inject_keywords SET
		module = ?, keyword = ?, note = ?, enabled = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`,
		k.Module, k.Keyword, k.Note, intFromBool(k.Enabled), unix(time.Now()), k.ID)
	if err != nil {
		return Keyword{}, fmt.Errorf("update keyword: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Keyword{}, ErrNotFound
	}
	return s.GetKeyword(k.ID)
}

// DeleteKeyword 软删除关键词。
func (s *Store) DeleteKeyword(id int64) error {
	res, err := s.db.Exec(`UPDATE inject_keywords SET deleted_at = ?
		WHERE id = ? AND deleted_at IS NULL`, unix(time.Now()), id)
	if err != nil {
		return fmt.Errorf("delete keyword: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReorderKeywords 按传入 id 顺序重排 sort_order（决定多条命中时哪条先匹配）。
func (s *Store) ReorderKeywords(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin reorder: %w", err)
	}
	defer tx.Rollback()
	now := unix(time.Now())
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE inject_keywords SET sort_order = ?, updated_at = ?
			WHERE id = ? AND deleted_at IS NULL`, i+1, now, id); err != nil {
			return fmt.Errorf("reorder keyword %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// AddInjectHits 累加关键词命中次数。
func (s *Store) AddInjectHits(hits map[int64]int64) error {
	if len(hits) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin hits: %w", err)
	}
	defer tx.Rollback()
	for id, n := range hits {
		if _, err := tx.Exec(`UPDATE inject_keywords SET hits = hits + ? WHERE id = ?`, n, id); err != nil {
			return fmt.Errorf("add hits %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// ResetKeywordHits 清零某条关键词的命中计数。
func (s *Store) ResetKeywordHits(id int64) error {
	if _, err := s.db.Exec(`UPDATE inject_keywords SET hits = 0, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`, unix(time.Now()), id); err != nil {
		return fmt.Errorf("reset hits: %w", err)
	}
	return nil
}

// ---- 提示词覆盖值 ---------------------------------------------------------

// PromptOverride 返回某模块的自定义提示词；未设置时返回空串。
func (s *Store) PromptOverride(module string) (string, error) {
	var text string
	err := s.db.QueryRow(`SELECT text FROM prompt_overrides WHERE module = ?`, module).Scan(&text)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get prompt override %s: %w", module, err)
	}
	return text, nil
}

// SetPromptOverride 设置某模块的自定义提示词；空串表示删除覆盖、回落到内置提示词。
func (s *Store) SetPromptOverride(module, text string) error {
	if module == "" {
		return fmt.Errorf("module is required")
	}
	if text == "" {
		if _, err := s.db.Exec(`DELETE FROM prompt_overrides WHERE module = ?`, module); err != nil {
			return fmt.Errorf("clear prompt override: %w", err)
		}
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO prompt_overrides(module, text, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(module) DO UPDATE SET text = excluded.text, updated_at = excluded.updated_at`,
		module, text, unix(time.Now()))
	if err != nil {
		return fmt.Errorf("set prompt override %s: %w", module, err)
	}
	return nil
}

// ListPromptOverrides 返回全部自定义提示词。
func (s *Store) ListPromptOverrides() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT module, text FROM prompt_overrides`)
	if err != nil {
		return nil, fmt.Errorf("list prompt overrides: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var m, t string
		if err := rows.Scan(&m, &t); err != nil {
			return nil, fmt.Errorf("scan prompt override: %w", err)
		}
		out[m] = t
	}
	return out, rows.Err()
}
