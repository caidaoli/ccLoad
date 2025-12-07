package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"

	_ "github.com/go-sql-driver/mysql"
)

// ErrSettingNotFound 系统设置未找到错误（重导出自 model 包）
var ErrSettingNotFound = model.ErrSettingNotFound

type MySQLStore struct {
	db *sql.DB

	// 异步Redis同步机制
	syncCh chan struct{}
	done   chan struct{}

	redisSync RedisSync

	wg sync.WaitGroup
}

// RedisSync Redis同步接口抽象
type RedisSync interface {
	IsEnabled() bool
	LoadChannelsWithKeysFromRedis(ctx context.Context) ([]*model.ChannelWithKeys, error)
	SyncAllChannelsWithKeys(ctx context.Context, channels []*model.ChannelWithKeys) error
	SyncAllAuthTokens(ctx context.Context, tokens []*model.AuthToken) error
	LoadAuthTokensFromRedis(ctx context.Context) ([]*model.AuthToken, error)
}

// NewMySQLStore 创建MySQL存储实例
// dsn格式: user:password@tcp(host:port)/dbname?charset=utf8mb4&parseTime=true
func NewMySQLStore(dsn string, redisSync RedisSync) (*MySQLStore, error) {
	// 确保DSN包含必要参数
	if dsn == "" {
		return nil, fmt.Errorf("MySQL DSN不能为空")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开MySQL连接失败: %w", err)
	}

	// 连接池配置
	db.SetMaxOpenConns(config.SQLiteMaxOpenConnsFile * 2) // MySQL可以更高并发
	db.SetMaxIdleConns(config.SQLiteMaxIdleConnsFile * 2)
	db.SetConnMaxLifetime(config.SQLiteConnMaxLifetime)

	// 测试连接
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("MySQL连接测试失败: %w", err)
	}

	s := &MySQLStore{
		db:        db,
		redisSync: redisSync,
		syncCh:    make(chan struct{}, 1),
		done:      make(chan struct{}),
	}

	// 迁移数据库表结构
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("MySQL迁移失败: %w", err)
	}

	// 启动异步Redis同步worker
	if redisSync != nil && redisSync.IsEnabled() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.redisSyncWorker()
		}()
	}

	return s, nil
}

func (s *MySQLStore) IsRedisEnabled() bool {
	return s.redisSync != nil && s.redisSync.IsEnabled()
}

func (s *MySQLStore) Close() error {
	if s.done != nil {
		close(s.done)
	}

	waitCh := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
	case <-time.After(time.Duration(config.RedisSyncShutdownTimeoutMs) * time.Millisecond):
		log.Printf("⚠️  Redis同步worker关闭超时（%dms）", config.RedisSyncShutdownTimeoutMs)
	}

	return s.db.Close()
}

func (s *MySQLStore) CleanupLogsBefore(ctx context.Context, cutoff time.Time) error {
	cutoffMs := cutoff.UnixMilli()
	_, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE time < ?`, cutoffMs)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
