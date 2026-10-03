package store

import (
	"database/sql"
	"fmt"
	"time"
)

// APIKey 是上游 key 记录（含备注与代理）。
type APIKey struct {
	ID          int64
	Module      string
	Label       string
	Note        string
	APIKey      string
	Proxy       string
	SortOrder   int
	Enabled     bool
	IsAnonymous bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const keyColumns = `id, module, label, note, api_key, proxy, sort_order, enabled,
	is_anonymous, created_at, updated_at`

func scanKey(sc interface{ Scan(...any) error }) (APIKey, error) {
	var k APIKey
	var created, updated int64
	var enabled, anon int
	err := sc.Scan(&k.ID, &k.Module, &k.Label, &k.Note, &k.APIKey, &k.Proxy, &k.SortOrder,
		&enabled, &anon, &created, &updated)
	if err != nil {
		return APIKey{}, err
	}
	k.Enabled = boolFromInt(int64(enabled))
	k.IsAnonymous = boolFromInt(int64(anon))
	k.CreatedAt = timeFromUnix(created)
	k.UpdatedAt = timeFromUnix(updated)
	return k, nil
}

// ListKeys 返回模块下未删除的 key，按 sort_order 升序。
func (s *Store) ListKeys(module string) ([]APIKey, error) {
	rows, err := s.db.Query(`SELECT `+keyColumns+` FROM upstream_keys
		WHERE module = ? AND deleted_at IS NULL ORDER BY sort_order ASC, id ASC`, module)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetKey 按 id 读取未删除的 key。
func (s *Store) GetKey(id int64) (APIKey, error) {
	row := s.db.QueryRow(`SELECT `+keyColumns+` FROM upstream_keys
		WHERE id = ? AND deleted_at IS NULL`, id)
	k, err := scanKey(row)
	if err == sql.ErrNoRows {
		return APIKey{}, ErrNotFound
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("get key: %w", err)
	}
	return k, nil
}

// CreateKey 新增 key；SortOrder 为零值时自动排到末尾。
func (s *Store) CreateKey(k APIKey) (APIKey, error) {
	now := time.Now()
	if k.SortOrder == 0 {
		var max sql.NullInt64
		if err := s.db.QueryRow(`SELECT MAX(sort_order) FROM upstream_keys
			WHERE module = ? AND deleted_at IS NULL`, k.Module).Scan(&max); err != nil {
			return APIKey{}, fmt.Errorf("max sort_order: %w", err)
		}
		k.SortOrder = int(max.Int64) + 1
	}
	res, err := s.db.Exec(`INSERT INTO upstream_keys
		(module, label, note, api_key, proxy, sort_order, enabled, is_anonymous, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.Module, k.Label, k.Note, k.APIKey, k.Proxy, k.SortOrder,
		intFromBool(k.Enabled), intFromBool(k.IsAnonymous), unix(now), unix(now))
	if err != nil {
		return APIKey{}, fmt.Errorf("insert key: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return APIKey{}, fmt.Errorf("last insert id: %w", err)
	}
	return s.GetKey(id)
}

// UpdateKey 更新可变字段（标签、备注、api key、代理、启用状态、匿名标记）。
func (s *Store) UpdateKey(k APIKey) (APIKey, error) {
	res, err := s.db.Exec(`UPDATE upstream_keys SET
		label = ?, note = ?, api_key = ?, proxy = ?, enabled = ?, is_anonymous = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`,
		k.Label, k.Note, k.APIKey, k.Proxy, intFromBool(k.Enabled), intFromBool(k.IsAnonymous),
		unix(time.Now()), k.ID)
	if err != nil {
		return APIKey{}, fmt.Errorf("update key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return APIKey{}, ErrNotFound
	}
	return s.GetKey(k.ID)
}

// DeleteKey 软删除 key（保留统计与日志关联）。
func (s *Store) DeleteKey(id int64) error {
	res, err := s.db.Exec(`UPDATE upstream_keys SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		unix(time.Now()), id)
	if err != nil {
		return fmt.Errorf("delete key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReorderKeys 按传入的 id 顺序重排 sort_order。
func (s *Store) ReorderKeys(module string, ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin reorder: %w", err)
	}
	defer tx.Rollback()
	now := unix(time.Now())
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE upstream_keys SET sort_order = ?, updated_at = ?
			WHERE id = ? AND module = ? AND deleted_at IS NULL`, i+1, now, id, module); err != nil {
			return fmt.Errorf("reorder key %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// PickKey 按顺序返回第一个可用 key：启用、未删除、且未在指定模型的冷却期内。
// 这是顺序轮询的核心查询——冷却维度含 model，因此某 key 在模型 A 冷却时仍可为模型 B 服务。
func (s *Store) PickKey(module, model string, now time.Time) (APIKey, error) {
	row := s.db.QueryRow(`SELECT `+keyColumns+` FROM upstream_keys k
		WHERE k.module = ? AND k.deleted_at IS NULL AND k.enabled = 1
		  AND NOT EXISTS (
			SELECT 1 FROM cooldowns c
			WHERE c.module = k.module AND c.key_id = k.id AND c.model = ? AND c.until_ts > ?)
		ORDER BY k.sort_order ASC, k.id ASC LIMIT 1`, module, model, unix(now))
	k, err := scanKey(row)
	if err == sql.ErrNoRows {
		return APIKey{}, ErrNotFound
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("pick key: %w", err)
	}
	return k, nil
}

// IsCoolingDown 报告指定 key 在指定模型上是否处于冷却期。
// 用于匿名回退条目（key_id = 0）：它不在 upstream_keys 表中，
// 因此无法通过 PickKey 的 NOT EXISTS 子句过滤，需要单独判断，
// 否则 429 后会绕过冷却持续请求上游。
func (s *Store) IsCoolingDown(module string, keyID int64, model string, now time.Time) (bool, error) {
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM cooldowns
		WHERE module = ? AND key_id = ? AND model = ? AND until_ts > ? LIMIT 1`,
		module, keyID, model, unix(now)).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check cooldown: %w", err)
	}
	return true, nil
}

// CountKeys 返回模块下未删除的 key 数量（用于限制换 key 次数）。
func (s *Store) CountKeys(module string) (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM upstream_keys
		WHERE module = ? AND deleted_at IS NULL AND enabled = 1`, module).Scan(&n); err != nil {
		return 0, fmt.Errorf("count keys: %w", err)
	}
	return n, nil
}
