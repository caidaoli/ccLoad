// Package version 提供应用版本信息
// 版本号通过 go build -ldflags 注入，用于静态资源缓存控制
package version

// Version 应用版本号，构建时通过 ldflags 注入
// 默认值 "dev" 用于开发环境
// 构建命令: go build -ldflags "-X ccLoad/internal/version.Version=$(git describe --tags --always)"
var Version = "dev"
