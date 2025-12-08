package sqlite

import (
	"database/sql"

	sqlstore "ccLoad/internal/storage/sql"
)

// SQLiteStore 最小化结构（仅用于迁移）
// ⚠️ 技术债: 这是为了兼容现有测试的临时wrapper
// TODO: 将 sqlite/*_test.go 迁移到 sql/*_test.go，然后删除此文件
type SQLiteStore struct {
	*sqlstore.SQLStore // 嵌入sql.SQLStore
	db                 *sql.DB
}

// NewSQLiteStore 临时兼容函数
// ⚠️ 技术债: 仅用于测试兼容，应该使用 storage.CreateSQLiteStore()
func NewSQLiteStore(db *sql.DB, redisSync sqlstore.RedisSync) *SQLiteStore {
	return &SQLiteStore{
		SQLStore: sqlstore.NewSQLStore(db, redisSync),
		db:       db,
	}
}
