package upstream

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
)

// fakeDialer 记录收到的目标地址，用于区分本地解析与远端解析。
type fakeDialer struct {
	gotAddr string
}

func (f *fakeDialer) Dial(network, addr string) (net.Conn, error) {
	f.gotAddr = addr
	return &stubConn{}, nil
}

func (f *fakeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	f.gotAddr = addr
	return &stubConn{}, nil
}

type stubConn struct{ net.Conn }

func (c *stubConn) Close() error { return nil }

// TestSocksDialContextResolveLocally 验证 socks5:// 在本地解析域名后再交给代理。
func TestSocksDialContextResolveLocally(t *testing.T) {
	f := &fakeDialer{}
	dial := socksDialContext(f, true)

	conn, err := dial(context.Background(), "tcp", "localhost:1080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()

	host, _, err := net.SplitHostPort(f.gotAddr)
	if err != nil {
		t.Fatalf("split %q: %v", f.gotAddr, err)
	}
	if net.ParseIP(host) == nil {
		t.Errorf("socks5 should pass a locally resolved IP, got %q", f.gotAddr)
	}
}

// TestSocksDialContextRemoteResolve 验证 socks5h:// 把域名原样交给代理解析。
func TestSocksDialContextRemoteResolve(t *testing.T) {
	f := &fakeDialer{}
	dial := socksDialContext(f, false)

	conn, err := dial(context.Background(), "tcp", "example.invalid:1080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()

	if f.gotAddr != "example.invalid:1080" {
		t.Errorf("socks5h must forward the hostname unchanged, got %q", f.gotAddr)
	}
}

// TestSocksDialContextKeepsIP 验证已有 IP 不再重复解析。
func TestSocksDialContextKeepsIP(t *testing.T) {
	f := &fakeDialer{}
	dial := socksDialContext(f, true)

	conn, err := dial(context.Background(), "tcp", "10.0.0.5:1080")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()

	if f.gotAddr != "10.0.0.5:1080" {
		t.Errorf("IP target should be passed through, got %q", f.gotAddr)
	}
}

// TestClientForCachesPerProxy 验证相同代理配置返回同一个客户端实例，
// 从而复用底层连接池（避免每个请求重新建连）。
func TestClientForCachesPerProxy(t *testing.T) {
	direct1, err := ClientFor("")
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	direct2, err := ClientFor("")
	if err != nil {
		t.Fatalf("direct again: %v", err)
	}
	if direct1 != direct2 {
		t.Error("direct client must be cached and reused")
	}

	a1, err := ClientFor("socks5h://user:pass@127.0.0.1:1080")
	if err != nil {
		t.Fatalf("socks5h: %v", err)
	}
	a2, err := ClientFor("socks5h://user:pass@127.0.0.1:1080")
	if err != nil {
		t.Fatalf("socks5h again: %v", err)
	}
	if a1 != a2 {
		t.Error("same proxy must reuse the cached client")
	}

	b, err := ClientFor("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("http proxy: %v", err)
	}
	if b == a1 || b == direct1 {
		t.Error("different proxies must use different clients")
	}

	// 空白代理串归一化为直连
	blank, err := ClientFor("   ")
	if err != nil {
		t.Fatalf("blank proxy: %v", err)
	}
	if blank != direct1 {
		t.Error("blank proxy must resolve to the direct client")
	}

	// 非法代理仍要报错，且不影响已缓存项
	if _, err := ClientFor("ftp://bad"); err == nil {
		t.Error("unsupported scheme must error")
	}
}

func TestClientForConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	results := make([]*http.Client, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := ClientFor("http://127.0.0.1:9999")
			if err != nil {
				t.Errorf("clientFor: %v", err)
				return
			}
			results[i] = c
		}(i)
	}
	wg.Wait()
	for i := 1; i < len(results); i++ {
		if results[i] != results[0] {
			t.Fatalf("concurrent ClientFor returned different instances")
		}
	}
}
