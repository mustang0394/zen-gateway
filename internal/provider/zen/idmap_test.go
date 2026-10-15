package zen

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"zengateway/internal/idgen"
	"zengateway/internal/provider"
	"zengateway/internal/store"
)

func newProvider(t *testing.T) *Provider {
	t.Helper()
	return New("", "", nil)
}

func buildHeaders(t *testing.T, down http.Header) http.Header {
	t.Helper()
	p := newProvider(t)
	return p.BuildHeaders(down, store.APIKey{APIKey: "public", IsAnonymous: true},
		provider.Versions{Zen: "2.0.29"})
}

// TestValidDownstreamIDsArePreserved 验证下游的合法 ID 被原样保留
// （保住客户端自己的 session 语义与上游 prompt 缓存）。
func TestValidDownstreamIDsArePreserved(t *testing.T) {
	ses := "ses_ef5920720ffeDse8BpbCnsgiMT"

	for _, header := range []string{"x-opencode-session", "x-opencode-session-id"} {
		t.Run(header, func(t *testing.T) {
			down := http.Header{}
			down.Set(header, ses)
			h := buildHeaders(t, down)

			if got := h.Get("x-opencode-session"); got != ses {
				t.Errorf("valid session must be preserved, got %q", got)
			}
			if got := h.Get("x-opencode-session-id"); got != ses {
				t.Errorf("session-id must mirror the session, got %q", got)
			}
		})
	}
}

// TestMissingIDsAreGenerated 验证下游未传时生成合法 ID，且四个会话头同值。
func TestMissingIDsAreGenerated(t *testing.T) {
	h := buildHeaders(t, http.Header{})

	ses := h.Get("x-opencode-session")
	if !idgen.IsSession(ses) {
		t.Fatalf("generated session is not valid: %q", ses)
	}
	for _, header := range []string{"x-opencode-session-id", "x-session-affinity", "X-Session-Id"} {
		if got := h.Get(header); got != ses {
			t.Errorf("%s = %q, want the same session %q", header, got, ses)
		}
	}
}

// TestRequestHeaderIsNeverSent 锁定已废弃的 x-opencode-request：
// 2026-10-14 起携带该头一律 403，因此不论下游传什么都不能出现在上游请求里。
func TestRequestHeaderIsNeverSent(t *testing.T) {
	down := http.Header{}
	down.Set("x-opencode-request", "msg_ef5920720ffc83HCMpIIJDrDdM")
	h := buildHeaders(t, down)

	if got := h.Get("x-opencode-request"); got != "" {
		t.Errorf("x-opencode-request must never be forwarded, got %q", got)
	}
}

// TestInvalidSessionIsMappedToValidID 是本功能的核心用例：
// OpenClaw 用 UUID 作为 sessionId，实测会让上游返回 403；
// 网关必须把它替换为合法 ID。
func TestInvalidSessionIsMappedToValidID(t *testing.T) {
	uuid := "550e8400-e29b-41d4-a716-446655440000" // OpenClaw turnId 的形态
	down := http.Header{}
	down.Set("x-opencode-session", uuid)

	h := buildHeaders(t, down)
	got := h.Get("x-opencode-session")

	if got == uuid {
		t.Fatal("invalid UUID must not be forwarded to upstream")
	}
	if !idgen.IsSession(got) {
		t.Errorf("mapped session must be a valid ses_ id, got %q", got)
	}
}

// TestInvalidSessionIDHeaderIsMapped 验证非法 ID 出现在 x-opencode-session-id
// （2.x 客户端的头名）时同样会被映射。
func TestInvalidSessionIDHeaderIsMapped(t *testing.T) {
	uuid := "550e8400-e29b-41d4-a716-446655440000"
	down := http.Header{}
	down.Set("x-opencode-session-id", uuid)

	h := buildHeaders(t, down)
	got := h.Get("x-opencode-session-id")

	if got == uuid || !idgen.IsSession(got) {
		t.Errorf("invalid session-id must be mapped to a valid id, got %q", got)
	}
	if h.Get("x-opencode-session") != got {
		t.Error("both session headers must carry the same id")
	}
}

