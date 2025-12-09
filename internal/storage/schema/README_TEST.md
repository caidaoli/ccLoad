# Schema Integration Tests

本文档描述了 `internal/storage/schema` 包的集成测试。

## 测试文件结构

- `builder_test.go` - 基础单元测试，测试表构建器的基本功能
- `integration_test.go` - 集成测试，验证DDL在真实数据库中的执行

## 运行测试

### 1. 运行所有测试（推荐）

```bash
cd /Users/caidaoli/Share/Source/go/ccLoad
go test -tags go_json -v ./internal/storage/schema/
```

### 2. 只运行单元测试

```bash
go test -tags go_json -v ./internal/storage/schema/ -run TestChannelsTableGeneration
```

### 3. 只运行集成测试（SQLite）

```bash
go test -tags go_json -v ./internal/storage/schema/ -run TestAllTablesSQLiteIntegration
```

### 4. 运行MySQL集成测试（可选）

如果需要测试MySQL，需要设置环境变量：

```bash
export CCLOAD_TEST_MYSQL_DSN="user:password@tcp(localhost:3306)/testdb?charset=utf8mb4&parseTime=True&loc=Local"
go test -tags go_json -v ./internal/storage/schema/ -run TestAllTablesMySQLIntegration
```

## 测试覆盖范围

### 单元测试 (`builder_test.go`)

- 测试MySQL DDL生成
- 测试SQLite DDL生成
- 测试索引生成
- 测试类型转换

### 集成测试 (`integration_test.go`)

#### TestAllTablesSQLiteIntegration
- 验证所有7张表的SQLite DDL能在内存数据库中成功执行
- 验证表结构（列类型）
- 验证索引创建
- 测试基本插入操作
- 测试表间关联关系

#### TestAllTablesMySQLIntegration
- 验证所有7张表的MySQL DDL能在真实MySQL数据库中执行
- 验证表结构（列类型）
- 验证索引创建
- 测试基本插入操作

#### 辅助测试
- TestTypeConversionCorrectness - 验证类型转换的正确性
- TestIndexGeneration - 验证索引生成的正确性
- TestBuilderChain - 验证Builder链式调用

## 7张表定义

1. **channels** - 渠道表
2. **api_keys** - API密钥表
3. **channel_models** - 渠道模型关联表
4. **auth_tokens** - 认证令牌表
5. **system_settings** - 系统配置表
6. **admin_sessions** - 管理员会话表
7. **logs** - 日志表

## 类型映射规则

| MySQL | SQLite | 说明 |
|-------|--------|------|
| INT | INTEGER | 整数类型 |
| BIGINT | BIGINT | 大整数类型（保持不变） |
| TINYINT | INTEGER | 小整数类型 |
| VARCHAR(n) | TEXT | 字符串类型 |
| TEXT | TEXT | 文本类型（保持不变） |
| DOUBLE | REAL | 双精度浮点数 |
| INT PRIMARY KEY AUTO_INCREMENT | INTEGER PRIMARY KEY AUTOINCREMENT | 自增主键 |
| UNIQUE KEY uk_xxx | UNIQUE | 唯一约束简化 |

## 测试环境

- SQLite: 使用内存数据库 `:memory:`
- MySQL: 通过环境变量DSN配置（可选）

## 注意事项

1. 所有测试必须使用 `-tags go_json` 标签运行
2. 集成测试会自动跳过MySQL测试，除非设置了环境变量
3. 索引验证在某些情况下可能失败，但这不影响核心功能
4. 测试会自动清理，无需手动干预