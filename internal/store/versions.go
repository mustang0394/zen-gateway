package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Version 记录某个版本目标的抓取结果。
type Version struct {
	Name      string // zen | cline.cli | cline.sdk
	Value     string
	FetchedAt time.Time
	OK        bool
	Error     string
}

// SaveVersion 写入（或覆盖）某目标的版本结果。
func (s *Store) SaveVersion(v Version) error {
	_, err := s.db.Exec(`INSERT INTO versions(name, value, fetched_at, ok, error)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
		  value = excluded.value, fetched_at = excluded.fetched_at,
		  ok = excluded.ok, error = excluded.error`,
		v.Name, v.Value, unix(v.FetchedAt), intFromBool(v.OK), v.Error)
	if err != nil {
		return fmt.Errorf("save version %s: %w", v.Name, err)
	}
	return nil
}

// GetVersion 读取某目标的版本记录。
func (s *Store) GetVersion(name string) (Version, error) {
	row := s.db.QueryRow(`SELECT name, value, fetched_at, ok, error FROM versions WHERE name = ?`, name)
	var v Version
	var fetched int64
	var ok int
	if err := row.Scan(&v.Name, &v.Value, &fetched, &ok, &v.Error); err != nil {
		if err == sql.ErrNoRows {
			return Version{}, ErrNotFound
		}
		return Version{}, fmt.Errorf("get version %s: %w", name, err)
	}
	v.FetchedAt = timeFromUnix(fetched)
	v.OK = boolFromInt(int64(ok))
	return v, nil
}

// ListVersions 返回全部版本记录。
func (s *Store) ListVersions() ([]Version, error) {
	rows, err := s.db.Query(`SELECT name, value, fetched_at, ok, error FROM versions ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()

	var out []Version
	for rows.Next() {
		var v Version
		var fetched int64
		var ok int
		if err := rows.Scan(&v.Name, &v.Value, &fetched, &ok, &v.Error); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		v.FetchedAt = timeFromUnix(fetched)
		v.OK = boolFromInt(int64(ok))
		out = append(out, v)
	}
	return out, rows.Err()
}
