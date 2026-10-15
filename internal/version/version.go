// Package version 负责从上游抓取各上游所需的版本号,并每日刷新。
//
// 三个版本目标:
//
//	zen        opencode 客户端的 semver(v2.0.29 → 2.0.29),
//	           用于 User-Agent 的 opencode/<channel>/<version>/cli 形状
//	cline.cli  cline 主版本 tag(v4.1.22 → 4.1.22),用于 X-CLIENT-VERSION /
//	           X-PLATFORM-VERSION / User-Agent
//	cline.sdk  cline SDK tag(sdk/sdk/v0.0.90 → 0.0.90),用于 X-CORE-VERSION
//
// 两个仓库的发布形态不同,抓取也分开:
//   - opencode 自 2.x 起只打 git tag、不再发 GitHub Release,GitHub 的
//     /releases/latest 至今仍停在 1.x。因此 zen 优先取 npm 包 @opencode/cli 的
//     latest dist-tag(该 tag 即官方 CLI 的发布号),再依次回落
//     GitHub releases/latest 与最新 semver tag。
//   - cline 仓库的 tag 存在多个前缀(desktop-v*、cli-v*、sdk/sdk/v*、v*),
//     因此不能使用 /releases/latest,必须拉取发布列表后按 tag 形态分类并取最大版本。
package version

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"zengateway/internal/store"
)

// 版本目标名（同时作为 store 中的主键）。
const (
	NameZen      = "zen"
	NameClineCLI = "cline.cli"
	NameClineSDK = "cline.sdk"
)

// Names 返回全部版本目标（用于初始化与展示）。
func Names() []string { return []string{NameZen, NameClineCLI, NameClineSDK} }

// Targets 是各目标的抓取配置。
type Targets struct {
	OpencodeRepo string // 默认 anomalyco/opencode
	ClineRepo    string // 默认 cline/cline
	// OpencodePackage 是 opencode 官方 CLI 的 npm 包名;zen 版本号的权威来源。
	OpencodePackage string // 默认 @opencode/cli
}

// DefaultTargets 返回默认抓取配置。
func DefaultTargets() Targets {
	return Targets{OpencodeRepo: "anomalyco/opencode", ClineRepo: "cline/cline", OpencodePackage: "@opencode/cli"}
}

// 兜底值：GitHub 不可达时保证 UA / header 仍可构造。
// zen 兜底值跟随网关镜像的官方客户端版本（当前 CLI 2.0.29）；
// 上游只校验 UA 形状而不校验版本号，但保持 2.x 更贴近真实客户端。
var fallback = map[string]string{
	NameZen:      "2.0.29",
	NameClineCLI: "4.1.22",
	NameClineSDK: "0.0.90",
}

var (
	zenTagRe = regexp.MustCompile(`^v?(\d+\.\d+\.\d+)$`)
	// cline 主版本 tag：严格 v 前缀的三段版本，排除 cli-v* / desktop-v* / sdk
	clineCLITagRe = regexp.MustCompile(`^v(\d+\.\d+\.\d+)$`)
	// cline SDK tag：sdk/sdk/vX.Y.Z（注意 tag 带路径前缀，不是 name 字段）
	clineSDKTagRe = regexp.MustCompile(`^sdk/sdk/v(\d+\.\d+\.\d+)$`)
	// 排除预发布后缀
	prereleaseRe = regexp.MustCompile(`-(beta|rc|alpha|pre)`)
)

// Manager 维护版本号并提供每日刷新。
type Manager struct {
	store   *store.Store
	log     *slog.Logger
	client  *http.Client
	targets Targets

	mu   sync.RWMutex
	vals map[string]string
}

// New 创建 Manager，并用 store 中的已有值（或兜底值）初始化内存缓存。
func New(st *store.Store, log *slog.Logger, targets Targets) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Manager{
		store:   st,
		log:     log,
		client:  &http.Client{Timeout: 20 * time.Second},
		targets: targets,
		vals:    map[string]string{},
	}
	for _, name := range Names() {
		m.vals[name] = m.resolveInitial(name)
	}
	return m
}

// resolveInitial 读取持久化值；缺失或无效时回落兜底值。
func (m *Manager) resolveInitial(name string) string {
	if m.store != nil {
		if v, err := m.store.GetVersion(name); err == nil && validVersion(v.Value) {
			return v.Value
		}
	}
	return fallback[name]
}

func validVersion(v string) bool {
	return regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v)
}

// Get 返回某目标的当前版本号。
func (m *Manager) Get(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if v := m.vals[name]; v != "" {
		return v
	}
	return fallback[name]
}

