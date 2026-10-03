package cooldown

import (
	"path/filepath"
	"testing"
	"time"

	"zengateway/internal/store"
)

func newService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nil), st
}

func TestListIncludesKeyLabelAndRemaining(t *testing.T) {
	svc, st := newService(t)
	k, _ := st.CreateKey(store.APIKey{Module: "cline", Label: "主号", APIKey: "sk", Enabled: true})
	if err := svc.Add("cline", k.ID, "z-ai/glm-5.3-flash", time.Now().Add(22*time.Hour+59*time.Minute), "429"); err != nil {
		t.Fatalf("add: %v", err)
	}

	items, err := svc.List("cline")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 cooldown, got %d", len(items))
	}
	if items[0].KeyLabel != "主号" {
		t.Errorf("key label = %q, want 主号", items[0].KeyLabel)
	}
	if items[0].Remaining != "22h59m" {
		t.Errorf("remaining = %q, want 22h59m", items[0].Remaining)
	}

	// 模块过滤
	if items, _ := svc.List("zen"); len(items) != 0 {
		t.Errorf("zen should have no cooldowns, got %d", len(items))
	}
}

func TestAddRejectsInvalidInput(t *testing.T) {
	svc, _ := newService(t)
	if err := svc.Add("", 1, "m", time.Now().Add(time.Hour), ""); err == nil {
		t.Error("empty module must be rejected")
	}
	if err := svc.Add("zen", 0, "m", time.Now().Add(time.Hour), ""); err == nil {
		t.Error("zero key id must be rejected")
	}
	if err := svc.Add("zen", 1, "  ", time.Now().Add(time.Hour), ""); err == nil {
		t.Error("blank model must be rejected")
	}
	if err := svc.Add("zen", 1, "m", time.Now().Add(-time.Hour), ""); err == nil {
		t.Error("past time must be rejected")
	}
}

func TestReleaseAndSweep(t *testing.T) {
	svc, st := newService(t)
	k, _ := st.CreateKey(store.APIKey{Module: "zen", Label: "A", APIKey: "public", Enabled: true, IsAnonymous: true})

	// 手工解除
	svc.Add("zen", k.ID, "m1", time.Now().Add(time.Hour), "429")
	items, _ := svc.List("zen")
	if len(items) != 1 {
		t.Fatalf("expected 1, got %d", len(items))
	}
	if err := svc.Release(items[0].ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if items, _ := svc.List("zen"); len(items) != 0 {
		t.Errorf("cooldown should be released, got %d", len(items))
	}
	if err := svc.Release(9999); err != store.ErrNotFound {
		t.Errorf("releasing unknown id should be ErrNotFound, got %v", err)
	}

	// 过期清扫：写入一条已过期记录（绕过 Add 的时间校验）
	if err := st.SetCooldown(store.Cooldown{
		Module: "zen", KeyID: k.ID, Model: "m2",
		Until: time.Now().Add(-time.Minute), Reason: "429", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	n, err := svc.Sweep()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d, want 1", n)
	}
}

func TestPurgeExpiredDataRespectsRetention(t *testing.T) {
	svc, st := newService(t)
	old := time.Now().AddDate(0, 0, -45).Format(store.DayLayout)
	recent := time.Now().Format(store.DayLayout)

	st.AddStat(store.StatDelta{Day: old, Module: "zen", KeyID: 1, Model: "m", Requests: 9})
	st.AddStat(store.StatDelta{Day: recent, Module: "zen", KeyID: 1, Model: "m", Requests: 2})

	n, err := svc.PurgeExpiredData(30)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d rows, want 1", n)
	}
	got, _ := st.SummaryFor(recent, "zen")
	if got.Requests != 2 {
		t.Errorf("recent data must be kept, got %d", got.Requests)
	}

	// 非法保留天数回落到 30 天
	if _, err := svc.PurgeExpiredData(0); err != nil {
		t.Errorf("retention 0 should fall back, got %v", err)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                             "0m",
		-time.Minute:                  "0m",
		5 * time.Minute:               "5m",
		2*time.Hour + 30*time.Minute:  "2h30m",
		22*time.Hour + 59*time.Minute: "22h59m",
		3*24*time.Hour + 4*time.Hour:  "3d4h",
	}
	for in, want := range cases {
		if got := HumanDuration(in); got != want {
			t.Errorf("HumanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}
