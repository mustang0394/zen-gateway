// Command zen-gateway 是 OpenCode Zen 免费模型中转网关。
//
// 用法：
//
//	zen-gateway [-config /etc/zen-gateway.json] [-listen :8080] [-upstream https://opencode.ai/zen/v1] [-verbose]
//
// 配置文件（可选）：
//
//	{
//	  "listen": ":8080",
//	  "proxies": {
//	    "oc_sk_xxxx": "socks5h://user:pass@host:1080",
//	    "public": "http://host:port"
//	  }
//	}
//
// 端点：
//
//	POST /v1/chat/completions   → 上游 /chat/completions
//	POST /v1/responses          → 上游 /responses
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
	"zengateway/internal/version"
	ztproxy "zengateway/internal/ztproxy"
)

// 构建时注入（-ldflags "-X main.buildVersion=... -X main.commit=..."）
var (
	buildVersion = "dev"
	commit       = "unknown"
)

func main() {
	configPath := flag.String("config", "", "path to json config (key→proxy map)")
	listen := flag.String("listen", "", "listen address (overrides config file and ZEN_LISTEN env)")
	upstream := flag.String("upstream", "https://opencode.ai/zen/v1", "upstream zen base url")
	verbose := flag.Bool("verbose", false, "debug logging")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: levelFor(*verbose)}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	addr := *listen
	if addr == "" {
		addr = cfg.Listen
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 每日从 GitHub 拉最新 opencode 版本号，刷新网关 UA
	version.StartDailyRefresh(ctx, log)

	gw := ztproxy.New(cfg, log, *upstream)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", gw.ChatCompletions)
	mux.HandleFunc("/v1/responses", gw.Responses)
	// 不带 /v1 前缀也接受，方便不同客户端习惯
	mux.HandleFunc("/chat/completions", gw.ChatCompletions)
	mux.HandleFunc("/responses", gw.Responses)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
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

	log.Info("zen-gateway listening", "addr", addr, "upstream", *upstream,
		"version", version.Current(), "buildVersion", buildVersion, "commit", commit, "proxiedKeys", len(cfg.Proxies))
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
