package store

import (
	"fmt"
	"sync"
	"time"
)

// StatDelta 是一次请求产生的统计增量（用于批量落盘）。
type StatDelta struct {
	Day              string
	Module           string
	KeyID            int64
	Model            string
	Requests         int64
	Errors           int64
	Retries          int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	TotalTokens      int64
	TTFTSumMs        int64
	TTFTCount        int64
}

// StatRow 是聚合后的统计行（含派生指标）。
type StatRow struct {
	Day              string  `json:"day"`
	Module           string  `json:"module"`
	KeyID            int64   `json:"keyId"`
	KeyLabel         string  `json:"keyLabel"`
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	Retries          int64   `json:"retries"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	AvgTTFTMs        float64 `json:"avgTtftMs"`
	TTFTCount        int64   `json:"ttftCount"`
	CacheHitRate     float64 `json:"cacheHitRate"`
}

// StatSummary 是模块级汇总。
type StatSummary struct {
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	Retries          int64   `json:"retries"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	AvgTTFTMs        float64 `json:"avgTtftMs"`
	TTFTCount        int64   `json:"ttftCount"`
	CacheHitRate     float64 `json:"cacheHitRate"`
}

// ---- 统计缓冲 -------------------------------------------------------------

// statBuffer 在内存中累加统计增量，由 flush 统一落盘。
// 请求高峰期只做内存加法，避免每个请求一次磁盘写。
type statBuffer struct {
	mu     sync.Mutex
	rows   map[string]*StatDelta
	logs   []RequestLog
	logCap int
}

func newStatBuffer() *statBuffer {
	return &statBuffer{rows: map[string]*StatDelta{}, logCap: maxBufferedLogs}
}

// addLog 追加一条请求明细；达到容量上限时返回 true，提示调用方落盘。
func (b *statBuffer) addLog(l RequestLog) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logs = append(b.logs, l)
	return len(b.logs) >= b.logCap
}

func statKey(d StatDelta) string {
	return d.Day + "\x00" + d.Module + "\x00" + fmt.Sprint(d.KeyID) + "\x00" + d.Model
}

// add 累加一条增量；当缓冲行数达到 limit 时返回 true，提示调用方立即落盘。
func (b *statBuffer) add(d StatDelta, limit int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := statKey(d)
	cur, ok := b.rows[k]
	if !ok {
		cp := d
		b.rows[k] = &cp
	} else {
		cur.Requests += d.Requests
		cur.Errors += d.Errors
		cur.Retries += d.Retries
		cur.PromptTokens += d.PromptTokens
		cur.CompletionTokens += d.CompletionTokens
		cur.CachedTokens += d.CachedTokens
		cur.TotalTokens += d.TotalTokens
		cur.TTFTSumMs += d.TTFTSumMs
		cur.TTFTCount += d.TTFTCount
	}
	return len(b.rows) >= limit
}

func (b *statBuffer) drain() ([]StatDelta, []RequestLog) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.rows) == 0 && len(b.logs) == 0 {
		return nil, nil
	}
	out := make([]StatDelta, 0, len(b.rows))
	for _, d := range b.rows {
		out = append(out, *d)
	}
	logs := b.logs
	b.rows = map[string]*StatDelta{}
	b.logs = nil
	return out, logs
}

func (b *statBuffer) requeue(deltas []StatDelta, logs []RequestLog) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logs = append(logs, b.logs...)
	for _, d := range deltas {
		k := statKey(d)
		if cur, ok := b.rows[k]; ok {
			cur.Requests += d.Requests
			cur.Errors += d.Errors
			cur.Retries += d.Retries
			cur.PromptTokens += d.PromptTokens
			cur.CompletionTokens += d.CompletionTokens
			cur.CachedTokens += d.CachedTokens
			cur.TotalTokens += d.TotalTokens
			cur.TTFTSumMs += d.TTFTSumMs
			cur.TTFTCount += d.TTFTCount
			continue
		}
		cp := d
		b.rows[k] = &cp
	}
}

// maxBufferedRows 触发提前落盘的聚合行数上限。
const maxBufferedRows = 100

// maxBufferedLogs 触发提前落盘的请求明细条数上限。
const maxBufferedLogs = 200

