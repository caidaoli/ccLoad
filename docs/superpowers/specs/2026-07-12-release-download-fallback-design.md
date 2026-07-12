# Release 下载源回退设计

## 根因

Docker 入口脚本和 Go 版本模块各自维护一个下载地址。Docker 只能从单一 `CCLOAD_RELEASE_BASE_URL` 下载，Go 自动更新又从 latest 重定向后的 HTML URL 反推资源地址。后者遇到 `ghproxy.net/https://github.com/...` 这类前缀代理时会破坏内嵌 URL 的双斜杠；两套实现也无法保证版本检测、校验文件和二进制下载按同一来源整体切换。

## 目标行为

- 未设置 `CCLOAD_RELEASE_BASE_URL` 时，依次使用 `ghproxy.net` 和 GitHub 直连。
- 一个来源只有在版本检测、`checksums.txt` 下载、二进制下载和 SHA256 校验全部成功时才算成功；任一环节失败都从下一个来源重新开始。
- 来源成功返回的版本不高于当前版本时立即停止，不因“没有新版本”而回退。
- 显式设置 `CCLOAD_RELEASE_BASE_URL` 时只使用该来源，不追加内置回退源。
- Docker 启动下载和 Go 内置版本检测、自动更新使用相同来源顺序与自定义源语义。
- 不设置 `HTTP_PROXY` 或 `HTTPS_PROXY`，业务渠道请求不受更新下载策略影响。

## Go 设计

在 `internal/version` 中引入显式 `ReleaseSource`：

```go
type ReleaseSource struct {
	Name            string
	LatestURL       string
	DownloadBaseURL string
}
```

默认来源固定为：

1. `ghproxy.net`
   - Latest: `https://ghproxy.net/https://github.com/caidaoli/ccLoad/releases/latest`
   - Download base: `https://ghproxy.net/https://github.com/caidaoli/ccLoad/releases/download`
2. `github.com`
   - Latest: `https://github.com/caidaoli/ccLoad/releases/latest`
   - Download base: `https://github.com/caidaoli/ccLoad/releases/download`

`CCLOAD_RELEASE_BASE_URL` 继续采用现有 Docker 契约：值必须指向 `.../releases/latest/download`。Go 侧从该值明确推导 `.../releases/latest` 和 `.../releases/download`，不通过重定向 URL 反推下载根地址。资源 URL 使用字符串边界拼接，保留代理前缀中的 `https://github.com`。

`AutoUpdater` 按来源执行完整事务。Checker 只需要版本检测，因此按来源尝试 latest；成功后公开最终 release URL。测试通过显式 sources 注入本地 HTTP 服务，不依赖公网。

## Docker 设计

将工作流 heredoc 中的入口脚本提取为 `docker/entrypoint.sh`。默认情况下脚本生成两个 latest-download 基础地址并顺序尝试；自定义变量存在时只生成一个地址。每次尝试使用独立临时目录，下载二进制和校验文件、验证 SHA256 后才原子替换现有程序。

工作流直接复制仓库内脚本到临时构建上下文，并在构建镜像前运行 `docker/entrypoint_test.sh`。行为测试以假 `curl`、`uname` 和测试二进制验证来源顺序、失败回退、自定义源不回退以及校验失败不覆盖现有程序。

## 错误处理

- 每个失败日志带来源名，最终错误聚合所有尝试失败原因。
- 自定义基础地址格式错误时 fail-fast，不静默回到内置来源。
- Docker 有旧二进制时，所有来源失败后继续使用旧程序；首次启动无二进制时退出。
- Go 自动更新只有成功校验并替换后才设置 pending restart。

## 验证

- `go test -tags sonic ./internal/version`
- `go test -tags sonic ./internal/app`
- `sh docker/entrypoint_test.sh`
- `go test -tags sonic ./internal/...`
- `make verify-web`
- `golangci-lint run ./...`
- `git diff --check`