// Zen 返回 opencode 版本号（用于 User-Agent 与 x-opencode-* 相关判断）。
func (m *Manager) Zen() string { return m.Get(NameZen) }

// ClineCLI 返回 cline 主版本号。
func (m *Manager) ClineCLI() string { return m.Get(NameClineCLI) }

// ClineSDK 返回 cline SDK 版本号。
func (m *Manager) ClineSDK() string { return m.Get(NameClineSDK) }

// Snapshot 返回全部版本值的副本。
func (m *Manager) Snapshot() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.vals))
	for k, v := range m.vals {
		out[k] = v
	}
	return out
}

// Refresh 立即抓取全部目标并落库；返回各目标结果。
func (m *Manager) Refresh(ctx context.Context) map[string]store.Version {
	results := map[string]store.Version{}
	for _, r := range []struct {
		name string
		fn   func(context.Context) (string, error)
	}{
		{NameZen, m.fetchZen},
		{NameClineCLI, m.fetchClineCLI},
		{NameClineSDK, m.fetchClineSDK},
	} {
		val, err := r.fn(ctx)
		now := time.Now()
		v := store.Version{Name: r.name, Value: m.Get(r.name), FetchedAt: now, OK: err == nil}
		if err != nil {
			v.Error = err.Error()
			m.log.Warn("version refresh failed, keeping previous", "name", r.name, "value", v.Value, "err", err)
		} else {
			v.Value = val
			m.mu.Lock()
			m.vals[r.name] = val
			m.mu.Unlock()
			m.log.Info("version refreshed", "name", r.name, "value", val)
		}
		if m.store != nil {
			if err := m.store.SaveVersion(v); err != nil {
				m.log.Warn("save version failed", "name", r.name, "err", err)
			}
		}
		results[r.name] = v
	}
	return results
}

// StartDaily 启动每日刷新协程：启动时立即抓取一次，随后每 24 小时一次。
// 调用方应在进程退出时 cancel context。
func (m *Manager) StartDaily(ctx context.Context) {
	m.Refresh(ctx)
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.Refresh(ctx)
			}
		}
	}()
}

// ---- 抓取实现 -------------------------------------------------------------

