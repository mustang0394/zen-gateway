// Package config 提供运行时配置，来源优先级：
//
//	命令行 flag > 环境变量 > 默认值
//
// 业务配置（上游 key、代理、下游 token、版本号等）一律存放在 SQLite，
// 由 Web 管理端维护；这里只保留进程级与部署相关配置。
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 环境变量名。
const (
	EnvListen      = "ZEN_LISTEN"
	EnvAdminToken  = "ZEN_ADMIN_TOKEN"
	EnvDataDir     = "ZEN_DATA_DIR"
	EnvDBPath      = "ZEN_DB_PATH"
	EnvZenUpstream = "ZEN_UPSTREAM"
	EnvClineUpstrm = "ZEN_CLINE_UPSTREAM"
	EnvRetention   = "ZEN_RETENTION_DAYS"
	EnvClineCool   = "ZEN_CLINE_COOLDOWN_FALLBACK"
)

// Config 是进程级配置。
type Config struct {
	// Listen 是网关监听地址。
	Listen string
	// DataDir 是运行时数据目录（存放 SQLite 数据库）。
	DataDir string
	// DBPath 是 SQLite 文件路径（优先于 DataDir 推导）。
	DBPath string
	// AdminToken 是 Web 管理端口令；为空则管理端不启用。
	AdminToken string
	// ZenUpstream 是 zen 模块上游基址。
	ZenUpstream string
	// ClineUpstream 是 cline 模块上游基址。
	ClineUpstream string
	// RetentionDays 是统计与明细的保留天数。
	RetentionDays int
	// ClineCooldownFallback 是 cline 429 无法解析冷却时长时的兜底值。
	ClineCooldownFallback time.Duration
	// Verbose 开启调试日志。
	Verbose bool
}

// Defaults 返回默认配置。
func Defaults() Config {
	return Config{
		Listen:                ":8080",
		DataDir:               "data",
		ZenUpstream:           "https://opencode.ai/zen/v1",
		ClineUpstream:         "https://api.cline.bot/api/v1",
		RetentionDays:         30,
		ClineCooldownFallback: time.Hour,
	}
}

// Load 从环境变量装配配置。flags 由调用方在解析后覆盖对应字段。
func Load() Config {
	c := Defaults()

	if v := strings.TrimSpace(os.Getenv(EnvListen)); v != "" {
		c.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvDataDir)); v != "" {
		c.DataDir = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvDBPath)); v != "" {
		c.DBPath = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvAdminToken)); v != "" {
		c.AdminToken = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvZenUpstream)); v != "" {
		c.ZenUpstream = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvClineUpstrm)); v != "" {
		c.ClineUpstream = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvRetention)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.RetentionDays = n
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvClineCool)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			c.ClineCooldownFallback = d
		}
	}
	return c
}

// DatabasePath 返回最终数据库文件路径。
func (c Config) DatabasePath() string {
	if strings.TrimSpace(c.DBPath) != "" {
		return c.DBPath
	}
	return filepath.Join(c.DataDir, "gateway.db")
}

// AdminEnabled 表示管理端是否可用（需配置口令）。
func (c Config) AdminEnabled() bool { return strings.TrimSpace(c.AdminToken) != "" }