// TestInvalidSessionMappingIsStable 验证同一非法值稳定映射到同一个合法 ID
// —— 这样上游 session 不会每请求都变，缓存得以保留。
func TestInvalidSessionMappingIsStable(t *testing.T) {
	p := newProvider(t)
	uuid := "550e8400-e29b-41d4-a716-446655440000"

	var first string
	for i := 0; i < 10; i++ {
		down := http.Header{}
		down.Set("x-opencode-session", uuid)
		got := p.BuildHeaders(down, store.APIKey{APIKey: "public"},
			provider.Versions{Zen: "2.0.29"}).Get("x-opencode-session")

		if !idgen.IsSession(got) {
			t.Fatalf("iteration %d: invalid mapped id %q", i, got)
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("iteration %d: mapping changed (%q → %q); upstream cache would be lost",
				i, first, got)
		}
	}
}

// TestDifferentInvalidValuesGetDifferentMappings 验证不同客户端互不影响
// （若都映射到同一个 session，上游会串会话）。
func TestDifferentInvalidValuesGetDifferentMappings(t *testing.T) {
	p := newProvider(t)
	ids := []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"claude-code-session-abc123",
		"ses_abc", // 前缀对但长度非法
	}
	seen := map[string]string{}
	for _, raw := range ids {
		down := http.Header{}
		down.Set("x-opencode-session", raw)
		got := p.BuildHeaders(down, store.APIKey{APIKey: "public"},
			provider.Versions{Zen: "2.0.29"}).Get("x-opencode-session")

		if !idgen.IsSession(got) {
			t.Fatalf("mapped id for %q is invalid: %q", raw, got)
		}
		for other, mapped := range seen {
			if mapped == got {
				t.Errorf("%q and %q mapped to the same session %q", raw, other, got)
			}
		}
		seen[raw] = got
	}
}

// TestWhitespaceIsTrimmed 验证首尾空白不会导致误判为非法。
func TestWhitespaceIsTrimmed(t *testing.T) {
	ses := "ses_ef5920720ffeDse8BpbCnsgiMT"
	down := http.Header{}
	down.Set("x-opencode-session", "  "+ses+"  ")
	h := buildHeaders(t, down)

	if got := h.Get("x-opencode-session"); got != ses {
		t.Errorf("whitespace should be trimmed and valid id preserved, got %q", got)
	}
}

// TestMappingExpiresAfterTTL 验证超过 TTL 未访问会重新生成（需求：3600s）。
//
// TTL 与时间源都由 idmap 内部注入，这里通过导出行为间接验证：
// 使用较长 TTL 时稳定复用；用极短 TTL + 足够等待时重新生成。
// 不使用「20ms sleep 应落在 50ms TTL 内」这类依赖调度精度的断言（在 -race 下会抖动）。
func TestMappingExpiresAfterTTL(t *testing.T) {
	raw := "550e8400-e29b-41d4-a716-446655440000"
	header := func(p *Provider) string {
		down := http.Header{}
		down.Set("x-opencode-session", raw)
		return p.BuildHeaders(down, store.APIKey{APIKey: "public"},
			provider.Versions{Zen: "2.0.29"}).Get("x-opencode-session")
	}

	// 情形一：TTL 足够长 → 连续调用稳定复用
	long := NewWithTTL("", "", nil, time.Hour)
	first := header(long)
	for i := 0; i < 5; i++ {
		if got := header(long); got != first {
			t.Fatalf("within TTL mapping must be stable: %q vs %q", got, first)
		}
	}

	// 情形二：TTL 极短 + 明确超过 → 应重新生成
	short := NewWithTTL("", "", nil, 20*time.Millisecond)
	a := header(short)

	// 等到「距上次访问 > TTL」且留出宽裕余量，避免依赖精确调度
	deadline := time.Now().Add(2 * time.Second)
	var b string
	for time.Now().Before(deadline) {
		time.Sleep(60 * time.Millisecond)
		if b = header(short); b != a {
			break // 已重新生成
		}
	}
	if b == a {
		t.Errorf("after TTL expiry a fresh id should be issued (still %q)", a)
	}
}

