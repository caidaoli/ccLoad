package storage

import (
	sqlstore "ccLoad/internal/storage/sql"
)

// quoteKeyIdent 返回 system_settings.key 在各数据库方言中的标识符。
func quoteKeyIdent(dialect Dialect) string {
	switch dialect {
	case DialectMySQL:
		return "`key`"
	case DialectPostgres:
		return `"key"`
	default:
		return "key"
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
