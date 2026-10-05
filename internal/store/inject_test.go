package store

import "testing"

func TestKeywordCRUD(t *testing.T) {
	s := testStore(t)

	k, err := s.CreateKeyword(Keyword{
		Module: "zen", Keyword: "Claude Code", Note: "第三方客户端特征", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k.ID == 0 || k.SortOrder != 1 || k.Hits != 0 {
		t.Fatalf("unexpected created keyword: %+v", k)
	}

	k.Keyword = "Cursor"
	k.Note = ""
	k.Enabled = false
	got, err := s.UpdateKeyword(k)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Keyword != "Cursor" || got.Note != "" || got.Enabled {
		t.Fatalf("update not applied: %+v", got)
	}

	if err := s.DeleteKeyword(k.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetKeyword(k.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
	if err := s.DeleteKeyword(k.ID); err != ErrNotFound {
		t.Errorf("second delete should be ErrNotFound, got %v", err)
	}
}

// TestActiveKeywordsModuleScope 验证模块过滤：含全局规则、排除停用与其他模块。
func TestActiveKeywordsModuleScope(t *testing.T) {
	s := testStore(t)
	mk := func(module, kw string, enabled bool) {
		t.Helper()
		if _, err := s.CreateKeyword(Keyword{Module: module, Keyword: kw, Enabled: enabled}); err != nil {
			t.Fatalf("create %s: %v", kw, err)
		}
	}
	mk("zen", "zen-only", true)
	mk("cline", "cline-only", true)
	mk(ModuleAll, "global", true)
	mk("zen", "zen-disabled", false)

	zen, err := s.ActiveInjectKeywords("zen")
	if err != nil {
		t.Fatalf("active zen: %v", err)
	}
	names := map[string]bool{}
	for _, k := range zen {
		names[k.Keyword] = true
	}
	if !names["zen-only"] || !names["global"] {
		t.Errorf("zen should see zen-only + global, got %v", names)
	}
	if names["cline-only"] || names["zen-disabled"] {
		t.Errorf("zen should not see cline/disabled rules, got %v", names)
	}

	// 管理端列表口径：模块范围与 ActiveInjectKeywords 一致（含全局），
	// 但会额外包含停用项（用户需要看到并可重新启用它们）
	listed, _ := s.ListKeywords("zen")
	if len(listed) != len(zen)+1 {
		t.Errorf("ListKeywords should include the disabled one: got %d, active %d",
			len(listed), len(zen))
	}
	disabledSeen := false
	for _, k := range listed {
		if k.Keyword == "zen-disabled" && !k.Enabled {
			disabledSeen = true
		}
		if k.Keyword == "cline-only" {
			t.Error("ListKeywords(zen) must not include other modules' rules")
		}
	}
	if !disabledSeen {
		t.Error("ListKeywords(zen) should include disabled rules for management")
	}

	all, _ := s.ListKeywords("")
	if len(all) != 4 {
		t.Errorf("unfiltered should return all 4, got %d", len(all))
	}
}

func TestKeywordReorderAndHits(t *testing.T) {
	s := testStore(t)
	a, _ := s.CreateKeyword(Keyword{Module: "zen", Keyword: "A", Enabled: true})
	b, _ := s.CreateKeyword(Keyword{Module: "zen", Keyword: "B", Enabled: true})

	if err := s.ReorderKeywords([]int64{b.ID, a.ID}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	list, _ := s.ActiveInjectKeywords("zen")
	if list[0].ID != b.ID || list[1].ID != a.ID {
		t.Fatalf("reorder not applied: %v", []int64{list[0].ID, list[1].ID})
	}

	if err := s.AddInjectHits(map[int64]int64{a.ID: 3, b.ID: 2}); err != nil {
		t.Fatalf("add hits: %v", err)
	}
	if err := s.AddInjectHits(map[int64]int64{a.ID: 1}); err != nil {
		t.Fatalf("add hits again: %v", err)
	}
	got, _ := s.GetKeyword(a.ID)
	if got.Hits != 4 {
		t.Errorf("hits = %d, want 4", got.Hits)
	}
	if err := s.ResetKeywordHits(a.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	got, _ = s.GetKeyword(a.ID)
	if got.Hits != 0 {
		t.Errorf("hits after reset = %d, want 0", got.Hits)
	}
	if err := s.AddInjectHits(nil); err != nil {
		t.Errorf("nil hits should be a no-op: %v", err)
	}
}

func TestPromptOverrideLifecycle(t *testing.T) {
	s := testStore(t)

	// 未设置时返回空串（调用方回落到内置提示词）
	txt, err := s.PromptOverride("zen")
	if err != nil || txt != "" {
		t.Fatalf("initial override = %q err=%v", txt, err)
	}

	if err := s.SetPromptOverride("zen", "custom zen prompt"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if txt, _ := s.PromptOverride("zen"); txt != "custom zen prompt" {
		t.Errorf("override = %q", txt)
	}

	// 覆盖写入
	if err := s.SetPromptOverride("zen", "updated"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if txt, _ := s.PromptOverride("zen"); txt != "updated" {
		t.Errorf("override not overwritten: %q", txt)
	}

	// 空串删除覆盖
	if err := s.SetPromptOverride("zen", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if txt, _ := s.PromptOverride("zen"); txt != "" {
		t.Errorf("override should be cleared, got %q", txt)
	}

	// 空模块名应被拒绝
	if err := s.SetPromptOverride("", "x"); err == nil {
		t.Error("empty module should be rejected")
	}

	// 批量读取
	s.SetPromptOverride("zen", "z")
	s.SetPromptOverride("cline", "c")
	all, err := s.ListPromptOverrides()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 || all["zen"] != "z" || all["cline"] != "c" {
		t.Errorf("list overrides = %v", all)
	}
}
