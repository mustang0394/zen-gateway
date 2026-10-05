package store

import (
	"path/filepath"
	"testing"
	"time"
)

// 迁移升级路径测试。
//
// 背景（真实事故）：新增功能时若**复用**已被历史版本占用的版本号，
// 已升级过的数据库会因 `version <= current` 而跳过执行，导致新表缺失
// （曾出现 "no such table: inject_keywords"）。
// 因此这里覆盖三条路径，并把「版本号不得复用」固化为回归测试。

// TestFreshDatabaseHasAllTables 验证全新数据库会创建全部表。
func TestFreshDatabaseHasAllTables(t *testing.T) {
	s := testStore(t)
	for _, table := range []string{
		"upstream_keys", "versions", "cooldowns", "stats", "request_log",
		"settings", "inject_keywords", "prompt_overrides", "schema_migrations",
	} {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %s should exist after migrations", table)
		}
	}

	// 新表可用
	if _, err := s.CreateKeyword(Keyword{Module: "zen", Keyword: "Claude Code", Enabled: true}); err != nil {
		t.Fatalf("inject_keywords not usable: %v", err)
	}
	if err := s.SetPromptOverride("zen", "custom"); err != nil {
		t.Fatalf("prompt_overrides not usable: %v", err)
	}
}

// TestUpgradeFromSubstringRewriteVersion 复现真实事故场景：
// 数据库曾运行过「提示词子串替换」版本（v10/v11 = rewrite_rules），
// 升级到新版本后必须能创建 inject_keywords 与 prompt_overrides。
func TestUpgradeFromSubstringRewriteVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "from-rewrites.db")

	// ---- 1) 手工构造「已运行旧版」的库 ----
	old, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	key, err := old.CreateKey(APIKey{Module: "zen", Label: "既有Key", Note: "升级前", APIKey: "public", Enabled: true, IsAnonymous: true})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	day := time.Now().Format(DayLayout)
	old.AddStat(StatDelta{Day: day, Module: "zen", KeyID: key.ID, Model: "m", Requests: 7})
	if err := old.SaveVersion(Version{Name: "zen", Value: "1.2.3", FetchedAt: time.Now(), OK: true}); err != nil {
		t.Fatalf("save version: %v", err)
	}

	// 模拟旧版遗留：删掉新表，并保留 v1..v11 已应用（v10/v11 在旧版是 rewrite_rules）
	if _, err := old.db.Exec(`DROP TABLE IF EXISTS inject_keywords`); err != nil {
		t.Fatalf("drop inject_keywords: %v", err)
	}
	if _, err := old.db.Exec(`DROP TABLE IF EXISTS prompt_overrides`); err != nil {
		t.Fatalf("drop prompt_overrides: %v", err)
	}
	// 旧版曾创建过 rewrite_rules，这里也建出来以贴近真实库
	if _, err := old.db.Exec(`CREATE TABLE IF NOT EXISTS rewrite_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT, module TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
		match TEXT NOT NULL, replace TEXT NOT NULL, is_regex INTEGER NOT NULL DEFAULT 0,
		case_sensitive INTEGER NOT NULL DEFAULT 1, scope TEXT NOT NULL DEFAULT 'system_first_user',
		include_tools INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1,
		sort_order INTEGER NOT NULL DEFAULT 0, hits INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, deleted_at INTEGER)`); err != nil {
		t.Fatalf("create legacy rewrite_rules: %v", err)
	}
	// 关键：把版本回退到 v11（旧版的最后一条），模拟「已升级到旧版最新」
	if _, err := old.db.Exec(`DELETE FROM schema_migrations WHERE version > 11`); err != nil {
		t.Fatalf("rollback version: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// ---- 2) 用当前版本打开：应补建新表且旧数据无损 ----
	up, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen (upgrade): %v", err)
	}
	defer up.Close()

	// 这正是之前报错的调用
	if _, err := up.CreateKeyword(Keyword{Module: "zen", Keyword: "Claude Code", Enabled: true}); err != nil {
		t.Fatalf("inject_keywords missing after upgrade: %v", err)
	}
	if err := up.SetPromptOverride("zen", "custom prompt"); err != nil {
		t.Fatalf("prompt_overrides missing after upgrade: %v", err)
	}
	if _, err := up.ListKeywords("zen"); err != nil {
		t.Fatalf("ListKeywords after upgrade: %v", err)
	}

	// 旧数据必须完好
	keys, err := up.ListKeys("zen")
	if err != nil || len(keys) != 1 {
		t.Fatalf("existing keys lost: %+v err=%v", keys, err)
	}
	if keys[0].Note != "升级前" || keys[0].ID != key.ID {
		t.Errorf("key data changed: %+v", keys[0])
	}
	if err := up.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if sum, _ := up.SummaryFor(day, "zen"); sum.Requests != 7 {
		t.Errorf("stats lost: %+v", sum)
	}
	if v, _ := up.GetVersion("zen"); v.Value != "1.2.3" {
		t.Errorf("version lost: %+v", v)
	}
}

