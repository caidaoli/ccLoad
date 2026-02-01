//go:build mysql_integration

package storage

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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
}
