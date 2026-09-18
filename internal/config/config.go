// Package config 解析 key→代理 的映射配置。
//
// 三种来源，优先级从高到低：
//
//  1. 命令行 flag（-listen / -config）
//  2. 环境变量（Docker 部署推荐）：
//     ZEN_LISTEN  — 监听地址，如 ":8080"
//     ZEN_PROXIES — JSON 对象 {"key":"proxy-url", ...}
//  3. 配置文件（JSON，-config 指定）：
//
//	{
//	  "listen": ":8080",
//	  "proxies": {
//	    "oc_sk_xxxxx": "socks5h://user:pass@1.2.3.4:1080",
//	    "public":      "http://5.6.7.8:8080"
//	  }
//	}
//
// 语义：客户端 Authorization: Bearer <key> 命中 proxies 里的键时走对应代理；
// 未命中（包括没传 key 而网关补的 "public" 也没有映射）则直连。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Env 变量名。
const (
	EnvListen  = "ZEN_LISTEN"
	EnvProxies = "ZEN_PROXIES"
)

type File struct {
	Listen  string            `json:"listen"`
	Proxies map[string]string `json:"proxies"`
}

// Load 组装最终配置。
//
//   - path 非空时读取配置文件；文件里 listen/proxies 为对应来源的默认值
//   - env 始终参与合并，但显式 flag 在 main 里最后覆盖
//   - 未设置任何来源时 Listen 回落 ":8080"、Proxies 为空（全直连）
func Load(path string) (*File, error) {
	f := &File{Proxies: map[string]string{}}

	// 1) 配置文件
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %q: %w", path, err)
		}
		if err := json.Unmarshal(raw, f); err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
		if f.Proxies == nil {
			f.Proxies = map[string]string{}
		}
	}

	// 2) 环境变量（覆盖文件值；Docker 场景无需映射配置文件）
	if v := strings.TrimSpace(os.Getenv(EnvListen)); v != "" {
		f.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvProxies)); v != "" {
		var proxies map[string]string
		if err := json.Unmarshal([]byte(v), &proxies); err != nil {
			return nil, fmt.Errorf("parse %s: %w (want JSON object like {\"key\":\"socks5h://host:1080\"})", EnvProxies, err)
		}
		for k, p := range proxies {
			if k == "" || strings.TrimSpace(p) == "" {
				continue // 忽略空映射项
			}
			f.Proxies[k] = strings.TrimSpace(p)
		}
	}

	// 3) 默认值
	if f.Listen == "" {
		f.Listen = ":8080"
	}
	if f.Proxies == nil {
		f.Proxies = map[string]string{}
	}
	return f, nil
}

// ProxyFor 返回该 key 应使用的代理 URL；不存在返回空串（直连）。
func (f *File) ProxyFor(key string) string {
	if f == nil {
		return ""
	}
	return f.Proxies[key]
}
