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
