// Package upstream 构建按目标 key 路由代理的 HTTP 客户端。
//
// 支持 http(s):// 、socks5:// 与 socks5h:// 三类代理：
//   - http(s)://   → http.Transport.Proxy（CONNECT 隧道）
//   - socks5h://   → x/net/proxy.SOCKS5，域名直接交给代理解析（远端解析）
//   - socks5://    → 同样走 SOCKS5，但在本地先解析域名再把 IP 交给代理
//     （与 curl 的 socks5/socks5h 语义对齐）
package upstream

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// ClientFor 返回走指定代理的 HTTP 客户端；proxyURL 为空则直连。
// 相同代理配置的客户端会被缓存并复用，以保证连接池与 Keep-Alive 生效
// （否则每个请求都会重新握手，对流式请求开销明显）。
// 客户端不设全局超时（SSE 长流），超时交给请求 context 控制。
func ClientFor(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)

	cacheMu.RLock()
	if c, ok := cache[proxyURL]; ok {
		cacheMu.RUnlock()
		return c, nil
	}
	cacheMu.RUnlock()

	c, err := build(proxyURL)
	if err != nil {
		return nil, err
	}

	cacheMu.Lock()
	if existing, ok := cache[proxyURL]; ok {
		cacheMu.Unlock()
		return existing, nil
	}
	cache[proxyURL] = c
	cacheMu.Unlock()
	return c, nil
}

var (
	cacheMu sync.RWMutex
	cache   = map[string]*http.Client{}
)

func build(proxyURL string) (*http.Client, error) {
	transport := &http.Transport{
		// SSE 流式转发需要禁用内部缓冲相关行为并保持长连接
		DisableCompression:  false,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		ForceAttemptHTTP2:   false, // 上游经代理时 h2 直连无意义，SSE 1.1 足够
	}

	if proxyURL == "" {
		return &http.Client{Transport: transport}, nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}

	switch {
	case u.Scheme == "http" || u.Scheme == "https":
		transport.Proxy = http.ProxyURL(u)
	case u.Scheme == "socks5" || u.Scheme == "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pass, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pass}
		}
		// 注意 host 是代理地址本身，不是目标地址。
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer: %w", err)
		}
		transport.DialContext = socksDialContext(dialer, u.Scheme == "socks5")
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (want http/https/socks5/socks5h)", strings.TrimSuffix(u.Scheme, "://"))
	}

	return &http.Client{Transport: transport}, nil
}

// socksDialContext 把 SOCKS5 dialer 适配为 http.Transport 需要的
// DialContext。resolveLocally 为 true（socks5://）时先在本地解析目标域名
// 为 IP 再交给代理；为 false（socks5h://）时把域名原样交给代理远端解析。
func socksDialContext(dialer proxy.Dialer, resolveLocally bool) func(context.Context, string, string) (net.Conn, error) {
	ctxDialer, _ := dialer.(proxy.ContextDialer)

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if ctxDialer != nil {
			return ctxDialer.DialContext(ctx, network, addr)
		}
		return dialer.Dial(network, addr)
	}

	if !resolveLocally {
		return dial
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		// 已是 IP 则不重复解析；域名按 GOOS 默认解析器解析
		if net.ParseIP(host) == nil {
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve %q locally for socks5: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("resolve %q locally for socks5: no address", host)
			}
			host = ips[0].IP.String()
		}
		return dial(ctx, network, net.JoinHostPort(host, port))
	}
}
