// Package idmap 提供「非法客户端 ID → 网关生成合法 ID」的内存映射。
//
// 背景：上游只接受严格符合 opencode 格式的 x-opencode-session（实测见 internal/idgen）。
// 第三方客户端（例如 OpenClaw 用 randomUUID() 作为 sessionId）传的 ID 不合法，
// 直接透传会被判定为非官方客户端并返回 403 FreeTierError。
//
// 如果简单地"每个请求都重新生成一个合法 ID"，会导致上游的 session 每次都变，
// prompt 缓存前缀失效、缓存命中率下降。因此这里按「原始非法值」建立映射：
// 同一个客户端会话持续发送同一个 UUID 时，始终映射到同一个合法 ID，
// 从而在修复 403 的同时保住缓存。
//
// 映射只存内存（不落盘），条目在 TTL（默认 3600s）内未被访问即过期。
package idmap

import (
	"context"
	"sync"
	"time"
)

// DefaultTTL 是映射条目的存活时间：条目在此时间内未被访问即被清理。
const DefaultTTL = time.Hour

// DefaultSweepInterval 是后台清理的默认间隔。
const DefaultSweepInterval = time.Minute

// DefaultMaxEntries 是映射表容量上限。
//
// 防止异常客户端用大量不同 ID 撑爆内存（每条约百字节，10 万条约十几 MB）。
// 达到上限时淘汰最久未访问的条目（清理过期项后仍超限，则按访问时间淘汰）。
const DefaultMaxEntries = 100_000

// entry 是一个映射条目。
type entry struct {
	// value 是网关生成并交给上游的合法 ID。
	value string
	// lastSeen 用于 TTL 判定与容量淘汰。
	lastSeen time.Time
	// visited 标记自上次批量淘汰以来是否被访问过（近似 LRU 的第二次机会标记）。
	visited bool
}

// Map 是并发安全的 ID 映射表。
type Map struct {
	mu      sync.Mutex
	entries map[string]entry
	ttl     time.Duration
	// maxEntries 是容量上限（可在测试中调小）。
	maxEntries int

	// generate 生成新的合法 ID（可注入以便测试）。
	generate func() string
	// now 提供当前时间（可注入以便测试）。
	now func() time.Time
}

// New 创建映射表。generate 用于生成合法 ID，ttl <= 0 时使用 DefaultTTL。
func New(generate func() string, ttl time.Duration) *Map {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Map{
		entries:    make(map[string]entry),
		ttl:        ttl,
		maxEntries: DefaultMaxEntries,
		generate:   generate,
		now:        time.Now,
	}
}

// Resolve 返回 original 对应的合法 ID：
//   - 若已存在未过期的映射，复用并刷新其访问时间（保住上游缓存）
//   - 否则生成一个新的合法 ID 并建立映射
//
// original 为空时不建立映射，直接返回新生成的值（无键可映射）。
func (m *Map) Resolve(original string) string {
	if original == "" {
		return m.generate()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if e, ok := m.entries[original]; ok && now.Sub(e.lastSeen) < m.ttl {
		e.lastSeen = now
		e.visited = true
		m.entries[original] = e
		return e.value
	}

	// 顺带清理：写入是低频操作，借机清理过期项可避免周期性全量扫描的开销
	m.evictExpiredLocked(now)
	m.evictForCapacityLocked()

	value := m.generate()
	m.entries[original] = entry{value: value, lastSeen: now, visited: true}
	return value
}

// Len 返回当前条目数（供测试与观测）。
func (m *Map) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// Sweep 清理已过期条目，返回清理数量。
func (m *Map) Sweep() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.evictExpiredLocked(m.now())
}

// evictExpiredLocked 删除超过 TTL 未访问的条目。调用方需持锁。
func (m *Map) evictExpiredLocked(now time.Time) int {
	removed := 0
	for k, e := range m.entries {
		if now.Sub(e.lastSeen) >= m.ttl {
			delete(m.entries, k)
			removed++
		}
	}
	return removed
}

// evictForCapacityLocked 在超出容量时淘汰最久未访问的条目。调用方需持锁。
//
// 采用「第二次机会」（近似 LRU）：遍历时遇到的条目若自上次淘汰后被访问过，
// 则清除标记并放行；否则删除。最多两轮即可把表压回目标容量。
// 相比「每次写入都全表扫描找最旧项」，这避免了 O(n) 的反复扫描。
func (m *Map) evictForCapacityLocked() {
	if len(m.entries) < m.maxEntries {
		return
	}
	// 一次淘汰到容量的 9/10，避免随后每次写入都触发淘汰
	target := m.maxEntries * 9 / 10
	for round := 0; round < 2 && len(m.entries) > target; round++ {
		for k, e := range m.entries {
			if len(m.entries) <= target {
				return
			}
			if e.visited {
				e.visited = false
				m.entries[k] = e
				continue
			}
			delete(m.entries, k)
		}
	}
	// 极端情况下（所有条目都被访问过）直接删到达标
	for k := range m.entries {
		if len(m.entries) <= target {
			return
		}
		delete(m.entries, k)
	}
}

// StartSweeper 启动后台清理协程，定期删除过期条目。
// 调用方应在进程退出时 cancel context。
func (m *Map) StartSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.Sweep()
			}
		}
	}()
}
