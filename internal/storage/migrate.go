package storage

import (
	"context"
	"database/sql"

	"ccLoad/internal/storage/mysql"
	"ccLoad/internal/storage/sqlite"
)

// migrateSQLite 执行SQLite数据库迁移
func migrateSQLite(ctx context.Context, db *sql.DB) error {
	return sqlite.Migrate(ctx, db)
}

// migrateMySQL 执行MySQL数据库迁移  
func migrateMySQL(ctx context.Context, db *sql.DB) error {
	return mysql.Migrate(ctx, db)
}
