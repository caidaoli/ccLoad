package app

import (
	"context"
	"fmt"
	"net"
	neturl "net/url"

	"golang.org/x/net/proxy"
)

// newSOCKS5Dialer 从 socks5:// URL 构建 DialContext 函数。
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

	return cd.DialContext, nil
}
