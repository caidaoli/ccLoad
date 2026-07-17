// Package schema 提供数据库表结构定义和DDL生成
package schema

import (
	"fmt"
	"regexp"
	"strings"
)

var varcharRegex = regexp.MustCompile(`VARCHAR\(\d+\)`)

// TableBuilder 轻量级表构建器（方言无关）
type TableBuilder struct {
	name    string
	columns []string
	indexes []IndexDef
}

// IndexDef 索引定义
type IndexDef struct {
	Name string
	SQL  string
}

// NewTable 创建表构建器
func NewTable(name string) *TableBuilder {
	return &TableBuilder{name: name}
}

// Name 返回表名
func (b *TableBuilder) Name() string {
	return b.name
}

// Column 添加列定义（使用MySQL语法作为基准）
func (b *TableBuilder) Column(def string) *TableBuilder {
	b.columns = append(b.columns, def)
	return b
}

// Index 添加索引定义
func (b *TableBuilder) Index(name, columns string) *TableBuilder {
	b.indexes = append(b.indexes, IndexDef{
		Name: name,
		SQL:  fmt.Sprintf("CREATE INDEX %s ON %s(%s)", name, b.name, columns),
	})
	return b
}

// BuildMySQL 生成MySQL DDL
func (b *TableBuilder) BuildMySQL() string {
	sql := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n\t%s\n) ;",
		b.name,
		strings.Join(b.columns, ",\n\t"))
	return sql
}

// BuildSQLite 生成SQLite DDL（类型转换）
func (b *TableBuilder) BuildSQLite() string {
	sqliteColumns := make([]string, len(b.columns))
	for i, col := range b.columns {
		sqliteColumns[i] = mysqlToSQLite(col)
	}

	sql := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n\t%s\n);",
		b.name,
		strings.Join(sqliteColumns, ",\n\t"))
	return sql
}

// mysqlToSQLite 类型转换（MySQL → SQLite）
func mysqlToSQLite(mysqlCol string) string {
	col := mysqlCol

	// 特殊模式先处理（避免部分匹配）
	col = strings.ReplaceAll(col, "INT PRIMARY KEY AUTO_INCREMENT", "INTEGER PRIMARY KEY AUTOINCREMENT")
	col = strings.ReplaceAll(col, "TINYINT", "INTEGER")
	col = strings.ReplaceAll(col, "BIGINT", "INTEGER") // [FIX] P3: BIGINT应转换为INTEGER

	// 通用类型映射（使用词边界）
	col = replaceWord(col, "INT", "INTEGER")
	col = strings.ReplaceAll(col, "DOUBLE", "REAL")

	// VARCHAR → TEXT
	col = replaceVarchar(col)

	// 索引约束简化（MySQL的UNIQUE KEY → SQLite的UNIQUE）
	col = strings.ReplaceAll(col, "UNIQUE KEY uk_channel_key", "UNIQUE")

	return col
}

// BuildPostgres 生成 PostgreSQL DDL（从 MySQL 基准列定义转换）
func (b *TableBuilder) BuildPostgres() string {
	pgColumns := make([]string, 0, len(b.columns))
	for _, col := range b.columns {
		converted := mysqlToPostgres(col)
		if converted == "" {
			continue // 行内 UNIQUE KEY 等已提升为独立约束时跳过空结果
		}
		pgColumns = append(pgColumns, converted)
	}

	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n\t%s\n);",
		b.name,
		strings.Join(pgColumns, ",\n\t"))
}

// mysqlToPostgres 类型转换（MySQL → PostgreSQL）
func mysqlToPostgres(mysqlCol string) string {
	col := strings.TrimSpace(mysqlCol)

	// 行内 UNIQUE KEY 定义 → 表级 UNIQUE 约束
	if strings.HasPrefix(strings.ToUpper(col), "UNIQUE KEY") {
		// UNIQUE KEY uk_channel_key (channel_id, key_index) → UNIQUE (channel_id, key_index)
		if i := strings.Index(col, "("); i >= 0 {
			return "UNIQUE " + col[i:]
		}
		return ""
	}

	// 主键自增
	col = strings.ReplaceAll(col, "INT PRIMARY KEY AUTO_INCREMENT", "BIGSERIAL PRIMARY KEY")

	// 反引号 → 双引号
	col = strings.ReplaceAll(col, "`", `"`)

	// 类型映射（长前缀先替换，避免部分匹配）
	col = strings.ReplaceAll(col, "LONGBLOB", "BYTEA")
	col = strings.ReplaceAll(col, "MEDIUMBLOB", "BYTEA")
	col = strings.ReplaceAll(col, "TINYBLOB", "BYTEA")
	col = strings.ReplaceAll(col, "BLOB", "BYTEA")
	col = strings.ReplaceAll(col, "LONGTEXT", "TEXT")
	col = strings.ReplaceAll(col, "MEDIUMTEXT", "TEXT")
	col = strings.ReplaceAll(col, "TINYINT", "SMALLINT")
	// DOUBLE → DOUBLE PRECISION（避免重复替换已有 DOUBLE PRECISION）
	if !strings.Contains(col, "DOUBLE PRECISION") {
		col = strings.ReplaceAll(col, "DOUBLE", "DOUBLE PRECISION")
	}
	// BIGINT/INT/VARCHAR/TEXT/CHAR 保留

	return col
}

// GetIndexesPostgres 获取 PostgreSQL 索引（IF NOT EXISTS）
func (b *TableBuilder) GetIndexesPostgres() []IndexDef {
	indexes := make([]IndexDef, len(b.indexes))
	for i, idx := range b.indexes {
		indexes[i] = IndexDef{
			Name: idx.Name,
			SQL:  strings.Replace(idx.SQL, "CREATE INDEX", "CREATE INDEX IF NOT EXISTS", 1),
		}
	}
	return indexes
}

// replaceWord 替换单词（避免部分匹配）
func replaceWord(s, oldWord, newWord string) string {
	words := strings.Fields(s)
	for i, word := range words {
		// 去除标点符号检查
		cleanWord := strings.TrimRight(word, ",")
		if cleanWord == oldWord {
			words[i] = strings.Replace(word, oldWord, newWord, 1)
		}
	}
	return strings.Join(words, " ")
}

// replaceVarchar 将 VARCHAR(n) 替换为 TEXT
func replaceVarchar(s string) string {
	return varcharRegex.ReplaceAllString(s, "TEXT")
}

// GetIndexesMySQL 获取MySQL索引创建语句
func (b *TableBuilder) GetIndexesMySQL() []IndexDef {
	return b.indexes
}

// GetIndexesSQLite 获取SQLite索引创建语句（添加IF NOT EXISTS）
func (b *TableBuilder) GetIndexesSQLite() []IndexDef {
	indexes := make([]IndexDef, len(b.indexes))
	for i, idx := range b.indexes {
		indexes[i] = IndexDef{
			Name: idx.Name,
			SQL:  strings.Replace(idx.SQL, "CREATE INDEX", "CREATE INDEX IF NOT EXISTS", 1),
		}
	}
	return indexes
}
