package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// testStore 创建临时目录下的 Store，并在测试结束自动关闭。
func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrationsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.db")

	s1, err := Open(path, nil)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := s1.CreateKey(APIKey{Module: "zen", APIKey: "public", Enabled: true}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重新打开：迁移必须幂等且数据保留
	s2, err := Open(path, nil)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	keys, err := s2.ListKeys("zen")
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 || keys[0].APIKey != "public" {
		t.Fatalf("data lost after reopen: %+v", keys)
	}
}

func TestKeyCRUDAndNote(t *testing.T) {
	s := testStore(t)

	k, err := s.CreateKey(APIKey{
		Module: "cline", Label: "主号", Note: "2026-10-01 注册，日本出口",
		APIKey: "sk-abc", Proxy: "socks5h://u:p@h:1080", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k.ID == 0 || k.SortOrder != 1 {
		t.Fatalf("unexpected created key: %+v", k)
	}
	if k.Note != "2026-10-01 注册，日本出口" {
		t.Fatalf("note not persisted: %q", k.Note)
	}

	// 备注可更新为空
	k.Note = ""
	k.Label = "备用"
	k.Enabled = false
	got, err := s.UpdateKey(k)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Note != "" || got.Label != "备用" || got.Enabled {
		t.Fatalf("update not applied: %+v", got)
	}

	// 删除后不可见，但记录保留（软删除）
	if err := s.DeleteKey(k.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetKey(k.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := s.DeleteKey(k.ID); err != ErrNotFound {
		t.Fatalf("second delete should be ErrNotFound, got %v", err)
	}
}

func TestPickKeyOrderAndCooldownScope(t *testing.T) {
	s := testStore(t)
	now := time.Now()

	a, _ := s.CreateKey(APIKey{Module: "zen", Label: "A", APIKey: "ka", Enabled: true})
	b, _ := s.CreateKey(APIKey{Module: "zen", Label: "B", APIKey: "kb", Enabled: true})

	// 顺序优先：始终选 sort_order 最小的 A
	picked, err := s.PickKey("zen", "model-1", now)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if picked.ID != a.ID {
		t.Fatalf("expected first key A, got %+v", picked)
	}

	// A 在 model-1 冷却后，应切到 B
	if err := s.SetCooldown(Cooldown{
		Module: "zen", KeyID: a.ID, Model: "model-1",
		Until: now.Add(time.Hour), Reason: "429", CreatedAt: now,
	}); err != nil {
		t.Fatalf("set cooldown: %v", err)
	}
	picked, err = s.PickKey("zen", "model-1", now)
	if err != nil {
		t.Fatalf("pick after cooldown: %v", err)
	}
	if picked.ID != b.ID {
		t.Fatalf("expected fallback to B, got %+v", picked)
	}

	// 冷却维度含 model：A 对 model-2 仍可用
	picked, err = s.PickKey("zen", "model-2", now)
	if err != nil {
		t.Fatalf("pick other model: %v", err)
	}
	if picked.ID != a.ID {
		t.Fatalf("cooldown must be per-model, got %+v", picked)
	}

	// 全部冷却：无可用 key
	if err := s.SetCooldown(Cooldown{
		Module: "zen", KeyID: b.ID, Model: "model-1",
		Until: now.Add(time.Hour), Reason: "429", CreatedAt: now,
	}); err != nil {
		t.Fatalf("set cooldown b: %v", err)
	}
	if _, err := s.PickKey("zen", "model-1", now); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound when all cooling, got %v", err)
	}

	// 到期后重新可用
	if _, err := s.PickKey("zen", "model-1", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("expected key available after cooldown expiry: %v", err)
	}
}

func TestCooldownUpsertAndPurge(t *testing.T) {
	s := testStore(t)
	now := time.Now()

	for i := 0; i < 3; i++ {
		if err := s.SetCooldown(Cooldown{
			Module: "cline", KeyID: 7, Model: "z-ai/glm-5.3-flash",
			Until: now.Add(time.Duration(i+1) * time.Hour), Reason: "429",
			Detail: "Try again in 22h 59m", CreatedAt: now,
		}); err != nil {
			t.Fatalf("set cooldown: %v", err)
		}
	}
	list, err := s.ListCooldowns("cline", now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("cooldown upsert should keep 1 row, got %d", len(list))
	}
	if list[0].Until.Sub(now) < 2*time.Hour {
		t.Fatalf("last write should win, got %v", list[0].Until)
	}

	n, err := s.PurgeExpiredCooldowns(now.Add(10 * time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 purged cooldown, got %d", n)
	}
}

func TestReorderKeys(t *testing.T) {
	s := testStore(t)
	a, _ := s.CreateKey(APIKey{Module: "zen", Label: "A", APIKey: "a", Enabled: true})
	b, _ := s.CreateKey(APIKey{Module: "zen", Label: "B", APIKey: "b", Enabled: true})

	if err := s.ReorderKeys("zen", []int64{b.ID, a.ID}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	keys, err := s.ListKeys("zen")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 2 || keys[0].ID != b.ID || keys[1].ID != a.ID {
		t.Fatalf("reorder not applied: %+v", keys)
	}
}

func TestStatsBufferAccumulateAndFlush(t *testing.T) {
	s := testStore(t)
	day := "2026-10-03"

	for i := 0; i < 5; i++ {
		s.AddStat(StatDelta{
			Day: day, Module: "zen", KeyID: 1, Model: "mimo-v2.5-free",
			Requests: 1, PromptTokens: 100, CompletionTokens: 50,
			CachedTokens: 80, TotalTokens: 150, TTFTSumMs: 400, TTFTCount: 1,
		})
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got, err := s.SummaryFor(day, "zen")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if got.Requests != 5 || got.PromptTokens != 500 || got.CompletionTokens != 250 {
		t.Fatalf("unexpected summary: %+v", got)
	}
	if got.CachedTokens != 400 || got.TotalTokens != 750 {
		t.Fatalf("unexpected token totals: %+v", got)
	}
	if got.AvgTTFTMs != 400 {
		t.Fatalf("avg ttft = %v, want 400", got.AvgTTFTMs)
	}
	if got.CacheHitRate != 80 {
		t.Fatalf("cache hit rate = %v, want 80", got.CacheHitRate)
	}

	// 缓冲落盘后再累加，必须是增量而非覆盖
	s.AddStat(StatDelta{Day: day, Module: "zen", KeyID: 1, Model: "mimo-v2.5-free", Requests: 1})
	if err := s.Flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	got, _ = s.SummaryFor(day, "zen")
	if got.Requests != 6 {
		t.Fatalf("upsert must accumulate, got requests=%d", got.Requests)
	}
}

func TestBreakdownIncludesKeyLabelAndPerModule(t *testing.T) {
	s := testStore(t)
	k, _ := s.CreateKey(APIKey{Module: "cline", Label: "主号", APIKey: "sk", Enabled: true})
	day := "2026-10-03"

	s.AddStat(StatDelta{Day: day, Module: "cline", KeyID: k.ID, Model: "m1", Requests: 3, PromptTokens: 30, CachedTokens: 15})
	s.AddStat(StatDelta{Day: day, Module: "zen", KeyID: 99, Model: "m2", Requests: 7})
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	rows, err := s.BreakdownFor(day, "cline")
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("module filter failed: %+v", rows)
	}
	if rows[0].KeyLabel != "主号" {
		t.Fatalf("key label join failed: %+v", rows[0])
	}
	if rows[0].CacheHitRate != 50 {
		t.Fatalf("cache hit rate = %v, want 50", rows[0].CacheHitRate)
	}

	all, err := s.BreakdownFor(day, "")
	if err != nil {
		t.Fatalf("breakdown all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 rows across modules, got %d", len(all))
	}
}

func TestPurgeOlderThanRetention(t *testing.T) {
	s := testStore(t)
	old := time.Now().AddDate(0, 0, -40).Format(DayLayout)
	recent := time.Now().Format(DayLayout)

	s.AddStat(StatDelta{Day: old, Module: "zen", KeyID: 1, Model: "m", Requests: 5})
	s.AddStat(StatDelta{Day: recent, Module: "zen", KeyID: 1, Model: "m", Requests: 2})
	s.AppendRequestLog(RequestLog{TS: time.Now().AddDate(0, 0, -40), Module: "zen", KeyID: 1, Model: "m", Status: 200})
	// 明细走内存缓冲，PurgeOlderThan 内部会先 flush，因此这里无需显式落盘

	n, err := s.PurgeOlderThan(time.Now().AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 purged stat row, got %d", n)
	}
	if _, err := s.SummaryFor(old, "zen"); err != nil {
		t.Fatalf("summary old: %v", err)
	}
	got, _ := s.SummaryFor(old, "zen")
	if got.Requests != 0 {
		t.Fatalf("old stats should be removed, got %d", got.Requests)
	}
	days, err := s.AvailableDays(30)
	if err != nil {
		t.Fatalf("available days: %v", err)
	}
	if len(days) != 1 || days[0] != recent {
		t.Fatalf("unexpected remaining days: %+v", days)
	}
}

func TestSettingsAndVersions(t *testing.T) {
	s := testStore(t)

	if v, err := s.GetSetting(SettingAccessTokenZen, "def"); err != nil || v != "def" {
		t.Fatalf("default setting = %q, err=%v", v, err)
	}
	if err := s.SetSetting(SettingAccessTokenZen, "tok-1"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	if err := s.SetSetting(SettingAccessTokenZen, "tok-2"); err != nil {
		t.Fatalf("overwrite setting: %v", err)
	}
	if v, _ := s.GetSetting(SettingAccessTokenZen, ""); v != "tok-2" {
		t.Fatalf("setting not overwritten: %q", v)
	}
	if err := s.SetSetting(SettingRetentionDays, "30"); err != nil {
		t.Fatalf("set retention: %v", err)
	}
	if n, err := s.GetIntSetting(SettingRetentionDays, 7); err != nil || n != 30 {
		t.Fatalf("int setting = %d, err=%v", n, err)
	}
	if n, err := s.GetIntSetting("missing.key", 9); err != nil || n != 9 {
		t.Fatalf("missing int setting should default, got %d err=%v", n, err)
	}

	now := time.Now()
	if err := s.SaveVersion(Version{Name: "cline.cli", Value: "4.1.22", FetchedAt: now, OK: true}); err != nil {
		t.Fatalf("save version: %v", err)
	}
	if err := s.SaveVersion(Version{Name: "cline.sdk", Value: "0.0.90", FetchedAt: now, OK: false, Error: "timeout"}); err != nil {
		t.Fatalf("save version: %v", err)
	}
	v, err := s.GetVersion("cline.cli")
	if err != nil || v.Value != "4.1.22" || !v.OK {
		t.Fatalf("version = %+v, err=%v", v, err)
	}
	failed, _ := s.GetVersion("cline.sdk")
	if failed.OK || failed.Error != "timeout" {
		t.Fatalf("failed version should keep error: %+v", failed)
	}
	list, err := s.ListVersions()
	if err != nil || len(list) != 2 {
		t.Fatalf("list versions = %+v, err=%v", list, err)
	}
	if _, err := s.GetVersion("nope"); err != ErrNotFound {
		t.Fatalf("missing version should be ErrNotFound, got %v", err)
	}
}

func TestConcurrentWritesNoBusy(t *testing.T) {
	s := testStore(t)
	day := "2026-10-03"

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := s.CreateKey(APIKey{Module: "zen", APIKey: "k", Enabled: true}); err != nil {
					errs <- err
					return
				}
				s.AddStat(StatDelta{Day: day, Module: "zen", KeyID: int64(i), Model: "m", Requests: 1})
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write error: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got, _ := s.SummaryFor(day, "zen")
	if got.Requests != 320 {
		t.Fatalf("expected 320 requests, got %d", got.Requests)
	}
}

func TestOpenCreatesMissingDataDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "gw.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open nested: %v", err)
	}
	defer s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file not created: %v", err)
	}
}

// TestConcurrentPickKeyAndCooldown 覆盖并发选 key 与冷却写入的组合场景。
func TestConcurrentPickKeyAndCooldown(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	for i := 0; i < 4; i++ {
		if _, err := s.CreateKey(APIKey{Module: "cline", APIKey: "k", Enabled: true}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 128)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				model := "m" + string(rune('a'+i%3))
				k, err := s.PickKey("cline", model, now)
				if err == ErrNotFound {
					continue
				}
				if err != nil {
					errs <- err
					return
				}
				if err := s.SetCooldown(Cooldown{
					Module: "cline", KeyID: k.ID, Model: model,
					Until: now.Add(time.Minute), Reason: "429", CreatedAt: now,
				}); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent pick/cooldown error: %v", err)
	}
}

// TestBackgroundFlushLoop 验证统计缓冲会由后台协程定时落盘
// （此前 SetFlushInterval 可被误认为能在运行期调整间隔，实际无效且存在数据竞争，
// 现已移除该 API，此处改为直接验证后台落盘行为本身）。
func TestBackgroundFlushLoop(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "flush.db"), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	day := time.Now().Format(DayLayout)
	st.AddStat(StatDelta{Day: day, Module: "zen", KeyID: 1, Model: "m", Requests: 1})

	// 不显式 Flush，等待后台 ticker（默认 5s）触发
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		// SummaryFor 内部会先 flush，因此改用直接读表避免掩盖后台行为
		var n int64
		if err := st.db.QueryRow(`SELECT COALESCE(SUM(requests),0) FROM stats WHERE day = ?`, day).Scan(&n); err != nil {
			t.Fatalf("query: %v", err)
		}
		if n == 1 {
			return // 后台已完成落盘
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("background flush did not persist stats within 8s")
}

// TestCloseFlushesPendingStats 验证 Close 会落盘尚未刷写的统计，避免退出丢数据。
func TestCloseFlushesPendingStats(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "close.db")

	st, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	day := time.Now().Format(DayLayout)
	for i := 0; i < 7; i++ {
		st.AddStat(StatDelta{Day: day, Module: "cline", KeyID: 5, Model: "m", Requests: 1, PromptTokens: 10})
	}
	// 立即关闭：7 条仍在缓冲中，Close 必须先落盘
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重新打开并核对数据已持久化
	st2, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	sum, err := st2.SummaryFor(day, "cline")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.Requests != 7 {
		t.Errorf("requests after close = %d, want 7 (Close must flush pending stats)", sum.Requests)
	}
	if sum.PromptTokens != 70 {
		t.Errorf("prompt tokens after close = %d, want 70", sum.PromptTokens)
	}
}

// TestIsCoolingDownForAnonymousEntry 覆盖匿名回退条目（key_id = 0）的冷却查询。
// 该条目不存在于 upstream_keys 表，无法被 PickKey 的 NOT EXISTS 子句过滤，
// 需要 IsCoolingDown 单独判定，否则 429 后会绕过冷却。
func TestIsCoolingDownForAnonymousEntry(t *testing.T) {
	s := testStore(t)
	now := time.Now()

	// 未冷却
	cooling, err := s.IsCoolingDown("zen", 0, "mimo-v2.5-free", now)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if cooling {
		t.Error("anonymous entry should not be cooling initially")
	}

	// 写入冷却
	if err := s.SetCooldown(Cooldown{
		Module: "zen", KeyID: 0, Model: "mimo-v2.5-free",
		Until: now.Add(time.Hour), Reason: "429", CreatedAt: now,
	}); err != nil {
		t.Fatalf("set cooldown: %v", err)
	}

	cooling, err = s.IsCoolingDown("zen", 0, "mimo-v2.5-free", now)
	if err != nil {
		t.Fatalf("check after set: %v", err)
	}
	if !cooling {
		t.Error("anonymous entry should be cooling after 429")
	}

	// 维度含 model：其他模型不受影响
	if cooling, _ := s.IsCoolingDown("zen", 0, "other-model", now); cooling {
		t.Error("cooldown must be per-model")
	}
	// 其他模块不受影响
	if cooling, _ := s.IsCoolingDown("cline", 0, "mimo-v2.5-free", now); cooling {
		t.Error("cooldown must be per-module")
	}
	// 到期后恢复
	if cooling, _ := s.IsCoolingDown("zen", 0, "mimo-v2.5-free", now.Add(2*time.Hour)); cooling {
		t.Error("cooldown should expire")
	}
}

// TestRequestLogBufferedAndFlushed 验证请求明细走内存缓冲后能正确落库，
// 且与聚合统计在同一事务中写入（避免单写连接下逐条 INSERT 的串行等待）。
func TestRequestLogBufferedAndFlushed(t *testing.T) {
	s := testStore(t)
	day := time.Now().Format(DayLayout)

	for i := 0; i < 5; i++ {
		s.AddStat(StatDelta{Day: day, Module: "cline", KeyID: 3, Model: "m", Requests: 1})
		s.AppendRequestLog(RequestLog{
			TS: time.Now(), Module: "cline", KeyID: 3, Model: "m", Status: 200,
			LatencyMs: 120, PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		})
	}

	n, err := s.RequestLogCount(day) // 内部会先 flush
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 5 {
		t.Fatalf("request_log rows = %d, want 5", n)
	}

	// 聚合统计同样落盘
	sum, _ := s.SummaryFor(day, "cline")
	if sum.Requests != 5 {
		t.Errorf("stats requests = %d, want 5", sum.Requests)
	}

	// 明细中保留 token 与耗时字段
	var latency int64
	var pt int64
	if err := s.db.QueryRow(`SELECT latency_ms, prompt_tokens FROM request_log
		WHERE day = ? LIMIT 1`, day).Scan(&latency, &pt); err != nil {
		t.Fatalf("query detail: %v", err)
	}
	if latency != 120 || pt != 10 {
		t.Errorf("detail row latency=%d prompt=%d, want 120 / 10", latency, pt)
	}
}

// TestRequestLogFlushOnCapacity 验证明细达到缓冲上限时自动落盘。
func TestRequestLogFlushOnCapacity(t *testing.T) {
	s := testStore(t)
	day := time.Now().Format(DayLayout)

	for i := 0; i < maxBufferedLogs+10; i++ {
		s.AppendRequestLog(RequestLog{
			TS: time.Now(), Module: "zen", KeyID: 1, Model: "m", Status: 200,
		})
	}
	n, err := s.RequestLogCount(day)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != int64(maxBufferedLogs+10) {
		t.Errorf("request_log rows = %d, want %d", n, maxBufferedLogs+10)
	}
}
