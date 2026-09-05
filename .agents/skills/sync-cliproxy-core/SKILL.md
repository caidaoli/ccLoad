---
name: sync-cliproxy-core
description: 同步或审计 ccLoad 的 CLIProxyAPI 核心与已登记 provider adapter 快照。适用于上游版本对齐，不用于普通渠道配置或独立业务修复。
---

# 同步 CLIProxy 转换核心与渠道适配器

一次同步 CLIProxyAPI 的四协议纯转换核心、已登记 provider adapters 及其对应测试。保持 ccLoad Registry 和 provider wire 契约，不引入上游运行时系统。

阅读或编辑本技能本身不执行同步。仅审计时固定比较目标、核对来源与 manifest、报告差异和验证结果，不进入集成变更或更新来源步骤。

仅审计当前快照且未指定比较目标时，使用 `UPSTREAM.md` 已记录的 commit；要求对比最新或指定版本时才解析相应目标。下方自动选择最新稳定版的规则适用于同步请求。

默认调用 `$sync-cliproxy-core` 时，自动固定上游最新稳定版本，并在同一次操作中完成 core 与全部已登记 provider adapters 的比较、集成、来源更新和验证。不要把 provider adapters 留给第二次同步。用户明确指定 commit/tag 时使用指定目标；用户明确要求仅审计时保持只读。

## 原子同步契约

- core 与 provider adapters 必须来自同一 checkout、同一不可变 commit。
- 只生成一份变化清单，只更新一次来源 commit 和同步日期，只运行一套完成验证。
- 任一同步域无法移植、缺少匹配测试或验证失败时，整个同步未完成；不得只更新 core 后声称成功。
- 不提供隐式 core-only 降级。用户明确缩小范围时可以只审计，但不得把部分写入伪装成完整同步。

## 权威边界

1. 按任务读取 [协议转换边界](../../../.claude/agent-guide/protocol.md)、`internal/protocol/cliproxy/UPSTREAM.md`、[core snapshot manifest](references/core-snapshot.manifest)、[provider adapter 语义边界](references/provider-adapters.md)和[provider manifest](references/provider-adapters.manifest)。`UPSTREAM.md` 是来源、固定提交和已落地状态的唯一事实源；core manifest 记录直接映射根、特殊映射、明确排除和最近一次 core/provider 原子差异的审查 blob，provider manifest 是 provider、逐文件映射、生产接线和契约测试的唯一 allowlist。
2. `internal/protocol/registry.go` 定义四协议契约；`internal/protocol/builtin/cliproxy_adapter.go` 处理通用输入验证、JSON/SSE 规范化和流帧封装；`internal/protocol/cliproxy/providers/` 保存 provider-specific 纯转换。
3. provider 选择留在 ccLoad 请求上下文边界，按实际 wire dialect/AuthType 决定；不要把 provider 注册成第五种通用客户端协议。
4. 不引入 CLIProxyAPI 的认证、配置、路由、缓存服务、插件、动态 Registry、executor 或网络刷新代码。Interactions 只有在 ccLoad 正式支持其线协议后才可登记。
5. 不添加 CLIProxyAPI 运行时 Go module 依赖或 `replace`。源码继续使用 `ccLoad/internal/protocol/cliproxy/...` 导入路径。
6. Registry 与 provider 边界测试是 ccLoad 兼容性权威。上游行为与本地线协议冲突时，修正根因并保留本地契约，不盲目覆盖。

## 同步流程

### 1. 预检

- 确认当前目录属于 ccLoad，并检查 `git status --short`。
- 保护已有修改。若工作区改动覆盖 core、providers、适配器、Registry、`go.mod`、`go.sum` 或 `UPSTREAM.md`，先区分用户修改与本次同步；无法安全隔离时停止并说明冲突。
- 记录当前同步 commit 和目标 commit。

### 2. 固定目标

- 用户指定 commit/tag 时，解析成完整不可变 commit SHA。
- 用户未指定目标或只说“同步最新”时，自动查询 `UPSTREAM.md` 记录仓库的远端 tags。以当前记录 tag 的非版本前缀和 `vMAJOR.MINOR.PATCH` 形状确定稳定 tag 系列，按语义版本选择最高版本；忽略预发布 tag，禁止按字典序或提交时间猜版本。
- 将选中的 tag 解引用为完整 commit SHA（等价于 `<tag>^{commit}`）。记录并同步该 commit，而不是 annotated tag object；报告目标和变化范围后直接继续，不等待确认。
- 若无法从当前记录确定 tag 系列、找不到稳定 tag，或 tag 无法解析为 commit，停止并说明原因。禁止退回到浮动分支 HEAD。
- 若目标 SHA 与当前同步 SHA 相同，不改写 `UPSTREAM.md` 的同步日期；运行确定性审计和验证后报告已是最新版本。
- 使用现有上游 checkout，或在临时目录克隆 `UPSTREAM.md` 记录的仓库。core、provider 生产源码与测试必须全部来自这个 checkout 的同一个 commit。

