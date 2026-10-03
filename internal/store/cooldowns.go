package store

import (
	"fmt"
	"time"
)

// Cooldown 表示某个 key 在某个模型上的冷却记录。
type Cooldown struct {
	ID        int64
	Module    string
	KeyID     int64
	Model     string
	Until     time.Time
	Reason    string
	Detail    string
	CreatedAt time.Time
}

// SetCooldown 写入或覆盖冷却记录（同一 module+key+model 唯一）。
func (s *Store) SetCooldown(c Cooldown) error {
	_, err := s.db.Exec(`INSERT INTO cooldowns(module, key_id, model, until_ts, reason, detail, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(module, key_id, model) DO UPDATE SET
		  until_ts = excluded.until_ts, reason = excluded.reason, detail = excluded.detail,
		  created_at = excluded.created_at`,
		c.Module, c.KeyID, c.Model, unix(c.Until), c.Reason, truncate(c.Detail, 200), unix(c.CreatedAt))
	if err != nil {
		return fmt.Errorf("set cooldown: %w", err)
	}
	return nil
}

// ListCooldowns 返回未过期的冷却记录（detail 一并带出便于页面展示）。
func (s *Store) ListCooldowns(module string, now time.Time) ([]Cooldown, error) {
	q := `SELECT id, module, key_id, model, until_ts, reason, detail, created_at
		FROM cooldowns WHERE until_ts > ?`
	args := []any{unix(now)}
	if module != "" {
		q += ` AND module = ?`
		args = append(args, module)
	}
	q += ` ORDER BY until_ts DESC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list cooldowns: %w", err)
	}
	defer rows.Close()

	var out []Cooldown
	for rows.Next() {
		var c Cooldown
		var until, created int64
		if err := rows.Scan(&c.ID, &c.Module, &c.KeyID, &c.Model, &until, &c.Reason, &c.Detail, &created); err != nil {
			return nil, fmt.Errorf("scan cooldown: %w", err)
		}
		c.Until = timeFromUnix(until)
		c.CreatedAt = timeFromUnix(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCooldown 手动解除一条冷却记录。
func (s *Store) DeleteCooldown(id int64) error {
	res, err := s.db.Exec(`DELETE FROM cooldowns WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete cooldown: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteCooldownsByKey 删除某 key 的全部冷却记录（删除 key 时一并清理）。
func (s *Store) DeleteCooldownsByKey(keyID int64) error {
	if _, err := s.db.Exec(`DELETE FROM cooldowns WHERE key_id = ?`, keyID); err != nil {
		return fmt.Errorf("delete key cooldowns: %w", err)
	}
	return nil
}

// PurgeExpiredCooldowns 清理已过期的冷却记录，返回清理条数。
func (s *Store) PurgeExpiredCooldowns(now time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM cooldowns WHERE until_ts <= ?`, unix(now))
	if err != nil {
		return 0, fmt.Errorf("purge cooldowns: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func truncate(v string, max int) string {
	if len(v) <= max {
		return v
	}
	return v[:max]
}
