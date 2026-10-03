// Command zen-gateway 是 OpenCode Zen 与 Cline 的 OpenAI 兼容中转网关。
//
// 用法：
//
//	zen-gateway [-listen :8080] [-data-dir data] [-upstream-zen URL] [-upstream-cline URL] [-verbose]
//
// 环境变量：
//
//	ZEN_LISTEN                 监听地址（默认 :8080）
//	ZEN_ADMIN_TOKEN            Web 管理端口令；未设置则不启用管理端
//	ZEN_DATA_DIR               数据目录（默认 data）
//	ZEN_DB_PATH                SQLite 文件路径（优先于 ZEN_DATA_DIR）
//	ZEN_UPSTREAM               zen 模块上游基址
//	ZEN_CLINE_UPSTREAM         cline 模块上游基址
//	ZEN_RETENTION_DAYS         统计保留天数（默认 30）
//	ZEN_CLINE_COOLDOWN_FALLBACK cline 429 兜底冷却时长（默认 1h）
//
// 端点：
//
//	POST /zen/v1/chat/completions     zen 模块 chat
//	POST /zen/v1/responses            zen 模块 responses
//	GET  /zen/v1/models               zen 模块模型列表
//	POST /cline/v1/chat/completions   cline 模块 chat
//	POST /cline/v1/responses          cline 模块 responses
//	GET  /cline/v1/models             cline 模块模型列表
//	GET  /admin                       Web 管理端
//	GET  /healthz                     健康检查
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"zengateway/internal/config"
	"zengateway/internal/cooldown"
	"zengateway/internal/provider"
	"zengateway/internal/provider/cline"
	"zengateway/internal/provider/zen"
	"zengateway/internal/router"
	"zengateway/internal/store"
	"zengateway/internal/version"
	"zengateway/internal/web"
)

// 构建时注入（-ldflags "-X main.buildVersion=... -X main.commit=..."）
var (
	buildVersion = "dev"
	commit       = "unknown"
)

func main() {
	cfg := config.Load()

	flag.StringVar(&cfg.Listen, "listen", cfg.Listen, "listen address (overrides ZEN_LISTEN)")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "data directory for sqlite database")
	flag.StringVar(&cfg.DBPath, "db", cfg.DBPath, "sqlite database path (overrides -data-dir)")
	flag.StringVar(&cfg.ZenUpstream, "upstream-zen", cfg.ZenUpstream, "zen upstream base url")
	flag.StringVar(&cfg.ClineUpstream, "upstream-cline", cfg.ClineUpstream, "cline upstream base url")
	flag.IntVar(&cfg.RetentionDays, "retention-days", cfg.RetentionDays, "days to keep stats and request logs")
	flag.BoolVar(&cfg.Verbose, "verbose", false, "debug logging")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: levelFor(cfg.Verbose)}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 1) 数据层
	st, err := store.Open(cfg.DatabasePath(), log)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// 2) 版本管理（每日刷新 zen / cline.cli / cline.sdk）
	vers := version.New(st, log, version.DefaultTargets())
	vers.StartDaily(ctx)

	// 3) 冷却池与数据保留策略
	cool := cooldown.New(st, log)
	cool.Run(ctx, cfg.RetentionDays)

	// 4) 模块
	versionsFn := func() provider.Versions {
		return provider.Versions{Zen: vers.Zen(), ClineCLI: vers.ClineCLI(), ClineSDK: vers.ClineSDK()}
	}
	zenHandler := provider.NewHandler(zen.New(cfg.ZenUpstream), provider.Runtime{
		Store: st, Log: log, Versions: versionsFn,
	})
	clineHandler := provider.NewHandler(cline.New(cfg.ClineUpstream, cfg.ClineCooldownFallback), provider.Runtime{
		Store: st, Log: log, Versions: versionsFn,
	})

	// 5) 管理端（需 ZEN_ADMIN_TOKEN）
	admin := web.New(st, vers, cool, log, cfg.AdminToken, cfg.ZenUpstream, cfg.ClineUpstream)
	if admin == nil {
		log.Warn("admin UI disabled: set ZEN_ADMIN_TOKEN to enable /admin")
	}

	// 6) 路由
	handlers := map[string]router.ModuleHandler{
		"zen":   zenHandler,
		"cline": clineHandler,
	}
	var adminHandler http.Handler
	if admin != nil {
		adminHandler = admin
	}
	rt := router.New(handlers, router.ModuleTokens(st, log), adminHandler)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           rt,
		ReadHeaderTimeout: 15 * time.Second,
		// 不设 WriteTimeout/IdleTimeout 上限：SSE 长流需要
	}

	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shCtx)
	}()

	log.Info("zen-gateway listening",
		"addr", cfg.Listen,
		"db", cfg.DatabasePath(),
		"zenUpstream", cfg.ZenUpstream,
		"clineUpstream", cfg.ClineUpstream,
		"admin", admin != nil,
		"buildVersion", buildVersion,
		"commit", commit,
		"zenVersion", vers.Zen(),
		"clineCliVersion", vers.ClineCLI(),
		"clineSdkVersion", vers.ClineSDK(),
	)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

func levelFor(verbose bool) slog.Level {
	if verbose {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