// TestUpgradeFromVersionWithNewTablesAlreadyCreated 覆盖中间状态：
// 曾运行过「有个 bug 的版本」（已提前创建 inject_keywords 但版本号记录混乱）。
// 由于新迁移使用 IF NOT EXISTS，重复建表不应报错。
func TestUpgradeFromVersionWithNewTablesAlreadyCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.db")

	st, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.CreateKeyword(Keyword{Module: "zen", Keyword: "keep me", Enabled: true}); err != nil {
		t.Fatalf("seed keyword: %v", err)
	}
	// 回退版本号，模拟「表已存在但迁移记录缺失」
	if _, err := st.db.Exec(`DELETE FROM schema_migrations WHERE version > 11`); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// 重新打开应能顺利补记录，且不因表已存在而失败
	up, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen with existing tables: %v", err)
	}
	defer up.Close()

	kws, err := up.ListKeywords("zen")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(kws) != 1 || kws[0].Keyword != "keep me" {
		t.Errorf("existing keyword should survive, got %+v", kws)
	}
}

// TestMigrationsAreIdempotentAcrossReopens 验证反复打开不会重复应用或报错。
func TestMigrationsAreIdempotentAcrossReopens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "repeat.db")

	for i := 0; i < 3; i++ {
		st, err := Open(path, nil)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		if _, err := st.CreateKeyword(Keyword{Module: "zen", Keyword: "kw", Enabled: true}); err == nil {
			// 第一次会插入，后续也允许插入（无唯一约束），仅确认不报错
		} else {
			t.Fatalf("open #%d: CreateKeyword failed: %v", i+1, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("close #%d: %v", i+1, err)
		}
	}

	st, err := Open(path, nil)
	if err != nil {
		t.Fatalf("final open: %v", err)
	}
	defer st.Close()
	var applied int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != len(migrations) {
		t.Errorf("applied migrations = %d, want %d", applied, len(migrations))
	}
}

// TestMigrationVersionsAreAppendOnly 把「版本号不得复用」固化为回归防护。
//
// 已发布的版本号一旦被占用就不能再用新语句替换——已升级的库会跳过它。
// 这里记录历史版本号的语义，任何改动都会被测试拦下。
func TestMigrationVersionsAreAppendOnly(t *testing.T) {
	// v1..v9 是初始版本，v10/v11 在历史版本中已被 rewrite_rules 占用（现为幂等占位），
	// v12 起才是新表。
	if len(migrations) < 14 {
		t.Fatalf("expected at least 14 migrations, got %d", len(migrations))
	}

	// v10/v11 必须是幂等占位（不得再承载建表逻辑）
	for _, v := range []int{10, 11} {
		stmt := migrations[v-1]
		if !isNoopMigration(stmt) {
			t.Errorf("v%d must stay a no-op placeholder (its version number is already taken by "+
				"the removed rewrite_rules feature); got: %.60s", v, stmt)
		}
	}

	// 新表必须出现在 v12 之后
	findTable := func(name string) int {
		for i, m := range migrations {
			if containsStr(m, "CREATE TABLE") && containsStr(m, name) {
				return i + 1
			}
		}
		return 0
	}
	if v := findTable("prompt_overrides"); v < 12 {
		t.Errorf("prompt_overrides created at v%d, must be appended after v11", v)
	}
	if v := findTable("inject_keywords"); v < 12 {
		t.Errorf("inject_keywords created at v%d, must be appended after v11", v)
	}
}

func isNoopMigration(s string) bool {
	t := trimSpace(s)
	return t == "SELECT 1" || t == "-- no-op"
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
