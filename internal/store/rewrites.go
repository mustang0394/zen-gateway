package store

import (
	"database/sql"
	"fmt"
	"time"
)

// 规则作用域：控制哪些文本会被改写。
const (
	// ScopeSystem 只改系统角色文本（chat 的 system/developer 消息、
	// responses 的 instructions 与 input 中的 system/developer 项）。
	ScopeSystem = "system"
	// ScopeSystemFirstUser 在 ScopeSystem 基础上，额外改首条 user 消息。
	// 不少客户端把系统提示词塞在第一条 user 消息里，因此这是默认值。
	ScopeSystemFirstUser = "system_first_user"
	// ScopeMessages 改所有角色的文本（含 assistant 历史与 tool 结果），覆盖最全但误伤风险最高。
	ScopeMessages = "messages"
)

// 模块取值：具体模块名，或 "*" 表示对所有模块生效。
const ModuleAll = "*"

// ValidScopes 返回全部合法作用域。
func ValidScopes() []string { return []string{ScopeSystem, ScopeSystemFirstUser, ScopeMessages} }

// IsValidScope 校验作用域取值。
func IsValidScope(scope string) bool {
	for _, s := range ValidScopes() {
		if s == scope {
			return true
		}
	}
	return false
}

// RewriteRule 是一条系统提示词改写规则。
type RewriteRule struct {
	ID            int64
	Module        string // zen | cline | *
	Name          string
	Match         string
	Replace       string
	IsRegex       bool
	CaseSensitive bool
	Scope         string // system | system_first_user | messages
	IncludeTools  bool   // 是否同时改写 tools 的描述文本
	Enabled       bool
	SortOrder     int
	Hits          int64 // 累计命中次数（仅用于观察规则是否生效）
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const rewriteRuleColumns = `id, module, name, match, replace, is_regex, case_sensitive,
	scope, include_tools, enabled, sort_order, hits, created_at, updated_at`

func scanRewriteRule(sc interface{ Scan(...any) error }) (RewriteRule, error) {
	var r RewriteRule
	var isRegex, caseSensitive, includeTools, enabled int
	var created, updated int64
	err := sc.Scan(&r.ID, &r.Module, &r.Name, &r.Match, &r.Replace, &isRegex, &caseSensitive,
		&r.Scope, &includeTools, &enabled, &r.SortOrder, &r.Hits, &created, &updated)
	if err != nil {
		return RewriteRule{}, err
	}
	r.IsRegex = boolFromInt(int64(isRegex))
	r.CaseSensitive = boolFromInt(int64(caseSensitive))
	r.IncludeTools = boolFromInt(int64(includeTools))
	r.Enabled = boolFromInt(int64(enabled))
	r.CreatedAt = timeFromUnix(created)
	r.UpdatedAt = timeFromUnix(updated)
	return r, nil
}

// ListRewriteRules 返回对指定模块有效的全部未删除规则，按 sort_order 升序。
//
// module 为空时返回全部规则；否则除了该模块自身的规则，还包含 module='*'
// 的全局规则——因为全局规则同样会影响该模块，若在管理端被过滤掉会让用户
// 误判「哪些规则正在生效」（与 ActiveRewriteRules 的口径保持一致）。
func (s *Store) ListRewriteRules(module string) ([]RewriteRule, error) {
	q := `SELECT ` + rewriteRuleColumns + ` FROM rewrite_rules WHERE deleted_at IS NULL`
	args := []any{}
	if module != "" {
		q += ` AND (module = ? OR module = ?)`
		args = append(args, module, ModuleAll)
	}
	q += ` ORDER BY sort_order ASC, id ASC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list rewrite rules: %w", err)
	}
	defer rows.Close()

	var out []RewriteRule
	for rows.Next() {
		r, err := scanRewriteRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan rewrite rule: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveRewriteRules 返回对指定模块生效且已启用的规则（含 module='*'），
// 供改写引擎构建内存快照使用。
func (s *Store) ActiveRewriteRules(module string) ([]RewriteRule, error) {
	rows, err := s.db.Query(`SELECT `+rewriteRuleColumns+` FROM rewrite_rules
		WHERE deleted_at IS NULL AND enabled = 1 AND (module = ? OR module = ?)
		ORDER BY sort_order ASC, id ASC`, module, ModuleAll)
	if err != nil {
		return nil, fmt.Errorf("active rewrite rules: %w", err)
	}
	defer rows.Close()

	var out []RewriteRule
	for rows.Next() {
		r, err := scanRewriteRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan rewrite rule: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRewriteRule 按 id 读取未删除规则。
func (s *Store) GetRewriteRule(id int64) (RewriteRule, error) {
	row := s.db.QueryRow(`SELECT `+rewriteRuleColumns+` FROM rewrite_rules
		WHERE id = ? AND deleted_at IS NULL`, id)
	r, err := scanRewriteRule(row)
	if err == sql.ErrNoRows {
		return RewriteRule{}, ErrNotFound
	}
	if err != nil {
		return RewriteRule{}, fmt.Errorf("get rewrite rule: %w", err)
	}
	return r, nil
}

// CreateRewriteRule 新增规则；SortOrder 为零时排到末尾。
func (s *Store) CreateRewriteRule(r RewriteRule) (RewriteRule, error) {
	now := time.Now()
	if r.Scope == "" {
		r.Scope = ScopeSystemFirstUser
	}
	if r.SortOrder == 0 {
		var max sql.NullInt64
		if err := s.db.QueryRow(`SELECT MAX(sort_order) FROM rewrite_rules
			WHERE deleted_at IS NULL`).Scan(&max); err != nil {
			return RewriteRule{}, fmt.Errorf("max sort_order: %w", err)
		}
		r.SortOrder = int(max.Int64) + 1
	}
	res, err := s.db.Exec(`INSERT INTO rewrite_rules
		(module, name, match, replace, is_regex, case_sensitive, scope, include_tools,
		 enabled, sort_order, hits, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		r.Module, r.Name, r.Match, r.Replace, intFromBool(r.IsRegex), intFromBool(r.CaseSensitive),
		r.Scope, intFromBool(r.IncludeTools), intFromBool(r.Enabled), r.SortOrder,
		unix(now), unix(now))
	if err != nil {
		return RewriteRule{}, fmt.Errorf("insert rewrite rule: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return RewriteRule{}, fmt.Errorf("last insert id: %w", err)
	}
	return s.GetRewriteRule(id)
}

// UpdateRewriteRule 更新规则全部可变字段。
func (s *Store) UpdateRewriteRule(r RewriteRule) (RewriteRule, error) {
	if r.Scope == "" {
		r.Scope = ScopeSystemFirstUser
	}
	res, err := s.db.Exec(`UPDATE rewrite_rules SET
		module = ?, name = ?, match = ?, replace = ?, is_regex = ?, case_sensitive = ?,
		scope = ?, include_tools = ?, enabled = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`,
		r.Module, r.Name, r.Match, r.Replace, intFromBool(r.IsRegex), intFromBool(r.CaseSensitive),
		r.Scope, intFromBool(r.IncludeTools), intFromBool(r.Enabled), unix(time.Now()), r.ID)
	if err != nil {
		return RewriteRule{}, fmt.Errorf("update rewrite rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return RewriteRule{}, ErrNotFound
	}
	return s.GetRewriteRule(r.ID)
}

// DeleteRewriteRule 软删除规则。
func (s *Store) DeleteRewriteRule(id int64) error {
	res, err := s.db.Exec(`UPDATE rewrite_rules SET deleted_at = ?
		WHERE id = ? AND deleted_at IS NULL`, unix(time.Now()), id)
	if err != nil {
		return fmt.Errorf("delete rewrite rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReorderRewriteRules 按传入 id 顺序重排 sort_order（规则按序执行，顺序会影响链式结果）。
//
// 已知语义：只有被提交的 id 会被重新编号。管理端的按模块过滤视图会包含
// module='*' 的全局规则，因此在某模块视图内排序后，未出现在该视图的其他规则
// 会保留原 sort_order，可能出现并列——此时由 id 兜底（ORDER BY sort_order, id），
// 结果仍是确定性的，且同一模块内部的相对顺序与用户所见一致。
//
// 无法做到「全局规则在两个模块里都排到指定位置」：全局规则只有一个 sort_order，
// 而两个模块的视图可能对它给出不同期望，这是模型的固有限制。
func (s *Store) ReorderRewriteRules(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin reorder: %w", err)
	}
	defer tx.Rollback()
	now := unix(time.Now())
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE rewrite_rules SET sort_order = ?, updated_at = ?
			WHERE id = ? AND deleted_at IS NULL`, i+1, now, id); err != nil {
			return fmt.Errorf("reorder rule %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// AddRewriteHits 累加规则的命中次数（批量，由统计缓冲定期调用）。
func (s *Store) AddRewriteHits(hits map[int64]int64) error {
	if len(hits) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin hits: %w", err)
	}
	defer tx.Rollback()
	for id, n := range hits {
		if _, err := tx.Exec(`UPDATE rewrite_rules SET hits = hits + ? WHERE id = ?`, n, id); err != nil {
			return fmt.Errorf("add hits rule %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// ResetRewriteHits 清零某条规则的命中计数（便于重新观察）。
func (s *Store) ResetRewriteHits(id int64) error {
	if _, err := s.db.Exec(`UPDATE rewrite_rules SET hits = 0, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`, unix(time.Now()), id); err != nil {
		return fmt.Errorf("reset hits: %w", err)
	}
	return nil
}
