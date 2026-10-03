package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// 设置项键名。
const (
	SettingAccessTokenZen   = "access_token.zen"   // 下游调用 zen 模块所需 token（空 = 允许匿名）
	SettingAccessTokenCline = "access_token.cline" // 下游调用 cline 模块所需 token（必填）
	SettingCooldownFallback = "cooldown.fallback_seconds"
	SettingRetentionDays    = "retention.days"
)

// GetSetting 读取设置项；不存在返回 def。
func (s *Store) GetSetting(key, def string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM settings WHERE k = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return def, nil
	}
	if err != nil {
		return def, fmt.Errorf("get setting %s: %w", key, err)
	}
	return v, nil
}

// SetSetting 写入设置项。
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(k, v, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v, updated_at = excluded.updated_at`,
		key, value, unix(time.Now()))
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// GetIntSetting 读取整数设置项。
func (s *Store) GetIntSetting(key string, def int) (int, error) {
	v, err := s.GetSetting(key, "")
	if err != nil {
		return def, err
	}
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, nil
	}
	return n, nil
}

// AllSettings 返回全部设置项。
func (s *Store) AllSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT k, v FROM settings ORDER BY k`)
	if err != nil {
		return nil, fmt.Errorf("all settings: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}
