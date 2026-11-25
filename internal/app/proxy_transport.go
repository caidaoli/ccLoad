package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	neturl "net/url"
	"strings"

	"golang.org/x/net/proxy"
)

/**
 * @brief 解析渠道代理地址，失败时不回显可能含凭据的原始输入。
 * @param raw 渠道配置中的代理地址。
 * @return 带认证信息的实际连接地址；配置不合法时为 nil。
 * @return 地址格式无效或代理协议不支持时的诊断。
 */
func parseChannelProxyURL(raw string) (*neturl.URL, error) {
	u, err := neturl.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("invalid proxy_url: expected scheme://host[:port]")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return u, nil
	default:
		return nil, fmt.Errorf("invalid proxy_url scheme: %q (allowed: http, https, socks5, socks5h)", u.Scheme)
	}
}

/**
 * @brief 取可安全写入日志的渠道代理地址。
 * @param raw 渠道配置中的代理地址，可能包含认证信息。
 * @return 仅协议和主机（含端口）；无法安全解析时返回固定占位。
 */
func channelProxyURLForLog(raw string) string {
	u, err := parseChannelProxyURL(strings.TrimSpace(raw))
	if err != nil {
		return "<invalid proxy URL>"
	}
	return (&neturl.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

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
