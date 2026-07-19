---
name: sync-cliproxy-core
description: Synchronize or audit ccLoad's in-tree CLIProxyAPI/cliproxy protocol conversion snapshot from an immutable upstream commit. Invoking without a target automatically resolves and synchronizes the latest stable upstream version. Use when asked to 同步、更新或升级 CLIProxyAPI、cliproxy、translator core、internal/protocol/cliproxy，刷新上游 commit，或审查一次协议核心同步。
---

# 同步 CLIProxy 转换核心

只同步 CLIProxyAPI 的纯协议转换代码和对应测试。保持 ccLoad Registry 线协议契约，不引入上游运行时系统。

默认调用 `$sync-cliproxy-core` 时，自动同步上游最新稳定版本并完成验证，不等待目标确认。用户明确指定 commit/tag 时使用指定目标；用户明确要求仅审计时保持只读。

## 权威边界

1. 先读仓库根目录 `CLAUDE.md` 和 `internal/protocol/cliproxy/UPSTREAM.md`。后者是来源、固定提交、排除范围和本地契约的唯一事实源。
2. `internal/protocol/registry.go` 定义 ccLoad 契约；`internal/protocol/builtin/cliproxy_adapter.go` 处理 ccLoad 输入验证、JSON/SSE 规范化和流帧封装；`internal/protocol/cliproxy/` 保存同步的纯转换核心。
3. 不引入 CLIProxyAPI 的认证、配置、路由、缓存、插件、动态 Registry、网络刷新、Antigravity 或 Interactions 代码。
4. 不添加 CLIProxyAPI 运行时 Go module 依赖或 `replace`。源码继续使用 `ccLoad/internal/protocol/cliproxy/...` 导入路径。
5. Registry 边界测试是 ccLoad 兼容性权威。上游行为与本地线协议冲突时，修正根因并保留本地契约，不盲目覆盖。

## 同步流程

### 1. 预检

- 确认当前目录属于 ccLoad，并检查 `git status --short`。
- 保护已有修改。若工作区改动覆盖协议目录、适配器、Registry、`go.mod`、`go.sum` 或 `UPSTREAM.md`，先区分用户修改与本次同步；无法安全隔离时停止并说明冲突。
- 记录当前同步 commit 和目标 commit。

### 2. 固定目标

- 用户指定 commit/tag 时，解析成完整不可变 commit SHA。
- 用户未指定目标或只说“同步最新”时，自动查询 `UPSTREAM.md` 记录仓库的远端 tags。以当前记录 tag 的非版本前缀和 `vMAJOR.MINOR.PATCH` 形状确定稳定 tag 系列，按语义版本选择最高版本；忽略预发布 tag，禁止按字典序或提交时间猜版本。
- 将选中的 tag 解引用为完整 commit SHA（等价于 `<tag>^{commit}`）。记录并同步该 commit，而不是 annotated tag object；报告目标和变化范围后直接继续，不等待确认。
- 若无法从当前记录确定 tag 系列、找不到稳定 tag，或 tag 无法解析为 commit，停止并说明原因。禁止退回到浮动分支 HEAD。
- 若目标 SHA 与当前同步 SHA 相同，不改写 `UPSTREAM.md` 的同步日期；运行确定性审计和验证后报告已是最新版本。
- 使用现有上游 checkout，或在临时目录克隆 `UPSTREAM.md` 记录的仓库。所有生产源码与测试必须来自同一个 checkout 和同一个 commit。

### 3. 比较范围

- 同时比较纯转换生产源码和对应 `_test.go`，不要只看生产文件。
- 先生成目标 commit 相对当前记录 commit 的目录级和文件级差异，再确认每个新增目录确属纯转换核心。
- 新增纯转换顶层目录时，同步更新 `scripts/verify.sh` 的显式允许列表；审计失败不能靠跳过检查解决。
- 明确列出排除的上游包。不要因为编译缺失就把上游运行时依赖一起搬入；在转换核心或 ccLoad 适配边界消除依赖。

### 4. 集成变更

- 以小批次应用转换源码和匹配测试，保持来源可审查。
- 将上游 import 改为本地 `ccLoad/internal/protocol/cliproxy/...`。
- 将 ccLoad 特有的传输适配留在 `builtin/cliproxy_adapter.go`；只有协议语义本身需要时才修改同步核心。
- 对工具调用、reasoning/signature、usage、JSON 字段形状、SSE framing/终止事件和跨 chunk 状态逐项核对 Registry 契约。
- 无法表示的客户端请求继续返回 `RequestTranslationError`，由代理映射为 HTTP 400；不得触发渠道切换或冷却。

### 5. 更新来源记录

- 只有在生产源码和对应测试都完成集成后，才更新 `internal/protocol/cliproxy/UPSTREAM.md` 的完整 commit、标签说明和同步日期。
- 保留 `internal/protocol/cliproxy/LICENSE`。许可证或上游归属变化必须显式审查。
- 不在 Skill 中复制 commit、日期或测试数量；这些易变事实只写入 `UPSTREAM.md`。

### 6. 验证

先运行确定性审计：

```bash
bash .agents/skills/sync-cliproxy-core/scripts/verify.sh --tests --upstream-repo /path/to/CLIProxyAPI
```

再运行仓库级检查：

```bash
go test -tags sonic ./internal/...
make build
golangci-lint run ./...
git diff --check
```

只在并发相关代码受影响时运行 `make race-fast` 或 `make race`。根据最终差异排查是否需要同步更新 `CLAUDE.md`、`README.md` 和 `README.zh-CN.md`。

## 完成报告

报告以下事实：

- 原同步 commit、目标 commit 和上游 checkout；
- 同步的目录/文件与明确排除的上游模块；
- 为维持 ccLoad wire contract 保留或新增的本地差异；
- `UPSTREAM.md`、许可证和三份项目文档是否更新；
- 每条验证命令的结果。
