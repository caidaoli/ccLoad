package schema

import (
	"strings"
	"testing"
)

func TestTableBuilder_NameAndDDL(t *testing.T) {
	tb := NewTable("t1").
		Column("id INT PRIMARY KEY AUTO_INCREMENT").
		Column("name VARCHAR(32) NOT NULL").
		Column("cooldown_until BIGINT NOT NULL DEFAULT 0").
		Column("enabled TINYINT NOT NULL DEFAULT 1").
		Index("idx_t1_enabled", "enabled")

	if tb.Name() != "t1" {
		t.Fatalf("Name=%q, want %q", tb.Name(), "t1")
	}

	mysqlDDL := tb.BuildMySQL()
	if mysqlDDL == "" {
		t.Fatalf("BuildMySQL returned empty")
	}

	sqliteDDL := tb.BuildSQLite()
	// 关键类型转换：AUTO_INCREMENT/BIGINT/TINYINT/VARCHAR
	for _, mustContain := range []string{
		"INTEGER PRIMARY KEY AUTOINCREMENT",
		"INTEGER NOT NULL DEFAULT 0",
		"INTEGER NOT NULL DEFAULT 1",
		"TEXT NOT NULL",
	} {
		if !strings.Contains(sqliteDDL, mustContain) {
			t.Fatalf("BuildSQLite missing %q, got:\n%s", mustContain, sqliteDDL)
		}
	}

	idx := tb.GetIndexesSQLite()
	if len(idx) != 1 {
		t.Fatalf("GetIndexesSQLite len=%d, want 1", len(idx))
	}
	if !strings.Contains(idx[0].SQL, "IF NOT EXISTS") {
		t.Fatalf("expected SQLite index to include IF NOT EXISTS, got %q", idx[0].SQL)
	}
}

func TestDefineSchemaMigrationsTable(t *testing.T) {
	tb := DefineSchemaMigrationsTable()
	if tb.Name() != "schema_migrations" {
		t.Fatalf("Name=%q, want %q", tb.Name(), "schema_migrations")
	}
	if ddl := tb.BuildSQLite(); ddl == "" {
		t.Fatalf("BuildSQLite returned empty")
	}
}