// TestSweeperRemovesExpired 验证后台清理协程能回收条目。
func TestSweeperRemovesExpired(t *testing.T) {
	p := NewWithTTL("", "", nil, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartIDSweeper(ctx, 10*time.Millisecond)

	down := http.Header{}
	down.Set("x-opencode-session", "some-invalid-id")
	p.BuildHeaders(down, store.APIKey{APIKey: "public"}, provider.Versions{Zen: "2.0.29"})

	if p.sessions.Len() != 1 {
		t.Fatalf("expected 1 mapping, got %d", p.sessions.Len())
	}

	// 轮询等待清理完成，避免依赖固定 sleep 时长
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.sessions.Len() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("sweeper should have cleaned the expired mapping, got %d", p.sessions.Len())
}

// TestConcurrentBuildHeadersIsStable 验证并发请求下映射稳定（缓存不会被并发打散）。
func TestConcurrentBuildHeadersIsStable(t *testing.T) {
	p := newProvider(t)
	raw := "550e8400-e29b-41d4-a716-446655440000"

	const workers, per = 32, 100
	results := make([][]string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out := make([]string, 0, per)
			for j := 0; j < per; j++ {
				down := http.Header{}
				down.Set("x-opencode-session", raw)
				out = append(out, p.BuildHeaders(down, store.APIKey{APIKey: "public"},
					provider.Versions{Zen: "2.0.29"}).Get("x-opencode-session"))
			}
			results[i] = out
		}(i)
	}
	wg.Wait()

	want := results[0][0]
	if !idgen.IsSession(want) {
		t.Fatalf("mapped id invalid: %q", want)
	}
	for i, row := range results {
		for j, got := range row {
			if got != want {
				t.Fatalf("worker %d call %d: mapping unstable (%q vs %q)", i, j, got, want)
			}
		}
	}
}

// TestNilMapsFallBackToGenerating 验证未初始化映射表时仍能生成合法 ID（不 panic）。
func TestNilMapsFallBackToGenerating(t *testing.T) {
	p := &Provider{upstream: DefaultUpstream} // 不经 New，映射表为 nil
	down := http.Header{}
	down.Set("x-opencode-session", "invalid-id")

	h := p.BuildHeaders(down, store.APIKey{APIKey: "public"}, provider.Versions{Zen: "2.0.29"})
	if got := h.Get("x-opencode-session"); !idgen.IsSession(got) {
		t.Errorf("should fall back to generating a valid id, got %q", got)
	}
}

// TestSessionTTLIsOneHour 锁定需求规定的 3600s。
func TestSessionTTLIsOneHour(t *testing.T) {
	if SessionTTL != 3600*time.Second {
		t.Errorf("SessionTTL = %v, want 3600s", SessionTTL)
	}
}

// TestMissingIDNotStoredInMap 验证「未传头」不会在映射表里留下条目
// （否则映射表会被无意义的空键撑大）。
func TestMissingIDNotStoredInMap(t *testing.T) {
	p := newProvider(t)
	p.BuildHeaders(http.Header{}, store.APIKey{APIKey: "public"}, provider.Versions{Zen: "2.0.29"})

	if n := p.sessions.Len(); n != 0 {
		t.Errorf("missing session header should not create a mapping, got %d", n)
	}
}

// TestExistingValidIDNotStoredInMap 验证合法 ID 不占用映射表空间。
func TestExistingValidIDNotStoredInMap(t *testing.T) {
	p := newProvider(t)
	down := http.Header{}
	down.Set("x-opencode-session", "ses_ef5920720ffeDse8BpbCnsgiMT")
	p.BuildHeaders(down, store.APIKey{APIKey: "public"}, provider.Versions{Zen: "2.0.29"})

	if n := p.sessions.Len(); n != 0 {
		t.Errorf("valid id needs no mapping, got %d entries", n)
	}
}

// TestTruncatedAndUpperCaseSessionsAreMapped 覆盖各类“看起来像但不合法”的值。
func TestTruncatedAndUpperCaseSessionsAreMapped(t *testing.T) {
	cases := map[string]string{
		"hex 大写":     "ses_EF5920720FFEDse8BpbCnsgiMT",
		"长度不足":       "ses_abc",
		"base62 少一位": "ses_ef5920720ffeDse8BpbCnsgiM",
		"含下划线":       "ses_ef5920720ffeDse8BpbCnsgiM_",
	}
	for label, raw := range cases {
		p := newProvider(t)
		down := http.Header{}
		down.Set("x-opencode-session", raw)
		got := p.BuildHeaders(down, store.APIKey{APIKey: "public"},
			provider.Versions{Zen: "2.0.29"}).Get("x-opencode-session")

		if !idgen.IsSession(got) {
			t.Errorf("%s (%q): mapped to invalid %q", label, raw, got)
		}
		if strings.TrimSpace(got) == raw {
			t.Errorf("%s: invalid value must be replaced", label)
		}
	}
}
