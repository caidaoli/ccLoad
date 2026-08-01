# GitHub Release

**Goal:** 从 `master` 的待发布提交推断下一个语义版本，创建并推送唯一的版本 Tag，触发 GitHub Actions 发布。
**Why planning is required:** 推送分支和 Tag 会改变远端状态并触发跨系统发布流程。
**Acceptance:** 推送前确认工作区内容已理解并提交、`master` 不落后于 `origin/master`、目标 Tag 在本地和远端均不存在、验证通过；推送后确认分支提交与 Tag 已存在于 `origin`。若推送失败且 Tag 仅存在于本地，删除本次创建的本地 Tag；禁止覆盖 Tag、强推、拉取、变基或合并。

### Outcome 1: 发布输入确定
- Work: 审查工作区和 `v3.17.3..HEAD` 的全部非 Merge 提交，按最高语义等级计算唯一的新版本。
- Verify: `git status --short --branch && git log v3.17.3..HEAD --no-merges --pretty=fuller`

### Outcome 2: 待发布修订可交付
- Work: 提交本次发布前的所有已理解改动，并对最终提交运行仓库要求的构建、测试和 lint 验证。
- Verify: `make build && go test -tags sonic ./internal/... && make verify-web && golangci-lint run ./...`

### Outcome 3: 分支与版本 Tag 已发布
- Work: 创建带注释且不覆盖的版本 Tag，并原子化推送当前 `master` 和该 Tag 到 `origin`；若远端不支持原子推送则停止，不降级为可能产生部分状态的普通推送。
- Risks/open questions: GitHub Actions 的最终发布结果属于推送后的异步外部状态，必须单独查询并明确报告可见状态。
- Verify: `git ls-remote --heads --tags origin master refs/tags/<new-tag> refs/tags/<new-tag>^{}`
