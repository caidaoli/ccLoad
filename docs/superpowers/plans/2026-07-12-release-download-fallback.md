# Release Download Fallback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Docker 启动下载、Go 版本检测和 Go 自动更新默认按 `ghproxy.net → GitHub` 回退，并让显式自定义来源保持单源行为。

**Architecture:** `internal/version` 用 `ReleaseSource` 明确表达 latest URL 与 download base，杜绝从代理重定向 URL 反推资源地址。Go 自动更新把单一来源的检测、下载和校验视为一个事务；Docker 入口脚本复用相同顺序和失败边界。

**Tech Stack:** Go 1.25、`net/http`、POSIX shell、curl、sha256sum、GitHub Actions

## Global Constraints

- 所有 Go 测试必须带 `-tags sonic`。
- 默认来源顺序必须是 `ghproxy.net` 后 GitHub 直连。
- 显式 `CCLOAD_RELEASE_BASE_URL` 时不得追加内置来源。
- 不设置全局 `HTTP_PROXY` 或 `HTTPS_PROXY`。
- 测试公共行为和模块边界，不测试 helper 委托或源码文本结构。

---

### Task 1: Go 来源建模与 URL 契约

**Files:**
- Modify: `internal/version/release_latest.go`
- Modify: `internal/version/updater_test.go`

**Interfaces:**
- Produces: `type ReleaseSource struct { Name, LatestURL, DownloadBaseURL string }`
- Produces: `releaseSources(customBaseURL string) ([]ReleaseSource, error)`
- Produces: `releaseDownloadURL(source ReleaseSource, tag, assetName string) (string, error)`

- [ ] **Step 1: Write failing tests**

覆盖默认来源顺序、自定义 `.../releases/latest/download` 只生成一个来源、非法自定义地址报错，以及 ghproxy 下载 URL 完整保留 `https://github.com`。

- [ ] **Step 2: Verify RED**

Run: `go test -tags sonic ./internal/version -run 'TestReleaseSources|TestReleaseDownloadURL'`

Expected: FAIL，因为 `ReleaseSource` 和新解析接口尚不存在。

- [ ] **Step 3: Implement minimal source parsing and URL construction**

用固定常量定义两个内置来源；自定义值去掉末尾 `/` 后必须以 `/releases/latest/download` 结尾，再推导 latest 与 download base。禁止使用 `url.JoinPath` 处理代理前缀。

- [ ] **Step 4: Verify GREEN**

Run: `go test -tags sonic ./internal/version -run 'TestReleaseSources|TestReleaseDownloadURL'`

Expected: PASS。

### Task 2: Go AutoUpdater 整体来源回退

**Files:**
- Modify: `internal/version/updater.go`
- Modify: `internal/version/updater_test.go`
- Modify: `internal/app/auto_update.go`

**Interfaces:**
- `AutoUpdateOptions.ReleaseSources []ReleaseSource` 用于显式注入测试或调用方来源。
- `AutoUpdater.updateOnce(ctx)` 按来源完成检测、下载、校验和替换。

- [ ] **Step 1: Write failing regression tests**

分别覆盖首来源 latest 失败后第二来源成功、首来源 checksum 或二进制失败后第二来源从头成功，以及所有来源失败时不替换可执行文件。

- [ ] **Step 2: Verify RED**

Run: `go test -tags sonic ./internal/version -run 'TestUpdateOnce.*Source'`

Expected: FAIL，现有 updater 只保存一个 latest URL。

- [ ] **Step 3: Implement source transaction fallback**

构造器默认从 `CCLOAD_RELEASE_BASE_URL` 解析来源；`updateOnce` 遍历 sources。检测到没有新版本立即返回；有新版本时基于当前 source 构造两个下载 URL，下载校验失败则继续下一个来源。只有成功替换后更新 pending state。

- [ ] **Step 4: Verify GREEN and existing updater behavior**

Run: `go test -tags sonic ./internal/version -run 'TestUpdateOnce|TestReleaseSources|TestReleaseDownloadURL'`

Expected: PASS。

### Task 3: Checker 使用相同来源策略

**Files:**
- Modify: `internal/version/checker.go`
- Modify: `internal/version/checker_additional_test.go`

**Interfaces:**
- `Checker.sources []ReleaseSource`；为空时解析环境变量和内置默认值。

- [ ] **Step 1: Write failing checker fallback test**

注入两个来源，Transport 让首来源失败、第二来源返回 tag，断言状态来自第二来源；再验证自定义来源解析只产生一个请求目标。

- [ ] **Step 2: Verify RED**

Run: `go test -tags sonic ./internal/version -run 'TestChecker.*Fallback'`

Expected: FAIL，Checker 当前硬编码 GitHub latest。

- [ ] **Step 3: Implement checker source traversal**

按顺序调用 `fetchLatestRelease`，首个成功结果更新状态；全部失败时保留旧状态并记录聚合错误。

- [ ] **Step 4: Verify GREEN**

Run: `go test -tags sonic ./internal/version`

Expected: PASS。

### Task 4: Docker 入口脚本回退

**Files:**
- Create: `docker/entrypoint.sh`
- Create: `docker/entrypoint_test.sh`
- Modify: `.github/workflows/docker.yml`

**Interfaces:**
- `CCLOAD_RELEASE_BASE_URL`: 可选 `.../releases/latest/download` 单源覆盖。
- 默认源：`https://ghproxy.net/https://github.com/${CCLOAD_REPO}/releases/latest/download`、`https://github.com/${CCLOAD_REPO}/releases/latest/download`。

- [ ] **Step 1: Write failing shell behavior test**

假 `curl` 根据 URL 返回成功或失败，测试默认顺序、下载失败回退、自定义源单次尝试、校验失败不覆盖旧二进制。

- [ ] **Step 2: Verify RED**

Run: `sh docker/entrypoint_test.sh`

Expected: FAIL，因为真实入口脚本尚不存在。

- [ ] **Step 3: Implement the extracted entrypoint**

每个来源使用独立临时目录并完成 binary、checksums、SHA256 校验后再移动；全部失败才使用旧二进制或退出。

- [ ] **Step 4: Make workflow copy and test the script**

工作流删除 entrypoint heredoc，改为 `cp docker/entrypoint.sh "$context_dir/entrypoint.sh"`，并在准备构建上下文前运行 shell 测试。

- [ ] **Step 5: Verify GREEN**

Run: `sh docker/entrypoint_test.sh`

Expected: PASS。

### Task 5: 文档与全量验证

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`

- [ ] **Step 1: Document exact behavior**

说明 Docker 首次启动和程序内自动更新默认使用 ghproxy 后 GitHub，`CCLOAD_RELEASE_BASE_URL` 的完整格式与单源覆盖语义，并明确该变量不是全局 HTTP 代理。

- [ ] **Step 2: Run full verification**

Run:

```bash
go test -tags sonic ./internal/version
go test -tags sonic ./internal/app
sh docker/entrypoint_test.sh
go test -tags sonic ./internal/...
make verify-web
golangci-lint run ./...
git diff --check
```

Expected: 所有命令退出码均为 0。

- [ ] **Step 3: Review and commit**

检查 `git diff --stat` 和 `git diff`，确认没有无关修改，然后使用 Conventional Commit 提交实现。
