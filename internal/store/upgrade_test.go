package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestUpgradeFromOldSchema 模拟「旧版本已有数据库」的升级：
// 先用不含 rewrite_rules 的旧迁移集建库并写入数据，再用当前代码打开，
// 验证新 migration 生效且原有数据完好。
func TestUpgradeFromOldSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	// ---- 1) 手工构造「旧版本」数据库：只应用前 N-2 条迁移（不含 rewrite_rules）----
	old, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// 写入一些真实数据
	key, err := old.CreateKey(APIKey{Module: "zen", Label: "既有Key", Note: "升级前写入", APIKey: "public", Enabled: true, IsAnonymous: true})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	day := time.Now().Format(DayLayout)
	old.AddStat(StatDelta{Day: day, Module: "zen", KeyID: key.ID, Model: "m", Requests: 7, PromptTokens: 70})
	if err := old.SaveVersion(Version{Name: "zen", Value: "1.2.3", FetchedAt: time.Now(), OK: true}); err != nil {
		t.Fatalf("save version: %v", err)
	}
	if err := old.SetSetting(SettingAccessTokenZen, "tok-keep"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	// 模拟旧库：删掉 rewrite_rules 表并回退 schema 版本号到该表之前
	migrationsBeforeRewrite := len(migrations) - 2 // 建表 + 索引两条
	if _, err := old.db.Exec(`DROP TABLE IF EXISTS rewrite_rules`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := old.db.Exec(`DELETE FROM schema_migrations WHERE version > ?`, migrationsBeforeRewrite); err != nil {
		t.Fatalf("rollback version: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// ---- 2) 用当前代码打开：应补上 rewrite_rules 且保留旧数据 ----
	up, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen (upgrade): %v", err)
	}
	defer up.Close()

	// 新表可用
	if _, err := up.CreateRewriteRule(RewriteRule{
		Module: "zen", Name: "升级后新增", Match: "A", Replace: "B", Enabled: true,
	}); err != nil {
		t.Fatalf("rewrite_rules table not created by migration: %v", err)
	}

	// 旧数据完好
	keys, err := up.ListKeys("zen")
	if err != nil || len(keys) != 1 {
		t.Fatalf("existing keys lost: %+v err=%v", keys, err)
	}
	if keys[0].Note != "升级前写入" {
		t.Errorf("key note lost: %q", keys[0].Note)
	}
	if keys[0].ID != key.ID {
		t.Errorf("key id changed: %d -> %d", key.ID, keys[0].ID)
	}
	if err := up.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	sum, err := up.SummaryFor(day, "zen")
	if err != nil || sum.Requests != 7 {
		t.Errorf("stats lost: %+v err=%v", sum, err)
	}
	if v, _ := up.GetVersion("zen"); v.Value != "1.2.3" {
		t.Errorf("version lost: %+v", v)
	}
	if tok, _ := up.GetSetting(SettingAccessTokenZen, ""); tok != "tok-keep" {
		t.Errorf("setting lost: %q", tok)
	}

	// 再次重启应幂等（不重复建表报错）
	if err := up.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}
	again, err := Open(path, nil)
	if err != nil {
		t.Fatalf("second upgrade open: %v", err)
	}
	defer again.Close()
	rules, err := again.ListRewriteRules("")
	if err != nil {
		t.Fatalf("list rules after re-upgrade: %v", err)
	}
	if len(rules) != 1 {
		t.Errorf("expected the rule to survive, got %d", len(rules))
	}
}
