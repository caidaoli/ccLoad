//go:build mysql_integration

package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"

	_ "github.com/go-sql-driver/mysql"
)

// ============================================================================
// MySQL 迁移条件化测试
// 运行条件：go test -tags "go_json mysql_integration" ./internal/storage/... -v -run TestMySQL
//
// 依赖环境：
// - Docker 已安装
// - 或设置 CCLOAD_TEST_MYSQL_DSN 环境变量指向现有 MySQL 实例
//
// 示例：
//   # 使用现有 MySQL
//   CCLOAD_TEST_MYSQL_DSN="root:test@tcp(127.0.0.1:3306)/ccload_test?parseTime=true" \
//       go test -tags "go_json mysql_integration" ./internal/storage/... -v -run TestMySQL
//
//   # 自动使用 Docker（无 DSN 环境变量时）
//   go test -tags "go_json mysql_integration" ./internal/storage/... -v -run TestMySQL
// ============================================================================

const (
	testMySQLImage    = "mysql:8.0"
	testMySQLRootPass = "testroot"
	testMySQLDB       = "ccload_test"
)

// mysqlTestEnv 管理测试用 MySQL 环境
type mysqlTestEnv struct {
	dsn         string
	containerID string
	db          *sql.DB
}

// setupMySQLEnv 创建 MySQL 测试环境
// 优先使用 CCLOAD_TEST_MYSQL_DSN 环境变量，否则启动 Docker 容器
func setupMySQLEnv(t *testing.T) *mysqlTestEnv {
	t.Helper()

	if dsn := os.Getenv("CCLOAD_TEST_MYSQL_DSN"); dsn != "" {
		t.Logf("使用环境变量提供的 MySQL DSN")
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatalf("连接 MySQL 失败: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Ping(); err != nil {
			t.Fatalf("MySQL ping 失败: %v", err)
		}
		return &mysqlTestEnv{dsn: dsn, db: db}
	}

	return startDockerMySQL(t)
}

// startDockerMySQL 启动 Docker MySQL 容器
func startDockerMySQL(t *testing.T) *mysqlTestEnv {
	t.Helper()

	// 检查 Docker 是否可用
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("Docker 不可用，跳过 MySQL 集成测试")
	}

	containerName := fmt.Sprintf("ccload-mysql-test-%d", time.Now().UnixNano())

	// 启动 MySQL 容器
	args := []string{
		"run", "-d",
		"--name", containerName,
		"-e", "MYSQL_ROOT_PASSWORD=" + testMySQLRootPass,
		"-e", "MYSQL_DATABASE=" + testMySQLDB,
		// 随机挑选空闲端口，避免与并行测试/本机服务冲突
		"-p", "127.0.0.1::3306",
		testMySQLImage,
	}
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("启动 MySQL 容器失败: %v\n%s", err, out)
	}
	containerID := strings.TrimSpace(string(out))
	t.Logf("启动 MySQL 容器: %s", containerID[:12])

	hostPort := dockerMappedHostPort(t, containerID, "3306/tcp")
	t.Logf("MySQL 端口映射: 127.0.0.1:%s -> 3306", hostPort)

	// 注册清理（在顶层测试结束时执行）
	t.Cleanup(func() {
		t.Logf("停止并删除 MySQL 容器: %s", containerID[:12])
		_ = exec.Command("docker", "stop", containerID).Run()
		_ = exec.Command("docker", "rm", containerID).Run()
	})

	// 等待 MySQL 就绪
	dsn := fmt.Sprintf("root:%s@tcp(127.0.0.1:%s)/%s?parseTime=true&multiStatements=true",
		testMySQLRootPass, hostPort, testMySQLDB)

	var db *sql.DB
	for i := range 30 {
		time.Sleep(time.Second)
		db, err = sql.Open("mysql", dsn)
		if err != nil {
			continue
		}
		if err := db.Ping(); err == nil {
			t.Logf("MySQL 就绪（等待 %d 秒）", i+1)
			t.Cleanup(func() { _ = db.Close() })
			return &mysqlTestEnv{dsn: dsn, containerID: containerID, db: db}
		}
		_ = db.Close()
	}

	t.Fatalf("MySQL 容器启动超时（30秒）")
	return nil
}

func dockerMappedHostPort(t *testing.T, containerID, privatePort string) string {
	t.Helper()

	out, err := exec.Command("docker", "port", containerID, privatePort).CombinedOutput()
	if err != nil {
		t.Fatalf("获取容器端口映射失败: %v\n%s", err, out)
	}

	line := strings.TrimSpace(string(out))
	if line == "" {
		t.Fatalf("容器端口映射为空: container=%s port=%s", containerID[:12], privatePort)
	}

	// docker port 有时返回多行；我们只需要第一条映射
	line = strings.Split(line, "\n")[0]
	if strings.Contains(line, "->") {
		parts := strings.Split(line, "->")
		line = strings.TrimSpace(parts[len(parts)-1])
	}

	idx := strings.LastIndex(line, ":")
	if idx == -1 || idx == len(line)-1 {
		t.Fatalf("无法解析容器端口映射: %q", line)
	}

	return line[idx+1:]
}

