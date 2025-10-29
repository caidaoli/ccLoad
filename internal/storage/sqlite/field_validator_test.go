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
}
