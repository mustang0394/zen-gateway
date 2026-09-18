// Package upstream 构建按目标 key 路由代理的 HTTP 客户端。
//
// 支持 http(s):// 与 socks5h:// 两类代理：
//   - http(s):// → http.Transport.Proxy（CONNECT 隧道）
//   - socks5h:// → x/net/proxy.SOCKS5；"socks5h" 表示域名解析也发生在代理端，
//     因此注册的 dialer 用域名直连代理即可，不本地解析。
package upstream

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// ClientFor 返回走指定代理的 HTTP 客户端；proxyURL 为空则直连。
// 客户端不设全局超时（SSE 长流），超时交给请求 context 控制。
func ClientFor(proxyURL string) (*http.Client, error) {
	transport := &http.Transport{
		// SSE 流式转发需要禁用内部缓冲相关行为并保持长连接
		DisableCompression:  false,
		MaxIdleConns:        64,
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
		// 注意 host 是代理地址本身（h 语义：目标域名不在这里解析）
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer: %w", err)
		}
		ctxDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			// 老 x/net 版本的兜底
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		} else {
			transport.DialContext = ctxDialer.DialContext
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (want http/https/socks5/socks5h)", strings.TrimSuffix(u.Scheme, "://"))
	}

	return &http.Client{Transport: transport}, nil
}