type release struct {
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// latestRelease 拉取仓库最新正式 release 的 tag。
func (m *Manager) latestRelease(ctx context.Context, repo string) (release, error) {
	var r release
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	if err := m.getJSON(ctx, url, &r); err != nil {
		return release{}, err
	}
	return r, nil
}

// releaseList 拉取仓库发布列表(单页最多 100 条,足以覆盖最新版本)。
func (m *Manager) releaseList(ctx context.Context, repo string, perPage int) ([]release, error) {
	var out []release
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d", repo, perPage)
	if err := m.getJSON(ctx, url, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// tag 是 GitHub tag 列表中的一项。
type tag struct {
	Name string `json:"name"`
}

// tagList 拉取仓库的 tag 列表。
// 注意列表顺序不保证版本降序(实测 anomalyco/opencode 首页第 15 项才是
// v2.0.29,前面全是 vscode-v*),因此调用方必须自己过滤并比较大小。
func (m *Manager) tagList(ctx context.Context, repo string, perPage int) ([]tag, error) {
	var out []tag
	url := fmt.Sprintf("https://api.github.com/repos/%s/tags?per_page=%d", repo, perPage)
	if err := m.getJSON(ctx, url, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Manager) getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "zen-gateway")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream status %d for %s", resp.StatusCode, url)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// fetchZen 取 opencode 客户端的 semver。
//
// opencode 自 2.x 起只打 git tag、不再发 GitHub Release:
// /releases/latest 至今仍停在 1.x。因此按权威程度依次尝试三种来源,
// 前一个失败(网络不可达、格式不符、无匹配 tag)就换下一个:
//
//  1. npm @opencode/cli 的 latest dist-tag —— 与命令行 `npm i -g` 装到的版本一致
//  2. GitHub releases/latest —— 旧渠道,官方恢复发 Release 后仍能生效
//  3. GitHub tags —— 取最大的纯 semver tag(需排除 vscode-v* 等其它组件的 tag)
//
// 全部失败时返回错误,由 Refresh 保留上一次的值。
func (m *Manager) fetchZen(ctx context.Context) (string, error) {
	var errs []string
	for _, source := range []struct {
		name string
		fn   func(context.Context) (string, error)
	}{
		{"npm dist-tag", m.fetchZenFromNPM},
		{"github release", m.fetchZenFromRelease},
		{"github tags", m.fetchZenFromTags},
	} {
		version, err := source.fn(ctx)
		if err == nil {
			return version, nil
		}
		errs = append(errs, source.name+": "+err.Error())
	}
	return "", fmt.Errorf("%s: %s", m.targets.OpencodeRepo, strings.Join(errs, "; "))
}

// npmDistTags 是 npm registry 的 dist-tags 响应(只取所需字段)。
type npmDistTags struct {
	Latest string `json:"latest"`
}

// fetchZenFromNPM 取 opencode 官方 CLI 的 latest dist-tag。
func (m *Manager) fetchZenFromNPM(ctx context.Context) (string, error) {
	pkg := strings.TrimSpace(m.targets.OpencodePackage)
	if pkg == "" {
		pkg = "@opencode/cli"
	}
	// 该端点仅 97 字节,远小于整包元数据(npm 全量文档可达数百 KB)。
	url := "https://registry.npmjs.org/-/package/" + pkg + "/dist-tags"
	var tags npmDistTags
	if err := m.getJSON(ctx, url, &tags); err != nil {
		return "", err
	}
	version := strings.TrimSpace(tags.Latest)
	if !validVersion(version) {
		return "", fmt.Errorf("unexpected npm latest %q", tags.Latest)
	}
	return version, nil
}

// fetchZenFromRelease 取 GitHub 最新正式 Release 的 tag。
func (m *Manager) fetchZenFromRelease(ctx context.Context) (string, error) {
	r, err := m.latestRelease(ctx, m.targets.OpencodeRepo)
	if err != nil {
		return "", err
	}
	tag := strings.TrimSpace(r.TagName)
	if !zenTagRe.MatchString(tag) {
		return "", fmt.Errorf("unexpected opencode tag %q", tag)
	}
	return strings.TrimPrefix(tag, "v"), nil
}

// fetchZenFromTags 从 GitHub tag 列表中取最大的纯 semver tag。
//
// 该仓库的 tag 混有其它组件(vscode-v*),且列表顺序不保证新→旧,
// 因此必须逐项过滤后按数值取最大,而不能直接取第一个。
func (m *Manager) fetchZenFromTags(ctx context.Context) (string, error) {
	list, err := m.tagList(ctx, m.targets.OpencodeRepo, 100)
	if err != nil {
		return "", err
	}
	var found []string
	for _, t := range list {
		if mm := zenTagRe.FindStringSubmatch(strings.TrimSpace(t.Name)); mm != nil {
			found = append(found, mm[1])
		}
	}
	if len(found) == 0 {
		return "", fmt.Errorf("no semver tag found in %d tags", len(list))
	}
	return maxSemver(found), nil
}

// fetchClineCLI 从 cline 发布列表中挑出最大的 vX.Y.Z 主版本。
func (m *Manager) fetchClineCLI(ctx context.Context) (string, error) {
	list, err := m.releaseList(ctx, m.targets.ClineRepo, 100)
	if err != nil {
		return "", err
	}
	var found []string
	for _, r := range list {
		if r.Draft || r.Prerelease || prereleaseRe.MatchString(r.TagName) {
			continue
		}
		if mm := clineCLITagRe.FindStringSubmatch(r.TagName); mm != nil {
			found = append(found, mm[1])
		}
	}
	if len(found) == 0 {
		return "", fmt.Errorf("no cline CLI version tag found in %d releases", len(list))
	}
	return maxSemver(found), nil
}

// fetchClineSDK 从 cline 发布列表中挑出最大的 sdk/sdk/vX.Y.Z 版本。
func (m *Manager) fetchClineSDK(ctx context.Context) (string, error) {
	list, err := m.releaseList(ctx, m.targets.ClineRepo, 100)
	if err != nil {
		return "", err
	}
	var found []string
	for _, r := range list {
		if r.Draft || r.Prerelease || prereleaseRe.MatchString(r.TagName) {
			continue
		}
		if mm := clineSDKTagRe.FindStringSubmatch(r.TagName); mm != nil {
			found = append(found, mm[1])
		}
	}
	if len(found) == 0 {
		return "", fmt.Errorf("no cline SDK version tag found in %d releases", len(list))
	}
	return maxSemver(found), nil
}

// maxSemver 返回版本号中最大者（按数值比较，而非字典序）。
func maxSemver(vs []string) string {
	sorted := append([]string(nil), vs...)
	sort.Slice(sorted, func(i, j int) bool { return compareSemver(sorted[i], sorted[j]) > 0 })
	return sorted[0]
}

// compareSemver 比较三个数值段；返回 >0 表示 a 大于 b。
func compareSemver(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		na, _ := strconv.Atoi(part(pa, i))
		nb, _ := strconv.Atoi(part(pb, i))
		if na != nb {
			return na - nb
		}
	}
	return 0
}

func part(p []string, i int) string {
	if i < len(p) {
		return p[i]
	}
	return "0"
}
