package mysql

import (
	"database/sql"
)

// MySQLStore 最小化结构（仅用于迁移）
type MySQLStore struct {
	db *sql.DB
}
