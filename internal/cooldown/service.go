// Package cooldown 提供冷却池的服务层与后台维护。
//
// 存储语义位于 store（维度为 module + key + model，即某 key 的某模型冷却
// 不影响该 key 的其他模型）。本包负责：
//   - 对外提供冷却池的查询/手动解除/手动添加
//   - 定期清理已过期记录
//   - 按保留期清理过期统计与请求明细
package cooldown

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"zengateway/internal/store"
)

// Service 是冷却池服务。
type Service struct {
	store *store.Store
	log   *slog.Logger
}

// New 创建服务。
func New(st *store.Store, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{store: st, log: log}
}

// View 是冷却记录 + key 标签的展示结构。
type View struct {
	store.Cooldown
	KeyLabel  string `json:"keyLabel"`
	Remaining string `json:"remaining"`
}

// List 返回未过期的冷却记录（附带 key 标签与剩余时间）。
func (s *Service) List(module string) ([]View, error) {
	items, err := s.store.ListCooldowns(module, time.Now())
	if err != nil {
		return nil, err
	}
	labels := map[int64]string{}
	modules := map[string]bool{}
	for _, c := range items {
		modules[c.Module] = true
	}
	for m := range modules {
		keys, err := s.store.ListKeys(m)
		if err != nil {
			continue
		}
		for _, k := range keys {
			labels[k.ID] = k.Label
		}
	}
	out := make([]View, 0, len(items))
	for _, c := range items {
		out = append(out, View{
			Cooldown:  c,
			KeyLabel:  labels[c.KeyID],
			Remaining: HumanDuration(time.Until(c.Until)),
		})
	}
	return out, nil
}

// Release 手动解除一条冷却。
func (s *Service) Release(id int64) error {
	if err := s.store.DeleteCooldown(id); err != nil {
		return err
	}
	s.log.Info("cooldown released manually", "id", id)
	return nil
}

// Add 手动添加一条冷却。
func (s *Service) Add(module string, keyID int64, model string, until time.Time, reason string) error {
	if module == "" || keyID == 0 || strings.TrimSpace(model) == "" {
		return fmt.Errorf("module, keyId and model are required")
	}
	if until.Before(time.Now()) {
		return fmt.Errorf("cool down until must be in the future")
	}
	if reason == "" {
		reason = "manual"
	}
	return s.store.SetCooldown(store.Cooldown{
		Module: module, KeyID: keyID, Model: model,
		Until: until, Reason: reason, Detail: "added manually from admin UI",
		CreatedAt: time.Now(),
	})
}

// Sweep 清理已过期冷却记录；返回清理条数。
func (s *Service) Sweep() (int64, error) {
	n, err := s.store.PurgeExpiredCooldowns(time.Now())
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.log.Debug("expired cooldowns swept", "count", n)
	}
	return n, nil
}

// PurgeExpiredData 按保留天数清理过期统计与请求明细（直接删除，不归档）。
func (s *Service) PurgeExpiredData(retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	n, err := s.store.PurgeOlderThan(cutoff)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.log.Info("expired stats purged", "rows", n, "retentionDays", retentionDays)
	}
	return n, nil
}

// Run 启动后台维护：每 10 分钟清理过期冷却，每日清理过期统计数据。
func (s *Service) Run(ctx context.Context, retentionDays int) {
	go func() {
		cooldownTicker := time.NewTicker(10 * time.Minute)
		defer cooldownTicker.Stop()
		dataTicker := time.NewTicker(24 * time.Hour)
		defer dataTicker.Stop()

		// 启动时先执行一次，避免重启后长时间不清理
		if _, err := s.Sweep(); err != nil {
			s.log.Warn("initial cooldown sweep failed", "err", err)
		}
		if _, err := s.PurgeExpiredData(retentionDays); err != nil {
			s.log.Warn("initial stats purge failed", "err", err)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-cooldownTicker.C:
				if _, err := s.Sweep(); err != nil {
					s.log.Warn("cooldown sweep failed", "err", err)
				}
			case <-dataTicker.C:
				if _, err := s.PurgeExpiredData(retentionDays); err != nil {
					s.log.Warn("stats purge failed", "err", err)
				}
			}
		}
	}()
}

// HumanDuration 把剩余时间格式化为 "22h59m" / "3d4h" / "5m" 形式。
// 秒级余量向上取整到分钟，避免剩余 59 秒显示为 0m。
func HumanDuration(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	totalMins := int((d + time.Minute - 1) / time.Minute)
	days := totalMins / (24 * 60)
	hours := (totalMins / 60) % 24
	mins := totalMins % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}
