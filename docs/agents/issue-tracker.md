# Issue tracker: Local Markdown

任务和规格保存在 `.scratch/` 中。

## 文件约定

- 每个功能一个目录：`.scratch/<feature-slug>/`。
- 规格：`spec.md`。
- 实施任务：`issues/<NN>-<slug>.md`，从 `01` 编号，每个任务独立文件。
- 在任务顶部用 `Status:` 记录 triage 状态，取值见 `triage-labels.md`。
- 评论和讨论追加到文件末尾的 `## Comments` 下。

## 技能操作

- “publish to the issue tracker”：按上述约定创建规格或任务文件，按需创建目录。
- “fetch the relevant ticket”：读取指定路径的任务；仅给出编号时，在对应功能目录查找。

## Wayfinding

用于 `/wayfinder`；其任务使用下述 claimed/resolved 状态。

- 地图：`.scratch/<effort>/map.md`，记录 Notes、Decisions-so-far、Fog。
- 子任务：`.scratch/<effort>/issues/<NN>-<slug>.md`，从 `01` 编号，正文记录待解问题。
- 类型：`Type:` 为 research、prototype、grilling 或 task。
- 状态：领取前不设 Status；领取后为 `Status: claimed`，完成后为 `Status: resolved`。
- 依赖：顶部用 `Blocked by: NN, NN` 引用同目录任务；全部依赖 resolved 后解除阻塞。
- 下一任务：按编号选择尚未领取、尚未解决且无阻塞的任务。
- 领取：开始工作前保存 `Status: claimed`。
- 完成：在 `## Answer` 下追加结论，设为 resolved，并在地图的 Decisions-so-far 中追加摘要与链接。
