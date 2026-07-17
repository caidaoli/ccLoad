package storage

import (
	sqlstore "ccLoad/internal/storage/sql"
)

// dialectUsesMySQLStyleKeyQuote MySQL 用反引号；Postgres 用双引号；SQLite 裸标识符
func quoteIdent(dialect Dialect, name string) string {
	switch dialect {
	case DialectMySQL:
		return "`" + name + "`"
	case DialectPostgres:
		return `"` + name + `"`
	default:
		return name
	}
}

func insertIgnoreSchemaMigrationSQL(dialect Dialect) string {
	switch dialect {
	case DialectMySQL:
		return `INSERT IGNORE INTO schema_migrations (version, applied_at) VALUES (?, UNIX_TIMESTAMP())`
	case DialectPostgres:
		return `INSERT INTO schema_migrations (version, applied_at) VALUES (?, EXTRACT(EPOCH FROM NOW())::BIGINT) ON CONFLICT (version) DO NOTHING`
	default:
		return `INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (?, unixepoch())`
	}
}

func rebindIfPostgres(dialect Dialect, query string) string {
	if dialect != DialectPostgres {
		return query
	}
	return sqlstore.RebindPostgres(query)
}