// AddStat 记录一次请求的统计增量；必要时立即落盘。
func (s *Store) AddStat(d StatDelta) {
	if d.Day == "" {
		d.Day = time.Now().Format(DayLayout)
	}
	if full := s.buf.add(d, maxBufferedRows); full {
		if err := s.flush(); err != nil {
			s.log.Warn("stats flush failed", "err", err)
		}
	}
}

// Flush 立即把缓冲写入数据库（供测试与关闭时使用）。
func (s *Store) Flush() error { return s.flush() }

func (s *Store) flush() error {
	deltas, logs := s.buf.drain()
	if len(deltas) == 0 && len(logs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		s.buf.requeue(deltas, logs)
		return fmt.Errorf("begin stats flush: %w", err)
	}
	now := unix(time.Now())
	const q = `INSERT INTO stats(day, module, key_id, model, requests, errors, retries,
			prompt_tokens, completion_tokens, cached_tokens, total_tokens,
			ttft_sum_ms, ttft_count, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, module, key_id, model) DO UPDATE SET
		  requests = requests + excluded.requests,
		  errors = errors + excluded.errors,
		  retries = retries + excluded.retries,
		  prompt_tokens = prompt_tokens + excluded.prompt_tokens,
		  completion_tokens = completion_tokens + excluded.completion_tokens,
		  cached_tokens = cached_tokens + excluded.cached_tokens,
		  total_tokens = total_tokens + excluded.total_tokens,
		  ttft_sum_ms = ttft_sum_ms + excluded.ttft_sum_ms,
		  ttft_count = ttft_count + excluded.ttft_count,
		  updated_at = excluded.updated_at`

	for _, d := range deltas {
		if _, err := tx.Exec(q, d.Day, d.Module, d.KeyID, d.Model, d.Requests, d.Errors, d.Retries,
			d.PromptTokens, d.CompletionTokens, d.CachedTokens, d.TotalTokens,
			d.TTFTSumMs, d.TTFTCount, now); err != nil {
			tx.Rollback()
			s.buf.requeue(deltas, logs)
			return fmt.Errorf("write stat row: %w", err)
		}
	}
	// 请求明细与聚合统计在同一事务中落盘，保证读写成本一次摊薄
	if len(logs) > 0 {
		stmt, err := tx.Prepare(`INSERT INTO request_log(ts, day, module, key_id, model, status,
				latency_ms, ttft_ms, prompt_tokens, completion_tokens, cached_tokens, total_tokens, error)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			tx.Rollback()
			s.buf.requeue(deltas, logs)
			return fmt.Errorf("prepare request log insert: %w", err)
		}
		for _, l := range logs {
			if l.Day == "" {
				l.Day = l.TS.Format(DayLayout)
			}
			if _, err := stmt.Exec(unix(l.TS), l.Day, l.Module, l.KeyID, l.Model, l.Status,
				l.LatencyMs, l.TTFTMs, l.PromptTokens, l.CompletionTokens, l.CachedTokens,
				l.TotalTokens, truncate(l.Error, 500)); err != nil {
				stmt.Close()
				tx.Rollback()
				s.buf.requeue(deltas, logs)
				return fmt.Errorf("write request log: %w", err)
			}
		}
		stmt.Close()
	}
	if err := tx.Commit(); err != nil {
		s.buf.requeue(deltas, logs)
		return fmt.Errorf("commit stats flush: %w", err)
	}
	return nil
}

// ---- 查询 -----------------------------------------------------------------

// SummaryFor 返回指定日与模块的汇总（module 为空则统计全部模块）。
func (s *Store) SummaryFor(day, module string) (StatSummary, error) {
	if err := s.flush(); err != nil {
		return StatSummary{}, err
	}
	q := `SELECT COALESCE(SUM(requests),0), COALESCE(SUM(errors),0), COALESCE(SUM(retries),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(cached_tokens),0), COALESCE(SUM(total_tokens),0),
			COALESCE(SUM(ttft_sum_ms),0), COALESCE(SUM(ttft_count),0)
		FROM stats WHERE day = ?`
	args := []any{day}
	if module != "" {
		q += ` AND module = ?`
		args = append(args, module)
	}
	var st StatSummary
	var ttftSum, ttftCount int64
	if err := s.db.QueryRow(q, args...).Scan(&st.Requests, &st.Errors, &st.Retries,
		&st.PromptTokens, &st.CompletionTokens, &st.CachedTokens, &st.TotalTokens,
		&ttftSum, &ttftCount); err != nil {
		return StatSummary{}, fmt.Errorf("summary: %w", err)
	}
	st.AvgTTFTMs = avg(ttftSum, ttftCount)
	st.TTFTCount = ttftCount
	st.CacheHitRate = rate(st.CachedTokens, st.PromptTokens)
	return st, nil
}

// BreakdownFor 返回指定日与模块下按 key×模型的明细（含 key 标签）。
func (s *Store) BreakdownFor(day, module string) ([]StatRow, error) {
	if err := s.flush(); err != nil {
		return nil, err
	}
	q := `SELECT st.day, st.module, st.key_id, COALESCE(k.label, ''), st.model,
			st.requests, st.errors, st.retries, st.prompt_tokens, st.completion_tokens,
			st.cached_tokens, st.total_tokens, st.ttft_sum_ms, st.ttft_count
		FROM stats st
		LEFT JOIN upstream_keys k ON k.id = st.key_id
		WHERE st.day = ?`
	args := []any{day}
	if module != "" {
		q += ` AND st.module = ?`
		args = append(args, module)
	}
	q += ` ORDER BY st.module ASC, st.requests DESC, st.model ASC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("breakdown: %w", err)
	}
	defer rows.Close()

	var out []StatRow
	for rows.Next() {
		var r StatRow
		var ttftSum, ttftCount int64
		if err := rows.Scan(&r.Day, &r.Module, &r.KeyID, &r.KeyLabel, &r.Model,
			&r.Requests, &r.Errors, &r.Retries, &r.PromptTokens, &r.CompletionTokens,
			&r.CachedTokens, &r.TotalTokens, &ttftSum, &ttftCount); err != nil {
			return nil, fmt.Errorf("scan stat row: %w", err)
		}
		r.AvgTTFTMs = avg(ttftSum, ttftCount)
		r.TTFTCount = ttftCount
		r.CacheHitRate = rate(r.CachedTokens, r.PromptTokens)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AvailableDays 返回有统计数据的日期（倒序，最多 limit 天）。
func (s *Store) AvailableDays(limit int) ([]string, error) {
	if err := s.flush(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 30
	}
	rows, err := s.db.Query(`SELECT DISTINCT day FROM stats ORDER BY day DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("available days: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("scan day: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func avg(sum, count int64) float64 {
	if count == 0 {
		return 0
	}
	return float64(sum) / float64(count)
}

func rate(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

// ---- 请求明细 -------------------------------------------------------------

// RequestLog 是一条请求明细记录。
type RequestLog struct {
	TS               time.Time
	Day              string
	Module           string
	KeyID            int64
	Model            string
	Status           int
	LatencyMs        int64
	TTFTMs           *int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	TotalTokens      int64
	Error            string
}

// AppendRequestLog 追加一条请求明细。
//
// 明细与聚合统计共用内存缓冲，由后台协程或满额触发批量事务落盘；
// 这样每个请求只做一次内存追加，避免在单写连接下逐条 INSERT 造成的串行等待。
func (s *Store) AppendRequestLog(l RequestLog) {
	if full := s.buf.addLog(l); full {
		if err := s.flush(); err != nil {
			s.log.Warn("request log flush failed", "err", err)
		}
	}
}

// RequestLogCount 返回某个日期的明细条数（供测试与排查使用）。
func (s *Store) RequestLogCount(day string) (int64, error) {
	if err := s.flush(); err != nil {
		return 0, err
	}
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_log WHERE day = ?`, day).Scan(&n); err != nil {
		return 0, fmt.Errorf("count request log: %w", err)
	}
	return n, nil
}

// PurgeOlderThan 删除早于 cutoff 的统计与请求明细，返回删除的统计行数。
// 过期数据按既定策略直接删除（不归档）。
func (s *Store) PurgeOlderThan(cutoff time.Time) (int64, error) {
	if err := s.flush(); err != nil {
		return 0, err
	}
	day := cutoff.Format(DayLayout)

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin purge: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM stats WHERE day < ?`, day)
	if err != nil {
		return 0, fmt.Errorf("purge stats: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM request_log WHERE day < ?`, day); err != nil {
		return 0, fmt.Errorf("purge request log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit purge: %w", err)
	}
	return n, nil
}
