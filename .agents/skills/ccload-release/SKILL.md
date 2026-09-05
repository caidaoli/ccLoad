---
name: ccload-release
description: 发布 ccLoad Beta 或显式 stable 版本，完成提交、推送、CI、Release 和镜像验证；仅检查或修改发布流程时不执行发布。
---

# 发布 ccLoad

通过唯一的 Tag 驱动 `.github/workflows/release.yml`。不要手动创建 Release、手动触发发布工作流或单独发布容器镜像。

只要求计算版本或预检时，执行下方对应的 dry-run 并报告结果，不进入 publish。用户要求发布或单独调用本技能（可带通道参数）才适用自动提交、推送和发布授权；编辑技能文件不执行发布。

## 参数契约

- `$ccload-release`、`/ccload-release`、`ccload-release beta`、`ccload-release preview`：发布下一个 Beta。
- `ccload-release stable`：发布稳定版。必须有显式 `stable` 参数；禁止根据语气猜测。
- 其他参数：停止并报告只支持 `beta`、`preview`、`stable`。

Tag 形状固定：

- Beta：`vX.Y.Z-beta.N`
- 稳定版：`vX.Y.Z`

## 版本规则

- Beta 必须锁定最近稳定版 `vX.Y.S` 的主版本 `X` 和次版本 `Y`，禁止因 `feat` 或 breaking change 生成 `vX.(Y+1).0-beta.N` 或 `v(X+1).0.0-beta.N`。
- 最近稳定版之后没有有效 Beta 时，发布 `vX.Y.(S+1)-beta.1`。
- 已有 `vX.Y.P-beta.N` 时，只检查该 Beta Tag 之后的新提交：
  - 小修改（没有 `feat`、`!` 或 `BREAKING CHANGE`）保持 `X.Y.P`，发布 `vX.Y.P-beta.(N+1)`。
  - 大修改（存在 `feat`、`!` 或 `BREAKING CHANGE`）只把 patch 加一并重置序号，发布 `vX.Y.(P+1)-beta.1`。
- 只有显式 `stable` 发布才从最近稳定版后的全部提交计算标准 SemVer：breaking change 增加主版本，`feat` 增加次版本，其余增加 patch。Beta 永远不修改主版本或次版本。
- 最近稳定版之后如果已存在修改主版本或次版本的可达 Beta Tag，立即停止发布并报告非法 Tag；禁止继续制造互相冲突的预览版本。

## 发布流程

1. 检查 `git status` 和完整 diff。工作区有改动时，根据实际改动推导一个单行 Conventional Commit subject；必须准确反映最高语义版本影响，禁止使用掩盖功能变更的通用 `chore`。不要为提交或推送重复询问用户。

2. 从仓库根目录运行预览。默认渠道是 `beta`。工作区干净时运行：

   ```bash
   bash .agents/skills/ccload-release/scripts/release.sh beta --dry-run
   ```

   工作区有改动时传入推导出的提交 subject：

   ```bash
   bash .agents/skills/ccload-release/scripts/release.sh beta --dry-run --commit-message 'feat(scope): summary'
   ```

   稳定版把 `beta` 改为 `stable`。

3. 核对脚本输出的上一稳定版、上一有效 Beta、版本动作、目标 Tag、工作区动作和分支动作。用户调用本 Skill 已经授权自动提交、非强制推送 `master` 和发布；目标符合参数契约时直接继续。

4. 执行发布。工作区干净时运行：

   ```bash
   bash .agents/skills/ccload-release/scripts/release.sh beta --publish
   ```

   工作区有改动时必须复用 dry-run 的同一个 subject：

   ```bash
   bash .agents/skills/ccload-release/scripts/release.sh beta --publish --commit-message 'feat(scope): summary'
   ```

   稳定版把 `beta` 改为 `stable`。脚本会自动 `git add -A`、创建提交、非强制推送 `master`，等待该提交对应的 `Test` workflow 全部成功，确认远端仍精确一致后再创建并推送 annotated Tag。本地已有未推送提交时不创建额外提交，直接推送并等待 CI。

5. 报告自动创建的提交（如有）、推送的 `master` 修订、目标 Tag、GitHub Release URL 和 Actions 结果。稳定版报告 `ghcr.io/caidaoli/ccload:<tag>` 和 `ghcr.io/caidaoli/ccload:latest`（该稳定版为 SemVer 最高版本时 `beta` 别名同步指向它，一并报告）；Beta 报告 `ghcr.io/caidaoli/ccload:<tag>` 和 `ghcr.io/caidaoli/ccload:beta`。

## 强制规则

- 当前分支必须是 `master`。本地 `master` 可与 `origin/master` 相等或领先；远端领先或双方分叉时停止，禁止自动 pull、merge、rebase 或 force-push。
- Beta 的目标 Tag 必须遵守“固定最近稳定版主版本和次版本”的版本规则；脚本输出出现 minor/major Beta 增量时必须停止。
- dry-run 不提交、不推送、不打 Tag。publish 自动提交全部当前工作区改动；禁止 amend、拆改已有提交或修改版本文件。
- `Test` workflow 是提交级唯一发布门禁，必须覆盖后端测试、Web 验证、构建、lint 和 PostgreSQL 集成测试。发布脚本必须按完整 commit SHA 找到并等待该 workflow；找不到、被取消或失败时不得创建 Tag。
- 分支推送只能是普通 fast-forward push。推送后以及 `Test` workflow 成功后都必须重新 fetch 并确认本地 `HEAD` 等于 `origin/master`，才能创建 Tag。CI 失败时保留已经推送的提交，不自动回滚。
- Beta Release 必须是 prerelease 且不得成为 latest；稳定版 Release 必须成为 latest。
- 每个 Release 都发布 GHCR 多架构镜像。稳定版必须同时打精确版本 Tag 和 `latest`；Beta 必须同时打精确版本 Tag 和 `beta`；精确标签与对应浮动别名必须指向同一镜像摘要。`beta` 别名指向全渠道 SemVer 最高版本：稳定版发布时若不存在更高的 Beta Tag，`beta` 同步推进到该稳定版；存在更高 Beta 时保持不动，禁止降级。
- Release Notes 按发布通道比较：稳定版只比较上一个可达稳定版；新稳定版之后的首个 Beta 与该稳定版比较，后续 Beta 才与同一稳定版之后的上一个 Beta 比较。
- 分支或发布失败后保留现场并报告本地提交、失败的 Tag/Actions URL。不要自动删提交、Tag、Release 或镜像；回滚必须由用户另行明确授权。
- 不绕过 `.github/workflows/release.yml` 的 Tag 校验，也不创建 `beta`、`latest` 这类浮动 Git Tag；它们只允许作为 GHCR 镜像别名由发布工作流管理。

## 脚本自检

修改发布脚本后运行：

```bash
bash .agents/skills/ccload-release/scripts/release.sh --self-test
```