### 3. 比较范围

- 一次生成目标 commit 相对当前记录 commit 的联合差异：四协议 core、allowlist 中每个 provider 的生产源码及对应 `_test.go`，不要分两次比较。
- 先生成目录级和文件级变化清单，再确认新增文件是纯转换语义。provider 的 `init.go`、动态 Registry 和 noop/分配实现测试按 allowlist 排除。
- 每个上游 core 变更必须由 core manifest 分类为直接映射、特殊映射、明确删除、明确排除或已登记 provider，并为本次所有非排除 core/provider 差异刷新 review blob。新增 core 源根或本地特例时更新 core manifest；上游删除已同步文件时登记 `delete` 行并删除本地映射；新增 provider 时更新 provider manifest、语义边界和 `UPSTREAM.md`。脚本只从 manifest 读取这些清单，不复制第二份。审计失败不能靠跳过检查解决。
- 明确列出排除的上游包。不要因为编译缺失就搬入 runtime；删除副作用依赖，或在同步包内用已有纯 `common`/`signature`/`util` 能力替代。
- 上游删除字段注入时，确认行为是否迁移到被排除的 runtime 层；若是，必须在本地转换边界保留，并用 Registry 契约测试验证。
- 复查 `UPSTREAM.md` 已排除项的排除理由是否仍成立：上游重构可能使旧理由失效（该同步的补回来），也可能采纳了本地契约（删掉过期的本地差异注记）。

### 4. 集成变更

- 以可审查的小批次应用源码，但把所有批次视为一个原子同步结果。
- 将上游 import 改为本地 `ccLoad/internal/protocol/cliproxy/...`。
- 将通用传输适配留在 `builtin/cliproxy_adapter.go`，provider envelope/字段补全留在 `providers/<provider>`；只有通用协议语义需要时才修改 core。
- 确保每个同步的 provider adapter 被生产请求路径实际调用，不能只复制成死代码。
- 对工具调用 ID、reasoning/signature、usage/cache、JSON 字段形状、错误 envelope、SSE framing/终止事件和跨 chunk 状态逐项核对 core 与 provider 契约。
- 无法表示的客户端请求继续返回 `RequestTranslationError`，由代理映射为 HTTP 400；不得触发渠道切换或冷却。

### 5. 更新来源记录

- 只有 core 和全部已登记 provider 的生产源码、对应测试、生产接线都完成后，才一次性更新 `internal/protocol/cliproxy/UPSTREAM.md` 的完整 commit、标签说明和同步日期。
- 在 `UPSTREAM.md` 分别记录 core 与 provider 的上游源目录、本地目录和实际落地状态；逐文件事实和本次差异审查 blob 只保留在 manifest。它们共享同一个 commit/date，不在 manifest 维护第二套版本号。
- 保留 `internal/protocol/cliproxy/LICENSE`。许可证或上游归属变化必须显式审查。
- 不在 Skill 中复制 commit、日期或测试数量；这些易变事实只写入 `UPSTREAM.md`。

### 6. 验证

先运行确定性审计：

```bash
bash .agents/skills/sync-cliproxy-core/scripts/verify_core_scope.sh --self-test
bash .agents/skills/sync-cliproxy-core/scripts/verify.sh --tests --require-providers \
  --upstream-repo /path/to/CLIProxyAPI \
  --base-commit <previous-synchronized-commit>
```

`--require-providers` 是完整同步的完成门禁：它同时要求 `--upstream-repo` 和完整的 `--base-commit`。该 base 必须等于 Git `HEAD` 中修改前 `UPSTREAM.md` 记录的同步 SHA，调用者不能跳过较早变更；脚本随后强制 base 到工作区 `UPSTREAM.md` 目标提交的每个 core/provider 变更都已映射、删除或明确排除。若 base 已等于目标，说明快照本来就是最新版本，脚本改做目标树、review blob 和 provider 的确定性审计，不伪造空同步差异。任一 allowlist provider 尚未落地、review blob 陈旧或出现未知上游文件时必须失败。日常审计历史快照可以省略这些参数，但不得据此报告完整同步成功。

完成同步后运行仓库级检查；只读审计根据实际审计范围选择相关检查：

```bash
go test -tags sonic ./internal/...
make build
golangci-lint run ./...
git diff --check
```

只在并发相关代码受影响时运行 `make race-fast` 或 `make race`。根据最终差异更新 `.claude/agent-guide/` 中受影响的专题及必要的 `README.md` / `README.zh-CN.md`；只有通用约束或入口变化才修改 `CLAUDE.md`。

## 完成报告

报告以下事实：

- 原同步 commit、目标 commit 和上游 checkout；
- 同一次同步覆盖的 core、provider 目录/文件与明确排除的上游模块；
- 为维持 ccLoad Registry/provider wire contract 保留或新增的本地差异；
- `UPSTREAM.md`、许可证和相关项目文档是否更新；
- 每条验证命令的结果。
