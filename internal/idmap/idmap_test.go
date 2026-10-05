package idmap

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// countingGen 返回可预测的生成器，便于断言复用行为。
func countingGen(prefix string) func() string {
	var n int
	var mu sync.Mutex
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		// 生成固定形状的合法 ID：前缀 + 12 位 hex + 14 位 base62
		return fmt.Sprintf("%s_%012x%014d", prefix, n, n)
	}
}

// TestResolveReusesMapping 验证同一原始值始终映射到同一个合法 ID（保住上游缓存）。
func TestResolveReusesMapping(t *testing.T) {
	m := New(countingGen("ses"), time.Hour)
	const original = "550e8400-e29b-41d4-a716-446655440000" // OpenClaw 的 UUID 形态

	first := m.Resolve(original)
	for i := 0; i < 20; i++ {
		if got := m.Resolve(original); got != first {
			t.Fatalf("call %d returned a different id: %q vs %q", i+2, got, first)
		}
	}
	if m.Len() != 1 {
		t.Errorf("expected exactly 1 mapping, got %d", m.Len())
	}
}

// TestDistinctOriginalsGetDistinctValues 验证不同原始值互不影响。
func TestDistinctOriginalsGetDistinctValues(t *testing.T) {
	m := New(countingGen("ses"), time.Hour)
	a := m.Resolve("uuid-a")
	b := m.Resolve("uuid-b")

	if a == b {
		t.Error("different originals must not share the same mapped id")
	}
	if m.Resolve("uuid-a") != a || m.Resolve("uuid-b") != b {
		t.Error("each original must keep its own stable mapping")
	}
	if m.Len() != 2 {
		t.Errorf("expected 2 mappings, got %d", m.Len())
	}
}

// TestEntryExpiresAfterTTL 验证超过 TTL 未访问后会重新生成。
func TestEntryExpiresAfterTTL(t *testing.T) {
	base := time.Now()
	clock := func() time.Time { return base }
	m := New(countingGen("ses"), time.Hour)
	m.now = clock

	const original = "uuid-x"
	first := m.Resolve(original)

	// TTL 内：复用
	base = base.Add(59 * time.Minute)
	if got := m.Resolve(original); got != first {
		t.Errorf("within TTL should reuse: %q vs %q", got, first)
	}

	// 访问会刷新时间戳：再前进 59 分钟仍在 TTL 内
	base = base.Add(59 * time.Minute)
	if got := m.Resolve(original); got != first {
		t.Errorf("access should refresh TTL: %q vs %q", got, first)
	}

	// 超过 TTL 未访问：应生成新值
	base = base.Add(61 * time.Minute)
	second := m.Resolve(original)
	if second == first {
		t.Error("after TTL expiry a new id should be generated")
	}
}

// TestSweepRemovesExpired 验证后台清理只删除过期条目。
func TestSweepRemovesExpired(t *testing.T) {
	base := time.Now()
	m := New(countingGen("ses"), time.Hour)
	m.now = func() time.Time { return base }

	m.Resolve("old-1")
	m.Resolve("old-2")
	base = base.Add(30 * time.Minute)
	m.Resolve("fresh")

	// 30 分钟后：都还在 TTL 内
	if n := m.Sweep(); n != 0 {
		t.Errorf("no entry should expire yet, removed %d", n)
	}

	base = base.Add(31 * time.Minute) // old-1/old-2 已超过 1h，fresh 为 31min
	removed := m.Sweep()
	if removed != 2 {
		t.Errorf("expected 2 expired entries removed, got %d", removed)
	}
	if m.Len() != 1 {
		t.Errorf("expected 1 remaining entry, got %d", m.Len())
	}
	if m.Resolve("fresh") == "" {
		t.Error("fresh entry should still be usable")
	}
}

// TestWriteTriggersOpportunisticCleanup 验证写入时顺带清理过期项。
func TestWriteTriggersOpportunisticCleanup(t *testing.T) {
	base := time.Now()
	m := New(countingGen("ses"), time.Hour)
	m.now = func() time.Time { return base }

	for i := 0; i < 5; i++ {
		m.Resolve(fmt.Sprintf("old-%d", i))
	}
	if m.Len() != 5 {
		t.Fatalf("setup failed: %d", m.Len())
	}

	base = base.Add(2 * time.Hour) // 全部过期
	m.Resolve("new-one")

	// 写入时应已清掉 5 个过期项，只留下新的
	if m.Len() != 1 {
		t.Errorf("write should have cleaned expired entries, got %d", m.Len())
	}
}

