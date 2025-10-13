package sqlite

import (
	"strings"
	"testing"
)

func TestValidateFieldName(t *testing.T) {
	tests := []struct {
		name      string
		field     string
		wantError bool
	}{
		{
			name:      "合法字段-id",
			field:     "id",
			wantError: false,
		},
		{
			name:      "合法字段-channel_id",
			field:     "channel_id",
			wantError: false,
		},
		{
			name:      "合法字段-带表前缀",
			field:     "c.name",
			wantError: false,
		},
		{
			name:      "非法字段-不在白名单",
			field:     "malicious_field",
			wantError: true,
		},
		{
			name:      "SQL注入尝试-DROP TABLE",
			field:     "id; DROP TABLE logs--",
			wantError: true,
		},
		{
			name:      "SQL注入尝试-UNION",
			field:     "id UNION SELECT * FROM users",
			wantError: true,
		},
		{
			name:      "空字段名",
			field:     "",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateFieldName(tt.field)
			if (err != nil) != tt.wantError {
				t.Errorf("ValidateFieldName(%q) error = %v, wantError %v",
					tt.field, err, tt.wantError)
			}

			if err != nil && !strings.Contains(err.Error(), "invalid field name") {
				t.Errorf("错误消息应包含 'invalid field name', got: %v", err)
			}
		})
	}
}

func TestSanitizeOrderBy(t *testing.T) {
	tests := []struct {
		name      string
		orderBy   string
		want      string
		wantError bool
	}{
		{
			name:      "合法-单字段升序",
			orderBy:   "id ASC",
			want:      "id ASC",
			wantError: false,
		},
		{
			name:      "合法-单字段降序",
			orderBy:   "created_at DESC",
			want:      "created_at DESC",
			wantError: false,
		},
		{
			name:      "合法-多字段",
			orderBy:   "priority DESC, id ASC",
			want:      "priority DESC, id ASC",
			wantError: false,
		},
		{
			name:      "合法-无排序方向",
			orderBy:   "name",
			want:      "name",
			wantError: false,
		},
		{
			name:      "非法-无效字段",
			orderBy:   "malicious_field DESC",
			wantError: true,
		},
		{
			name:      "非法-SQL注入",
			orderBy:   "id; DROP TABLE logs--",
			wantError: true,
		},
		{
			name:      "非法-无效排序方向",
			orderBy:   "id RANDOM",
			wantError: true,
		},
		{
			name:      "空字符串",
			orderBy:   "",
			want:      "",
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SanitizeOrderBy(tt.orderBy)
			if (err != nil) != tt.wantError {
				t.Errorf("SanitizeOrderBy(%q) error = %v, wantError %v",
					tt.orderBy, err, tt.wantError)
			}

			if !tt.wantError && got != tt.want {
				t.Errorf("SanitizeOrderBy(%q) = %q, want %q",
					tt.orderBy, got, tt.want)
			}
		})
	}
}

func TestValidateMultipleFields(t *testing.T) {
	t.Run("全部合法", func(t *testing.T) {
		err := ValidateMultipleFields("id", "name", "created_at")
		if err != nil {
			t.Errorf("ValidateMultipleFields 应该通过，got error: %v", err)
		}
	})

	t.Run("包含非法字段", func(t *testing.T) {
		err := ValidateMultipleFields("id", "malicious", "name")
		if err == nil {
			t.Error("ValidateMultipleFields 应该返回错误")
		}
	})
}

// TestQueryBuilderFieldValidation 测试 QueryBuilder 的字段验证
func TestQueryBuilderFieldValidation(t *testing.T) {
	t.Run("WhereIn-合法字段", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("WhereIn 不应该 panic，got: %v", r)
			}
		}()

		qb := NewQueryBuilder("SELECT * FROM logs")
		qb.WhereIn("channel_id", []any{1, 2, 3})

		query, args := qb.Build()
		expectedQuery := "SELECT * FROM logs WHERE channel_id IN (?,?,?)"
		if query != expectedQuery {
			t.Errorf("query = %q, want %q", query, expectedQuery)
		}

		if len(args) != 3 {
			t.Errorf("args length = %d, want 3", len(args))
		}
	})

	t.Run("WhereIn-非法字段应panic", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Error("WhereIn 应该 panic")
			}
			if !strings.Contains(r.(string), "SQL注入防护") {
				t.Errorf("panic 消息应包含 'SQL注入防护', got: %v", r)
			}
		}()

		qb := NewQueryBuilder("SELECT * FROM logs")
		qb.WhereIn("malicious_field", []any{1, 2, 3})
	})

	t.Run("AddTimeRange-合法字段", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("AddTimeRange 不应该 panic，got: %v", r)
			}
		}()

		wb := NewWhereBuilder()
		wb.AddTimeRange("time", 1234567890)

		whereClause, args := wb.Build()
		expectedClause := "time >= ?"
		if whereClause != expectedClause {
			t.Errorf("whereClause = %q, want %q", whereClause, expectedClause)
		}

		if len(args) != 1 {
			t.Errorf("args length = %d, want 1", len(args))
		}
	})

	t.Run("AddTimeRange-非法字段应panic", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Error("AddTimeRange 应该 panic")
			}
		}()

		wb := NewWhereBuilder()
		wb.AddTimeRange("malicious; DROP TABLE", 1234567890)
	})
}
