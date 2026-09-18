// Package version 负责获取 opencode 最新版本号并维护 User-Agent。
//
// 每日定时请求 GitHub releases API，把网关发往上游的 User-Agent 覆盖为
// "opencode/<latest>"，不信任下游客户端传入的值。
package version

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"
)

var (
	current atomic.Value // 存 string
	client  = &http.Client{Timeout: 15 * time.Second}
	// 只接受形如 v1.2.3 / 1.2.3 的版本号，防止 API 异常返回时把脏值注入 UA
	semverRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)
)

const fallbackVersion = "1.18.31" // GitHub 拉取失败时的兜底版本

// Current 返回当前应使用的版本号。
func Current() string {
	v, _ := current.Load().(string)
	if v == "" {
		return fallbackVersion
	}
	return v
}

// UserAgent 返回完整 UA 头值。与真实 opencode 客户端一致地以
// "opencode/<version>" 开头；后缀补充 runtime 信息可提升相似度，实测仅前缀必要。
func UserAgent() string {
	return "opencode/" + Current()
}

// StartDailyRefresh 启动每日一次的版本刷新协程；启动时立即拉取一次。
// 调用方在进程退出时应 cancel context。
func StartDailyRefresh(ctx context.Context, log *slog.Logger) {
	if err := refresh(ctx, log); err != nil {
		log.Warn("initial version fetch failed, using fallback", "version", fallbackVersion, "err", err)
	}
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := refresh(ctx, log); err != nil {
					log.Warn("daily version refresh failed, keeping previous", "version", Current(), "err", err)
				} else {
					log.Info("version refreshed", "version", Current())
				}
			}
		}
	}()
}

// refresh 请求 GitHub 最新 release 并校验后写入 current。
// 用 /releases/latest：opencode 官方发布走正式 release，pre-release 会被该端点自动跳过。
func refresh(ctx context.Context, log *slog.Logger) error {
	const url = "https://api.github.com/repos/anomalyco/opencode/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "zen-gateway")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github api status %d", resp.StatusCode)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	tag := payload.TagName
	if !semverRe.MatchString(tag) {
		return fmt.Errorf("unexpected tag %q", tag)
	}
	if tag[0] == 'v' {
		tag = tag[1:]
	}
	current.Store(tag)
	log.Info("github version fetched", "version", tag)
	return nil
}
