// Package store 提供网关的 SQLite 持久化层。
//
// 设计要点：
//   - 使用纯 Go 驱动 modernc.org/sqlite，保持 CGO_ENABLED=0 静态构建与跨平台发布矩阵
//   - 版本化迁移（schema_migrations 表），幂等、可重复启动
//   - 统计与请求明细走内存缓冲 + 批量事务写入，避免高频请求打爆磁盘 IO
//   - 单写连接（SetMaxOpenConns(1)）配合 WAL，实测并发写无 SQLITE_BUSY
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound 表示目标记录不存在。
var ErrNotFound = errors.New("store: not found")

// DayLayout 是统计使用的日期键格式（本地时区）。
const DayLayout = "2006-01-02"

// DefaultFlushInterval 是统计缓冲的默认落盘间隔。
const DefaultFlushInterval = 5 * time.Second

// Store 是 SQLite 持久化句柄。
type Store struct {
	db  *sql.DB
	log *slog.Logger

	buf           *statBuffer
	flushInterval time.Duration // 启动时确定，运行期不变

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// migrations 是有序迁移列表。表结构变更只允许在末尾追加，不得修改已发布项。
var migrations = []string{
	`CREATE TABLE upstream_keys (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		module TEXT NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		note TEXT NOT NULL DEFAULT '',
		api_key TEXT NOT NULL,
		proxy TEXT NOT NULL DEFAULT '',
		sort_order INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER NOT NULL DEFAULT 1,
		is_anonymous INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		deleted_at INTEGER)`,
	`CREATE INDEX ix_keys_module ON upstream_keys(module, deleted_at, sort_order, id)`,
	`CREATE TABLE versions (
		name TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		fetched_at INTEGER NOT NULL,
		ok INTEGER NOT NULL,
		error TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE cooldowns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		module TEXT NOT NULL,
		key_id INTEGER NOT NULL,
		model TEXT NOT NULL,
		until_ts INTEGER NOT NULL,
		reason TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		UNIQUE(module, key_id, model))`,
	`CREATE INDEX ix_cooldowns_lookup ON cooldowns(module, model, until_ts)`,
	`CREATE TABLE stats (
		day TEXT NOT NULL,
		module TEXT NOT NULL,
		key_id INTEGER NOT NULL,
		model TEXT NOT NULL,
		requests INTEGER NOT NULL DEFAULT 0,
		errors INTEGER NOT NULL DEFAULT 0,
		retries INTEGER NOT NULL DEFAULT 0,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		cached_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		ttft_sum_ms INTEGER NOT NULL DEFAULT 0,
		ttft_count INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (day, module, key_id, model)) WITHOUT ROWID`,
	`CREATE TABLE request_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		day TEXT NOT NULL,
		module TEXT NOT NULL,
		key_id INTEGER NOT NULL,
		model TEXT NOT NULL,
		status INTEGER NOT NULL,
		latency_ms INTEGER NOT NULL,
		ttft_ms INTEGER,
		prompt_tokens INTEGER,
		completion_tokens INTEGER,
		cached_tokens INTEGER,
		total_tokens INTEGER,
		error TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX ix_request_log_day ON request_log(day)`,
	`CREATE TABLE settings (
		k TEXT PRIMARY KEY,
		v TEXT NOT NULL,
		updated_at INTEGER NOT NULL)`,
}

// Open 打开（必要时创建）数据库并执行迁移，随后启动统计落盘协程。
func Open(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"+
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// 单写连接：modernc + WAL 下最稳，避免并发写竞争 SQLITE_BUSY
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{
		db:            db,
		log:           log,
		buf:           newStatBuffer(),
		flushInterval: DefaultFlushInterval,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	go s.flushLoop()
	return s, nil
}

// Close 停止后台协程、落盘剩余统计并关闭连接。
func (s *Store) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
	if err := s.flush(); err != nil {
		s.log.Warn("final stats flush failed", "err", err)
	}
	return s.db.Close()
}

// migrate 按 schema_migrations 记录逐条应用未执行的迁移。
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i, stmt := range migrations {
		version := i + 1
		if version <= current {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", version, err)
		}
		if _, err := tx.Exec(stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
			version, time.Now().Unix()); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}
		s.log.Info("db migration applied", "version", version)
	}
	return nil
}

func (s *Store) flushLoop() {
	defer close(s.done)
	t := time.NewTicker(s.flushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			if err := s.flush(); err != nil {
				s.log.Warn("stats flush failed", "err", err)
			}
		}
	}
}

// ---- 内部工具 -------------------------------------------------------------

func unix(t time.Time) int64 { return t.Unix() }

func timeFromUnix(v int64) time.Time { return time.Unix(v, 0) }

func boolFromInt(v int64) bool { return v != 0 }

func intFromBool(b bool) int {
	if b {
		return 1
	}
	return 0
}
