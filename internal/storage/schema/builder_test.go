package schema

import (
	"testing"
)

func TestChannelsTableGeneration(t *testing.T) {
	channels := DefineChannelsTable()

	t.Run("MySQL DDL", func(t *testing.T) {
		sql := channels.BuildMySQL()
		t.Logf("MySQL DDL:\n%s", sql)

		// 验证关键字
		if !contains(sql, "INT PRIMARY KEY AUTO_INCREMENT") {
			t.Error("Missing AUTO_INCREMENT")
		}
		if !contains(sql, "VARCHAR(191)") {
			t.Error("Missing VARCHAR")
		}
	})

	t.Run("SQLite DDL", func(t *testing.T) {
		sql := channels.BuildSQLite()
		t.Logf("SQLite DDL:\n%s", sql)

		// 验证类型转换
		if !contains(sql, "INTEGER PRIMARY KEY AUTOINCREMENT") {
			t.Error("Missing AUTOINCREMENT")
		}
		if !contains(sql, "TEXT") {
			t.Error("Missing TEXT type")
		}
		if contains(sql, "VARCHAR") {
			t.Error("VARCHAR not converted to TEXT")
		}
	})

	t.Run("Indexes", func(t *testing.T) {
		mysqlIndexes := channels.GetIndexesMySQL()
		sqliteIndexes := channels.GetIndexesSQLite()

		if len(mysqlIndexes) != 4 {
			t.Errorf("Expected 4 MySQL indexes, got %d", len(mysqlIndexes))
		}

		// 验证SQLite索引包含IF NOT EXISTS
		for _, idx := range sqliteIndexes {
			if !contains(idx.SQL, "IF NOT EXISTS") {
				t.Errorf("SQLite index missing IF NOT EXISTS: %s", idx.SQL)
			}
		}

		t.Logf("MySQL indexes: %d", len(mysqlIndexes))
		t.Logf("SQLite indexes: %d", len(sqliteIndexes))
	})
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && stringContains(s, substr)
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
