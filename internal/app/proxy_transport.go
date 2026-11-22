package app

import (
	"context"
	"fmt"
	"net"
	neturl "net/url"

	"golang.org/x/net/proxy"
)

/**
 * @brief 保留需要限制 SOCKS5 握手生命周期的任务上下文。
 * @details net/http 为复用连接会从拨号上下文移除请求的截止时间和取消信号，但保留值。
 */
type socks5DialTaskContextKey struct{}

/**
 * @brief 从 SOCKS5 代理 URL 构建拨号函数。
 * @param u 代理地址及可选认证信息。
 * @return 代理拨号函数；保留任务上下文时，其结束会中止当前握手。
 * @return 初始化代理失败时返回错误。
 */
func newSOCKS5Dialer(u *neturl.URL) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	var auth *proxy.Auth
	if u.User != nil {
		auth = &proxy.Auth{User: u.User.Username()}
		if p, ok := u.User.Password(); ok {
			auth.Password = p
		}
	}

	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "1080")
	}

	d, err := proxy.SOCKS5("tcp", host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("create socks5 dialer: %w", err)
	}

	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("socks5 dialer does not support DialContext")
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if task, ok := ctx.Value(socks5DialTaskContextKey{}).(context.Context); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			stop := context.AfterFunc(task, cancel)
			defer stop()
		}
		return cd.DialContext(ctx, network, addr)
	}, nil
}