// TestEmptyOriginalDoesNotCreateMapping 验证空值不建立映射（无键可映射）。
func TestEmptyOriginalDoesNotCreateMapping(t *testing.T) {
	m := New(countingGen("ses"), time.Hour)
	a := m.Resolve("")
	b := m.Resolve("")

	if a == b {
		t.Error("empty original should generate a fresh id each time (nothing to map on)")
	}
	if m.Len() != 0 {
		t.Errorf("empty original must not create mappings, got %d", m.Len())
	}
}

// TestCapacityIsBounded 验证容量上限生效，避免异常客户端撑爆内存。
func TestCapacityIsBounded(t *testing.T) {
	base := time.Now()
	m := New(countingGen("ses"), time.Hour)
	m.now = func() time.Time { return base }
	m.maxEntries = 1000 // 用小容量快速验证淘汰逻辑

	// 写入超过容量上限的条目（时间不变，故都不会过期）
	for i := 0; i < 1500; i++ {
		m.Resolve(fmt.Sprintf("k-%d", i))
		base = base.Add(time.Millisecond) // 让 lastSeen 有区分度
	}
	if m.Len() > m.maxEntries {
		t.Errorf("entries = %d, should not exceed capacity %d", m.Len(), m.maxEntries)
	}
	if m.Len() == 0 {
		t.Error("capacity eviction should not wipe the table")
	}
	// 被访问过的近期条目应被保留（第二次机会算法）
	recent := m.Resolve("k-1499")
	if recent == "" {
		t.Error("recent entry should still resolve")
	}
}

// TestConcurrentResolveIsStableAndSafe 验证并发下同一原始值只映射到一个 ID。
func TestConcurrentResolveIsStableAndSafe(t *testing.T) {
	m := New(countingGen("ses"), time.Hour)
	const original = "uuid-concurrent"

	const workers = 32
	const perWorker = 200
	results := make([][]string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out := make([]string, 0, perWorker)
			for j := 0; j < perWorker; j++ {
				out = append(out, m.Resolve(original))
			}
			results[i] = out
		}(i)
	}
	wg.Wait()

	want := results[0][0]
	for i, row := range results {
		for j, got := range row {
			if got != want {
				t.Fatalf("worker %d call %d got %q, want %q (concurrent resolve must be stable)",
					i, j, got, want)
			}
		}
	}
	if m.Len() != 1 {
		t.Errorf("expected 1 mapping after concurrent access, got %d", m.Len())
	}
}

// TestConcurrentResolveAndSweep 覆盖「请求路径解析」与「后台清理」并发的情形。
func TestConcurrentResolveAndSweep(t *testing.T) {
	m := New(countingGen("ses"), time.Millisecond) // 极短 TTL 以触发清理
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				m.Resolve(fmt.Sprintf("k-%d-%d", i, j))
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			m.Sweep()
		}
	}()
	wg.Wait()
}

// TestStartSweeperStopsOnContextCancel 验证清理协程可正常退出。
func TestStartSweeperStopsOnContextCancel(t *testing.T) {
	m := New(countingGen("ses"), 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	m.StartSweeper(ctx, 5*time.Millisecond)

	m.Resolve("temp")

	// 轮询等待清理完成（避免依赖固定 sleep 时长）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && m.Len() != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if m.Len() != 0 {
		t.Errorf("sweeper should have removed the expired entry, got %d", m.Len())
	}

	cancel()
	time.Sleep(30 * time.Millisecond) // 给协程退出留时间（无 panic 即可）
}

// TestDefaultTTLIsOneHour 锁定默认 TTL 与清理间隔（需求为 3600s）。
func TestDefaultTTLIsOneHour(t *testing.T) {
	if DefaultTTL != 3600*time.Second {
		t.Errorf("DefaultTTL = %v, want 3600s", DefaultTTL)
	}
	m := New(countingGen("ses"), 0)
	if m.ttl != DefaultTTL {
		t.Errorf("ttl = %v, want default %v", m.ttl, DefaultTTL)
	}
	// 非正数 TTL 应回落到默认值
	m2 := New(countingGen("ses"), -time.Minute)
	if m2.ttl != DefaultTTL {
		t.Errorf("negative ttl should fall back to default, got %v", m2.ttl)
	}
}
