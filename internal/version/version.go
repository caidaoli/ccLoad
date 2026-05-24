// Package version 提供应用版本信息
// 版本号通过 go build -ldflags 注入，用于静态资源缓存控制
package version

// 构建信息变量，通过 ldflags 注入
// 构建命令示例:
//
//	go build -ldflags "-X ccLoad/internal/version.Version=$(git describe --tags --always) \
//	  -X ccLoad/internal/version.Commit=$(git rev-parse --short HEAD) \
//	  -X 'ccLoad/internal/version.BuildTime=$(date +%Y-%m-%d\ %H:%M:%S\ %z)' \
//	  -X ccLoad/internal/version.BuiltBy=$(whoami)"
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
	BuiltBy   = "unknown"
)

// OutboundUserAgent 返回 ccLoad 自身发出请求时使用的默认 User-Agent。
// 用于版本检查、渠道健康检测（仅当 Tester 未显式伪装客户端时）等出站请求。
func OutboundUserAgent() string {
	return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) ccLoad/" + Version + " Chrome/146.0.7680.188 Electron/41.2.1 Safari/537.36"
}
