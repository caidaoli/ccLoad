package sql_test

import (
	"testing"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
)

func TestWhereBuilder_ApplyLogFilter(t *testing.T) {
	t.Parallel()

	channelID := int64(42)
	statusCode := 500
	authTokenID := int64(7)

	tests := []struct {
		name          string
		filter        *model.LogFilter
		expectArgsLen int
		expectNoWhere bool
	}{
		{
			name:          "nil filter",
			filter:        nil,
			expectNoWhere: true,
		},
		{
			name:          "empty filter",
			filter:        &model.LogFilter{},
			expectNoWhere: true,
		},
		{
			name: "channel_id only",
			filter: &model.LogFilter{
				ChannelID: &channelID,
			},
			expectArgsLen: 1,
		},
		{
			name: "model exact match",
			filter: &model.LogFilter{
				Model: "gpt-4o",
			},
			expectArgsLen: 1,
		},
		{
			name: "model like match",
			filter: &model.LogFilter{
				ModelLike: "claude",
			},
			expectArgsLen: 1,
		},
		{
			name: "status_code filter",
			filter: &model.LogFilter{
				StatusCode: &statusCode,
			},
			expectArgsLen: 1,
		},
		{
			name: "auth_token_id filter",
			filter: &model.LogFilter{
				AuthTokenID: &authTokenID,
			},
			expectArgsLen: 1,
		},
		{
			name: "all filters combined",
			filter: &model.LogFilter{
				ChannelID:   &channelID,
				Model:       "gpt-4o",
				StatusCode:  &statusCode,
				AuthTokenID: &authTokenID,
			},
			expectArgsLen: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wb := sqlstore.NewWhereBuilder()
			wb.ApplyLogFilter(tt.filter)
			clause, args := wb.Build()

			if tt.expectNoWhere {
				if clause != "" {
					t.Errorf("expected empty clause, got %q", clause)
				}
				return
			}

			if len(args) != tt.expectArgsLen {
				t.Errorf("expected %d args, got %d", tt.expectArgsLen, len(args))
			}

			if clause == "" {
				t.Error("expected non-empty clause")
			}
		})
	}
}

func TestWhereBuilder_Build_EmptyConditions(t *testing.T) {
	t.Parallel()

	wb := sqlstore.NewWhereBuilder()
	clause, args := wb.Build()

	if clause != "" {
		t.Errorf("expected empty clause, got %q", clause)
	}
	if len(args) != 0 {
		t.Errorf("expected 0 args, got %d", len(args))
	}
}

func TestWhereBuilder_Build_MultipleConditions(t *testing.T) {
	t.Parallel()

	wb := sqlstore.NewWhereBuilder()
	wb.AddCondition("a = ?", 1)
	wb.AddCondition("b = ?", 2)
	wb.AddCondition("c > ?", 3)

	clause, args := wb.Build()

	if clause != "a = ? AND b = ? AND c > ?" {
		t.Errorf("unexpected clause: %q", clause)
	}
	if len(args) != 3 {
		t.Errorf("expected 3 args, got %d", len(args))
	}
}

func TestWhereBuilder_BuildWithPrefix(t *testing.T) {
	t.Parallel()

	t.Run("empty conditions returns empty", func(t *testing.T) {
		wb := sqlstore.NewWhereBuilder()
		clause, args := wb.BuildWithPrefix("WHERE")

		if clause != "" {
			t.Errorf("expected empty clause, got %q", clause)
		}
		if len(args) != 0 {
			t.Errorf("expected 0 args, got %d", len(args))
		}
	})

	t.Run("with conditions adds prefix", func(t *testing.T) {
		wb := sqlstore.NewWhereBuilder()
		wb.AddCondition("x = ?", 1)
		clause, _ := wb.BuildWithPrefix("WHERE")

		if clause != "WHERE x = ?" {
			t.Errorf("unexpected clause: %q", clause)
		}
	})

	t.Run("custom prefix", func(t *testing.T) {
		wb := sqlstore.NewWhereBuilder()
		wb.AddCondition("x = ?", 1)
		clause, _ := wb.BuildWithPrefix("AND")

		if clause != "AND x = ?" {
			t.Errorf("unexpected clause: %q", clause)
		}
	})
}

func TestWhereBuilder_AddCondition_EmptyString(t *testing.T) {
	t.Parallel()

	wb := sqlstore.NewWhereBuilder()
	wb.AddCondition("", 1) // 空条件应被忽略
	wb.AddCondition("a = ?", 2)

	clause, args := wb.Build()

	if clause != "a = ?" {
		t.Errorf("unexpected clause: %q", clause)
	}
	if len(args) != 1 {
		t.Errorf("expected 1 arg, got %d", len(args))
	}
}

func TestWhereBuilder_Chaining(t *testing.T) {
	t.Parallel()

	// 测试链式调用
	clause, args := sqlstore.NewWhereBuilder().
		AddCondition("a = ?", 1).
		AddCondition("b > ?", 2).
		Build()

	if clause != "a = ? AND b > ?" {
		t.Errorf("unexpected clause: %q", clause)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %d", len(args))
	}
}
