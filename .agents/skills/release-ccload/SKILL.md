---
name: release-ccload
description: 用于发布 ccLoad 新版本、计算下一个语义版本、创建并推送发布 Tag、等待 GitHub Actions，以及验证 GitHub Release 和稳定版容器镜像。默认发布 Beta；只有用户明确传入 stable 时才发布稳定版。
---

# 发布 ccLoad

通过唯一的 Tag 驱动 `.github/workflows/release.yml`。不要手动创建 Release、手动触发发布工作流或单独发布容器镜像。

## 参数契约

- `$release-ccload`、`/release-ccload`、`release-ccload beta`、`release-ccload preview`：发布下一个 Beta。
- `release-ccload stable`：发布稳定版。必须有显式 `stable` 参数；禁止根据语气猜测。
- 其他参数：停止并报告只支持 `beta`、`preview`、`stable`。

Tag 形状固定：

- Beta：`vX.Y.Z-beta.N`
- 稳定版：`vX.Y.Z`

## 发布流程

1. 从仓库根目录运行预览。默认渠道是 `beta`：

   ```bash
   bash .agents/skills/release-ccload/scripts/release.sh beta --dry-run
   ```

   稳定版改为：

   ```bash
   bash .agents/skills/release-ccload/scripts/release.sh stable --dry-run
   ```

2. 核对脚本输出的上一稳定版、语义版本增量和目标 Tag。用户调用本 Skill 已经是发布授权；目标符合参数契约时直接继续，不重复询问。

3. 执行发布：

   ```bash
   bash .agents/skills/release-ccload/scripts/release.sh beta --publish
   ```

   稳定版使用 `stable --publish`。

4. 报告目标 Tag、GitHub Release URL、Actions 结果。稳定版还要报告 `ghcr.io/caidaoli/ccload:<tag>` 和 `ghcr.io/caidaoli/ccload:latest`；Beta 明确说明未发布容器。

## 强制规则

- 工作区必须干净，当前分支必须是 `master`，本地 `HEAD` 必须等于 `origin/master`。
- 脚本只创建和推送 annotated Tag；不提交、不推送分支、不修改版本文件。
- 发布前必须通过后端测试、Web 验证、构建和 lint。任一失败都不得创建 Tag。
- Beta Release 必须是 prerelease 且不得成为 latest；稳定版 Release 必须成为 latest。
- 只有稳定版发布 GHCR，并且镜像必须打精确版本 Tag 和 `latest`。
- 发布失败后保留现场并报告失败的 Tag/Actions URL。不要自动删 Tag、Release 或镜像；回滚必须由用户另行明确授权。
- 不绕过 `.github/workflows/release.yml` 的 Tag 校验，也不创建 `beta`、`latest` 这类浮动发布 Tag。

## 脚本自检

修改发布脚本后运行：

```bash
bash .agents/skills/release-ccload/scripts/release.sh --self-test
```
