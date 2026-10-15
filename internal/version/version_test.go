package version

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"zengateway/internal/store"
)

func newTestManager(t *testing.T, base string) *Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "v.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, nil, Targets{OpencodeRepo: "anomalyco/opencode", ClineRepo: "cline/cline"})
	// 把请求重定向到本地测试服务器
	m.client = &http.Client{Transport: rewriteTransport{base: base}}
	return m
}

// rewriteTransport 把 GitHub API 请求重写到本地 httptest 服务器。
type rewriteTransport struct{ base string }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = t.base
	return http.DefaultTransport.RoundTrip(clone)
}

// clineReleases 是取自真实 GitHub 的发布列表样本，用于验证 tag 分类。
var clineReleases = []release{
	{TagName: "sdk/sdk/v0.0.90", Name: "SDK v0.0.90"},
	{TagName: "desktop-v0.0.43", Name: "Desktop v0.0.43"},
	{TagName: "desktop-v0.0.42", Name: "Desktop v0.0.42"},
	{TagName: "cli-v3.0.68", Name: "CLI v3.0.68"},
	{TagName: "v4.1.22", Name: "v4.1.22"},
	{TagName: "sdk/sdk/v0.0.89", Name: "SDK v0.0.89"},
	{TagName: "cli-v3.0.67", Name: "CLI v3.0.67"},
	{TagName: "v4.1.21", Name: "v4.1.21"},
	{TagName: "desktop-v0.0.23-beta.1", Name: "Desktop v0.0.23-beta.1"},
	{TagName: "v4.1.9-rc", Name: "v4.1.9-rc"},
	{TagName: "sdk/sdk/v0.0.88", Name: "SDK v0.0.88"},
	{TagName: "v4.1.10", Name: "v4.1.10"},
}

func TestRefreshClassifiesTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/-/package/@opencode/cli/dist-tags":
			json.NewEncoder(w).Encode(npmDistTags{Latest: "1.19.4"})
		case r.URL.Path == "/repos/cline/cline/releases":
			json.NewEncoder(w).Encode(clineReleases)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Listener.Addr().String())
	got := m.Refresh(context.Background())

	for name, want := range map[string]string{
		NameZen:      "1.19.4",
		NameClineCLI: "4.1.22", // 必须是 v* 里最大的，排除 cli-v3.0.68 / desktop-*
		NameClineSDK: "0.0.90", // 必须来自 sdk/sdk/v*，排除 desktop-v0.0.43
	} {
		if !got[name].OK {
			t.Fatalf("%s refresh failed: %s", name, got[name].Error)
		}
		if v := m.Get(name); v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
}

func TestRefreshKeepsPreviousOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Listener.Addr().String())
	before := m.Snapshot()

	got := m.Refresh(context.Background())
	for name, v := range got {
		if v.OK {
			t.Errorf("%s should have failed", name)
		}
		if v.Error == "" {
			t.Errorf("%s should record error reason", name)
		}
	}
	after := m.Snapshot()
	for name, want := range before {
		if after[name] != want {
			t.Errorf("%s changed on failure: %q -> %q", name, want, after[name])
		}
	}
}

func TestRefreshPersistsAndReloads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/anomalyco/opencode/releases/latest":
			json.NewEncoder(w).Encode(release{TagName: "v2.0.0"})
		case "/repos/cline/cline/releases":
			json.NewEncoder(w).Encode(clineReleases)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "persist.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	m := New(st, nil, Targets{OpencodeRepo: "anomalyco/opencode", ClineRepo: "cline/cline"})
	m.client = &http.Client{Transport: rewriteTransport{base: srv.Listener.Addr().String()}}
	m.Refresh(context.Background())

	// 新实例应从 store 读回持久化值
	m2 := New(st, nil, Targets{})
	if m2.Zen() != "2.0.0" {
		t.Errorf("zen after reload = %q, want 2.0.0", m2.Zen())
	}
	if m2.ClineCLI() != "4.1.22" || m2.ClineSDK() != "0.0.90" {
		t.Errorf("cline versions after reload = %q / %q", m2.ClineCLI(), m2.ClineSDK())
	}
}

func TestUnreachableGithubUsesFallback(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "empty.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	m := New(st, nil, DefaultTargets())
	if m.Zen() != fallback[NameZen] {
		t.Errorf("zen fallback = %q", m.Zen())
	}
	if m.ClineCLI() != fallback[NameClineCLI] || m.ClineSDK() != fallback[NameClineSDK] {
		t.Errorf("cline fallback = %q / %q", m.ClineCLI(), m.ClineSDK())
	}
}

func TestRejectsInvalidVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/-/package/@opencode/cli/dist-tags":
			json.NewEncoder(w).Encode(npmDistTags{Latest: "not-a-version"})
		case "/repos/anomalyco/opencode/releases/latest":
			json.NewEncoder(w).Encode(release{TagName: "not-a-version"})
		case "/repos/anomalyco/opencode/tags":
			json.NewEncoder(w).Encode([]tag{{Name: "vscode-v1.0.0"}, {Name: "nightly"}})
		case "/repos/cline/cline/releases":
			json.NewEncoder(w).Encode([]release{{TagName: "desktop-v0.0.43"}, {TagName: "cli-v3.0.68"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Listener.Addr().String())
	got := m.Refresh(context.Background())
	for name := range got {
		if got[name].OK {
			t.Errorf("%s should reject invalid/absent tags", name)
		}
	}
	if m.Zen() != fallback[NameZen] {
		t.Errorf("zen should keep fallback when tag invalid, got %q", m.Zen())
	}
}

func TestCompareAndMaxSemver(t *testing.T) {
	if compareSemver("4.1.22", "4.1.9") <= 0 {
		t.Error("4.1.22 should be greater than 4.1.9 (numeric, not lexicographic)")
	}
	if compareSemver("1.2.3", "1.2.3") != 0 {
		t.Error("equal versions should compare 0")
	}
	if compareSemver("0.0.90", "0.0.100") >= 0 {
		t.Error("0.0.100 should be greater than 0.0.90")
	}
	if got := maxSemver([]string{"4.1.9", "4.1.22", "4.1.10"}); got != "4.1.22" {
		t.Errorf("maxSemver = %q, want 4.1.22", got)
	}
	if got := maxSemver([]string{"0.0.90"}); got != "0.0.90" {
		t.Errorf("maxSemver single = %q", got)
	}
}

func TestStartDailyRefreshesImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/anomalyco/opencode/releases/latest":
			json.NewEncoder(w).Encode(release{TagName: "v9.9.9"})
		case "/repos/cline/cline/releases":
			json.NewEncoder(w).Encode(clineReleases)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Listener.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartDaily(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.Zen() == "9.9.9" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("StartDaily did not refresh on startup, zen=%q", m.Zen())
}

// zenSourceServer 按来源逐个可控地返回响应:对应路径返回 ok 时给 body,否则 404。
func zenSourceServer(t *testing.T, npmLatest, releaseTag, tags string) *Manager {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/-/package/@opencode/cli/dist-tags":
			if npmLatest == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(npmDistTags{Latest: npmLatest})
		case "/repos/anomalyco/opencode/releases/latest":
			if releaseTag == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(release{TagName: releaseTag})
		case "/repos/anomalyco/opencode/tags":
			if tags == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// 直接写原始 JSON(该参数就是 GitHub tag 列表的响应体)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(tags))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return newTestManager(t, srv.Listener.Addr().String())
}

// TestZenPrefersNPMDistTag 锁定首选来源:npm 的 latest dist-tag 才是官方 CLI
// 的发布号。GitHub Releases 自 opencode 2.x 起不再更新(停在 1.x),因此
// 即使 Release 接口能返回,也必须优先采用 npm 的 2.x。
func TestZenPrefersNPMDistTag(t *testing.T) {
	// Release 仍停在 1.x(线上真实情况),npm 已是 2.x
	m := zenSourceServer(t, "2.0.29", "v1.19.4", `[{"name":"v2.0.28"}]`)

	if _, err := m.fetchZen(context.Background()); err != nil {
		t.Fatalf("fetchZen: %v", err)
	}
	if got := m.Get(NameZen); got != "2.0.29" {
		t.Errorf("zen = %q, want 2.0.29 (npm must win over the stale release)", got)
	}
}

// TestZenFallsBackToRelease 验证 npm 不可用时回落到 GitHub Release。
func TestZenFallsBackToRelease(t *testing.T) {
	m := zenSourceServer(t, "", "v1.19.4", `[{"name":"v1.19.3"}]`)
	if got, err := m.fetchZen(context.Background()); err != nil || got != "1.19.4" {
		t.Errorf("fetchZen = %q, %v; want 1.19.4 from release", got, err)
	}
}

// TestZenFallsBackToTags 验证 npm 与 Release 都不可用时回落到 tag 列表,
// 并且能跳过 vscode-v* 等其它组件的 tag、按数值取最大。
func TestZenFallsBackToTags(t *testing.T) {
	tags := `[{"name":"vscode-v1.0.0"},{"name":"v2.0.29"},{"name":"v2.0.9"},{"name":"nightly"}]`
	m := zenSourceServer(t, "", "", tags)

	got, err := m.fetchZen(context.Background())
	if err != nil {
		t.Fatalf("fetchZen: %v", err)
	}
	if got != "2.0.29" {
		t.Errorf("fetchZen = %q, want 2.0.29 (numeric max, vscode-* skipped)", got)
	}
}

// TestZenAllSourcesFail 验证三个来源全失败时报错(由 Refresh 保留旧值)。
func TestZenAllSourcesFail(t *testing.T) {
	m := zenSourceServer(t, "not-a-version", "v1.2", `[{"name":"vscode-v1.0.0"}]`)

	if _, err := m.fetchZen(context.Background()); err == nil {
		t.Fatal("expected an error when every source is unusable")
	}
	if got := m.Get(NameZen); got != fallback[NameZen] {
		t.Errorf("zen should keep the previous value on total failure, got %q", got)
	}
}
