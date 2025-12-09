package schema

import (
	"fmt"
	"strings"
)

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
	col = strings.ReplaceAll(col, "BIGINT ", "BIGINT ")  // BIGINT保持不变
	col = strings.ReplaceAll(col, "TINYINT", "INTEGER")

	// 通用类型映射（使用词边界）
	col = replaceWord(col, "INT", "INTEGER")
	col = strings.ReplaceAll(col, "DOUBLE", "REAL")

	// VARCHAR → TEXT
	col = replaceVarchar(col)

	// 索引约束简化（MySQL的UNIQUE KEY → SQLite的UNIQUE）
	col = strings.ReplaceAll(col, "UNIQUE KEY uk_channel_key", "UNIQUE")

	return col
}

// replaceWord 替换单词（避免部分匹配）
func replaceWord(s, old, new string) string {
	words := strings.Fields(s)
	for i, word := range words {
		// 去除标点符号检查
		cleanWord := strings.TrimRight(word, ",")
		if cleanWord == old {
			words[i] = strings.Replace(word, old, new, 1)
		}
	}
	return strings.Join(words, " ")
}

// replaceVarchar 将 VARCHAR(n) 替换为 TEXT
func replaceVarchar(s string) string {
	// 简单实现：替换所有VARCHAR(n)
	for i := 0; i < 1000; i++ {
		old := fmt.Sprintf("VARCHAR(%d)", i)
		if strings.Contains(s, old) {
			s = strings.ReplaceAll(s, old, "TEXT")
		}
	}
	return s
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