// cleanupMySQLTables 清理所有表（用于测试前重置）
func cleanupMySQLTables(t *testing.T, db *sql.DB) {
	t.Helper()

	// 禁用外键检查
	_, _ = db.Exec("SET FOREIGN_KEY_CHECKS = 0")
	defer func() { _, _ = db.Exec("SET FOREIGN_KEY_CHECKS = 1") }()

	tables := []string{"logs", "admin_sessions", "system_settings", "auth_tokens", "channel_models", "api_keys", "channels", "schema_migrations"}
	for _, table := range tables {
		_, _ = db.Exec("DROP TABLE IF EXISTS " + table)
	}
}

// ============================================================================
// MySQL 迁移测试套件
// 使用顶层测试函数包裹子测试，确保容器生命周期正确管理
// ============================================================================

func TestMySQL(t *testing.T) {
	env := setupMySQLEnv(t)

	// 子测试共享同一个容器
	t.Run("FullMigration", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		store, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("CreateMySQLStore 失败: %v", err)
		}
		defer store.Close()

		// 验证关键表存在
		tables := []string{"channels", "api_keys", "channel_models", "auth_tokens", "logs", "system_settings", "admin_sessions"}
		for _, table := range tables {
			var count int
			err := env.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count)
			if err != nil {
				t.Fatalf("表 %s 查询失败: %v", table, err)
			}
			t.Logf("表 %s 存在（行数: %d）", table, count)
		}
	})

	t.Run("Idempotent", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		// 第一次迁移
		store1, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("第一次迁移失败: %v", err)
		}
		store1.Close()

		// 第二次迁移（应该幂等）
		store2, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("第二次迁移失败（应幂等）: %v", err)
		}
		store2.Close()

		t.Log("幂等性验证通过：二次迁移成功")
	})

	t.Run("EnsureColumns_AddNew", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		store, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移失败: %v", err)
		}
		defer store.Close()

		// 验证 logs 表的新列存在
		expectedColumns := []string{"auth_token_id", "client_ip", "minute_bucket", "cache_read_input_tokens", "actual_model"}
		for _, col := range expectedColumns {
			var columnName string
			err := env.db.QueryRow(
				"SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'logs' AND COLUMN_NAME = ?",
				testMySQLDB, col,
			).Scan(&columnName)
			if err != nil {
				t.Fatalf("列 logs.%s 不存在: %v", col, err)
			}
			t.Logf("列 logs.%s 存在", col)
		}

		// 验证 auth_tokens 表的新列
		authTokenCols := []string{"allowed_models", "cost_used_microusd", "cost_limit_microusd"}
		for _, col := range authTokenCols {
			var columnName string
			err := env.db.QueryRow(
				"SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'auth_tokens' AND COLUMN_NAME = ?",
				testMySQLDB, col,
			).Scan(&columnName)
			if err != nil {
				t.Fatalf("列 auth_tokens.%s 不存在: %v", col, err)
			}
			t.Logf("列 auth_tokens.%s 存在", col)
		}

		// 验证 channels 表的 daily_cost_limit 列
		var columnName string
		err = env.db.QueryRow(
			"SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'channels' AND COLUMN_NAME = 'daily_cost_limit'",
			testMySQLDB,
		).Scan(&columnName)
		if err != nil {
			t.Fatalf("列 channels.daily_cost_limit 不存在: %v", err)
		}
		t.Log("列 channels.daily_cost_limit 存在")
	})

	t.Run("EnsureColumns_AlreadyExists", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		// 第一次迁移
		store1, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("第一次迁移失败: %v", err)
		}
		store1.Close()

		// 第二次调用不应报错
		store2, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("已存在列不应报错: %v", err)
		}
		store2.Close()

		t.Log("已存在列验证通过：不报错")
	})

	t.Run("EnsureColumns_APIKeyLengthDrift", func(t *testing.T) {
		cleanupMySQLTables(t, env.db)

		mustExec := func(stmt string) {
			t.Helper()
			if _, err := env.db.Exec(stmt); err != nil {
				t.Fatalf("预置旧表失败: %v\nSQL=%s", err, stmt)
			}
		}

		// 预置旧版 schema：api_keys.api_key 仍是 VARCHAR(64)
		mustExec(`
			CREATE TABLE channels (
				id INT PRIMARY KEY AUTO_INCREMENT,
				name VARCHAR(191) NOT NULL UNIQUE,
				url VARCHAR(191) NOT NULL,
				priority INT NOT NULL DEFAULT 0,
				channel_type VARCHAR(64) NOT NULL DEFAULT 'anthropic',
				enabled TINYINT NOT NULL DEFAULT 1,
				cooldown_until BIGINT NOT NULL DEFAULT 0,
				cooldown_duration_ms BIGINT NOT NULL DEFAULT 0,
				daily_cost_limit DOUBLE NOT NULL DEFAULT 0,
				created_at BIGINT NOT NULL,
				updated_at BIGINT NOT NULL,
				INDEX idx_channels_enabled (enabled),
				INDEX idx_channels_priority (priority DESC),
				INDEX idx_channels_type_enabled (channel_type, enabled),
				INDEX idx_channels_cooldown (cooldown_until)
			)
		`)

		mustExec(`
			CREATE TABLE api_keys (
				id INT PRIMARY KEY AUTO_INCREMENT,
				channel_id INT NOT NULL,
				key_index INT NOT NULL,
				api_key VARCHAR(64) NOT NULL,
				key_strategy VARCHAR(32) NOT NULL DEFAULT 'sequential',
				cooldown_until BIGINT NOT NULL DEFAULT 0,
				cooldown_duration_ms BIGINT NOT NULL DEFAULT 0,
				created_at BIGINT NOT NULL,
				updated_at BIGINT NOT NULL,
				UNIQUE KEY uk_channel_key (channel_id, key_index),
				FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE,
				INDEX idx_api_keys_cooldown (cooldown_until),
				INDEX idx_api_keys_channel_cooldown (channel_id, cooldown_until)
			)
		`)

		mustExec(`
			CREATE TABLE channel_models (
				channel_id INT NOT NULL,
				model VARCHAR(191) NOT NULL,
				redirect_model VARCHAR(191) NOT NULL DEFAULT '',
				created_at BIGINT NOT NULL DEFAULT 0,
				PRIMARY KEY (channel_id, model),
				FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE,
				INDEX idx_channel_models_model (model)
			)
		`)

		store, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("迁移旧 schema 失败: %v", err)
		}
		defer store.Close()

		var (
			dataType   string
			charLen    sql.NullInt64
			isNullable string
		)
		err = env.db.QueryRow(`
			SELECT DATA_TYPE, CHARACTER_MAXIMUM_LENGTH, IS_NULLABLE
			FROM INFORMATION_SCHEMA.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'api_keys' AND COLUMN_NAME = 'api_key'
		`).Scan(&dataType, &charLen, &isNullable)
		if err != nil {
			t.Fatalf("查询 api_keys.api_key 列定义失败: %v", err)
		}

		if !strings.EqualFold(dataType, "varchar") {
			t.Fatalf("api_keys.api_key 类型错误: got=%s want=varchar", dataType)
		}
		if !charLen.Valid || charLen.Int64 != 100 {
			t.Fatalf("api_keys.api_key 长度错误: got=%v want=100", charLen)
		}
		if !strings.EqualFold(isNullable, "NO") {
			t.Fatalf("api_keys.api_key 可空性错误: got=%s want=NO", isNullable)
		}

		longKey := "sk-" + strings.Repeat("x", 77) // 长度 80，验证旧64约束已解除
		created, updated, err := store.ImportChannelBatch(context.Background(), []*model.ChannelWithKeys{
			{
				Config: &model.Config{
					Name:        "legacy-key-len",
					URL:         "https://api.example.com",
					Priority:    1,
					ChannelType: "openai",
					Enabled:     true,
					ModelEntries: []model.ModelEntry{
						{Model: "gpt-4"},
					},
				},
				APIKeys: []model.APIKey{
					{KeyIndex: 0, APIKey: longKey, KeyStrategy: model.KeyStrategySequential},
				},
			},
		})
		if err != nil {
			t.Fatalf("导入长 key 失败: %v", err)
		}
		if created != 1 || updated != 0 {
			t.Fatalf("导入计数异常: created=%d updated=%d", created, updated)
		}

		var keyLen int
		err = env.db.QueryRow("SELECT CHAR_LENGTH(api_key) FROM api_keys WHERE key_index = 0 LIMIT 1").Scan(&keyLen)
		if err != nil {
			t.Fatalf("查询导入 key 长度失败: %v", err)
		}
		if keyLen != len(longKey) {
			t.Fatalf("导入 key 长度不匹配: got=%d want=%d", keyLen, len(longKey))
		}

		store.Close()
		store2, err := CreateMySQLStoreForTest(env.dsn)
		if err != nil {
			t.Fatalf("二次迁移失败（应幂等）: %v", err)
		}
		defer store2.Close()
	})
}
