package provider

import (
	"net/http"

	"zengateway/internal/store"
	"zengateway/internal/upstream"
)

// clientFor 按 key 的代理配置构建（并缓存）HTTP 客户端。
// 复用 internal/upstream 的实现，保证 http/socks5/socks5h 语义一致。
func clientFor(key store.APIKey) (*http.Client, error) {
	return upstream.ClientFor(key.Proxy)
}
