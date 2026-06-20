package app

import (
	"context"
	"log"
	"net"
	"strings"
)

// parseHostOverrides 解析 "host1=ip1,host2=ip2" 格式的域名→IP 覆盖映射。
// 跳过无 '=' 的条目，以及 value 不是合法 IP 的条目（含 "x=y=z" 脏数据）。空串返回 nil。
func parseHostOverrides(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	result := make(map[string]string)
	for entry := range strings.SplitSeq(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), "=", 2)
		if len(parts) != 2 {
			continue
		}
		host := strings.TrimSpace(parts[0])
		ip := strings.TrimSpace(parts[1])
		if host == "" || ip == "" {
			continue
		}
		// value 必须是合法 IP：否则会被当作域名再走一次系统 DNS，静默吞掉配置错误。
		// net.ParseIP 同时拒绝 "x=y=z" 这类多等号脏数据，无需单独判 '='。
		if net.ParseIP(ip) == nil {
			log.Printf("[WARN] CCLOAD_HOST_OVERRIDES 跳过无效 IP: %q=%q", host, ip)
			continue
		}
		result[host] = ip
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// dialContextFunc 是 net.Dialer.DialContext 的函数签名。
type dialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// wrapDialerWithHostOverrides 包装 DialContext，将命中覆盖表的域名替换为指定 IP，
// 保留原端口号。TLS SNI/证书校验/Host 头不受影响（它们使用 URL 的 host，不经过 dialer）。
// overrides 为 nil 或空时直接返回原始 dialer。
func wrapDialerWithHostOverrides(dial dialContextFunc, overrides map[string]string) dialContextFunc {
	if len(overrides) == 0 {
		return dial
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return dial(ctx, network, addr)
		}
		if ip, ok := overrides[host]; ok {
			addr = net.JoinHostPort(ip, port)
		}
		return dial(ctx, network, addr)
	}
}

// logHostOverrides 启动时记录生效的覆盖条目。
func logHostOverrides(overrides map[string]string) {
	if len(overrides) == 0 {
		return
	}
	for host, ip := range overrides {
		log.Printf("[INFO] DNS覆盖: %s → %s", host, ip)
	}
}
