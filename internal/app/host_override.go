package app

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
)

// parseHostOverrides 解析 "host1=ip1,host2=ip2" 格式的域名→IP 覆盖映射。
// 空串返回 nil；非空配置必须全部合法，配置错误由调用方 fail-fast。
func parseHostOverrides(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	result := make(map[string]string)
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" { // 容忍尾随/连续逗号产生的空条目
			continue
		}
		host, ip, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("invalid CCLOAD_HOST_OVERRIDES entry %q: want host=ip", entry)
		}
		host = strings.TrimSpace(host)
		ip = strings.TrimSpace(ip)
		if host == "" || ip == "" {
			return nil, fmt.Errorf("invalid CCLOAD_HOST_OVERRIDES entry %q: host and ip are required", entry)
		}
		if net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("invalid CCLOAD_HOST_OVERRIDES entry %q: %q is not an IP address", entry, ip)
		}
		result[host] = ip
	}

	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
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
