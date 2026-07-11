# 模型目录缓存默认路径降级设计

## 根因

SQLite 与模型目录缓存对默认 `data/` 目录采用了不一致的错误处理：SQLite 在默认目录不可写时会退到系统临时目录，而模型目录缓存始终使用 `data/model-catalog.json`，写入失败后只记录持久化错误。这使同一个运行环境中数据库可以正常启动，但模型价格缓存无法跨重启复用。

## 目标

- 未显式配置缓存路径时，`data/` 不可写则自动使用系统临时目录。
- 显式配置 `CCLOAD_MODEL_CATALOG_CACHE` 时严格使用指定路径，失败必须可见，不得静默降级。
- 启动加载与后台同步必须使用同一个解析后的缓存路径。
- 保持现有语义：远端目录安装到内存成功后，缓存写入失败不回滚内存目录。

## 非目标

- 不重构 SQLite 的路径解析逻辑。
- 不引入跨包通用路径抽象。
- 不改变 models.dev 下载、校验、ETag 或同步周期。
- 不改变模型目录缓存的 JSON 格式。

## 设计

在模型目录同步模块内增加默认缓存路径解析逻辑：

1. 读取并清理 `CCLOAD_MODEL_CATALOG_CACHE`。
2. 环境变量非空时直接返回该路径，不检查、不降级。
3. 环境变量为空时，优先选择 `data/model-catalog.json`。
4. 若 `data/` 已存在且可写，使用默认路径。
5. 若 `data/` 不存在，尝试以 `0755` 创建并验证可写性。
6. 默认目录无法创建或不可写时，使用 `${TMPDIR}/ccload/model-catalog.json`，并记录持久化降级警告。

路径只在 `StartModelCatalogSync` 创建同步器时解析一次。该同步器的 `LoadCache` 与 `Sync` 共用同一个 `cachePath`，因此不会出现从一个位置加载、向另一个位置写入的问题。

可写性检查使用临时文件探测，而不是只检查权限位。这样能正确覆盖只读挂载、ACL 和容器文件系统等实际写入条件。探测文件必须立即关闭并删除。

## 错误处理

- 默认路径不可写：降级到系统临时目录，服务继续运行。
- 临时目录最终写入失败：沿用现有行为，通过 `PersistenceError` 记录，内存价格继续生效。
- 显式路径不可写：不降级，通过现有 `PersistenceError` 暴露配置错误。
- 缓存文件不存在：沿用现有行为，启动时视为无缓存。

## 测试

扩展现有 `internal/app/model_catalog_sync_test.go`，不新增测试文件：

- 默认 `data/` 可创建且可写时返回 `data/model-catalog.json`。
- 用同名普通文件占用 `data` 时，返回 `${TMPDIR}/ccload/model-catalog.json`。
- 设置 `CCLOAD_MODEL_CATALOG_CACHE` 时原样返回指定路径，即使其父目录不可写也不降级。
- 保留并运行现有“缓存写入失败但内存目录仍生效”测试。

验证命令：

```bash
go test -tags sonic ./internal/app/...
```

