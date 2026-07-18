package sql_test

import (
	"context"
	stdsql "database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"ccLoad/internal/model"
	sqlstore "ccLoad/internal/storage/sql"
)

const foundRowsDriverName = "ccload_mysql_found_rows_test"

var (
	registerFoundRowsDriverOnce sync.Once
	foundRowsStatesMu           sync.Mutex
	foundRowsStates             = map[string]*foundRowsState{}
)

type foundRowsState struct {
	tokenHash string
	existing  *model.AuthToken
}

type foundRowsDriver struct{}

func (foundRowsDriver) Open(name string) (driver.Conn, error) {
	foundRowsStatesMu.Lock()
	state := foundRowsStates[name]
	foundRowsStatesMu.Unlock()
	return &foundRowsConn{state: state}, nil
}

type foundRowsConn struct {
	state *foundRowsState
}

func (c *foundRowsConn) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (c *foundRowsConn) Close() error {
	return nil
}

func (c *foundRowsConn) Begin() (driver.Tx, error) {
	return nil, driver.ErrSkip
}

func (c *foundRowsConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if !strings.Contains(query, "INSERT INTO auth_tokens") {
		return nil, driver.ErrSkip
	}
	if !strings.Contains(query, "ON DUPLICATE KEY UPDATE") {
		return nil, &mysql.MySQLError{Number: 1062, Message: "Duplicate entry for key 'auth_tokens.token'"}
	}
	return foundRowsResult{lastInsertID: c.state.existing.ID, rowsAffected: 1}, nil
}

func (c *foundRowsConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "FROM auth_tokens WHERE token = ?") {
		return nil, driver.ErrSkip
	}
	if len(args) == 1 && args[0].Value == c.state.tokenHash {
		return &foundRowsRows{values: [][]driver.Value{authTokenDriverValues(c.state.existing)}}, nil
	}
	return &foundRowsRows{}, nil
}

type foundRowsResult struct {
	lastInsertID int64
	rowsAffected int64
}

func (r foundRowsResult) LastInsertId() (int64, error) {
	return r.lastInsertID, nil
}

func (r foundRowsResult) RowsAffected() (int64, error) {
	return r.rowsAffected, nil
}

type foundRowsRows struct {
	values [][]driver.Value
	pos    int
}

func (r *foundRowsRows) Columns() []string {
	return []string{
		"id", "token", "description", "created_at", "expires_at", "last_used_at", "is_active",
		"success_count", "failure_count", "stream_avg_ttfb", "non_stream_avg_rt", "stream_count", "non_stream_count",
		"prompt_tokens_total", "completion_tokens_total", "cache_read_tokens_total", "cache_creation_tokens_total", "total_cost_usd", "effective_cost_usd",
		"cost_used_microusd", "cost_limit_microusd", "allowed_models", "allowed_channel_ids", "channel_restriction_mode", "max_concurrency",
	}
}

func (r *foundRowsRows) Close() error {
	return nil
}

func (r *foundRowsRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.pos])
	r.pos++
	return nil
}

func authTokenDriverValues(token *model.AuthToken) []driver.Value {
	mode, err := model.NormalizeChannelRestrictionMode(token.ChannelRestrictionMode)
	if err != nil {
		panic(err)
	}
	return []driver.Value{
		token.ID,
		token.Token,
		token.Description,
		token.CreatedAt.UnixMilli(),
		int64(0),
		int64(0),
		int64(1),
		token.SuccessCount,
		token.FailureCount,
		token.StreamAvgTTFB,
		token.NonStreamAvgRT,
		token.StreamCount,
		token.NonStreamCount,
		token.PromptTokensTotal,
		token.CompletionTokensTotal,
		token.CacheReadTokensTotal,
		token.CacheCreationTokensTotal,
		token.TotalCostUSD,
		token.EffectiveCostUSD,
		token.CostUsedMicroUSD,
		token.CostLimitMicroUSD,
		`["gpt-4o"]`,
		`[42]`,
		mode,
		token.MaxConcurrency,
	}
}

func newFoundRowsTestStore(t *testing.T, state *foundRowsState) *sqlstore.SQLStore {
	t.Helper()
	registerFoundRowsDriverOnce.Do(func() {
		stdsql.Register(foundRowsDriverName, foundRowsDriver{})
	})

	dsn := t.Name()
	foundRowsStatesMu.Lock()
	foundRowsStates[dsn] = state
	foundRowsStatesMu.Unlock()
	t.Cleanup(func() {
		foundRowsStatesMu.Lock()
		delete(foundRowsStates, dsn)
		foundRowsStatesMu.Unlock()
	})

	db, err := stdsql.Open(foundRowsDriverName, dsn)
	if err != nil {
		t.Fatalf("open found rows db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return sqlstore.NewSQLStore(db, "mysql")
}

func TestEnsureAuthToken_MySQLClientFoundRowsBackfillsExistingToken(t *testing.T) {
	ctx := context.Background()
	tokenHash := model.HashToken("client-found-rows-token")
	existing := &model.AuthToken{
		ID:                77,
		Token:             tokenHash,
		Description:       "existing restricted token",
		CreatedAt:         time.Unix(1700000000, 0),
		IsActive:          true,
		SuccessCount:      3,
		CostLimitMicroUSD: 5000,
		AllowedModels:     []string{"gpt-4o"},
		AllowedChannelIDs: []int64{42},
		MaxConcurrency:    2,
	}
	store := newFoundRowsTestStore(t, &foundRowsState{
		tokenHash: tokenHash,
		existing:  existing,
	})

	token := &model.AuthToken{
		Token:       tokenHash,
		Description: "env default token",
		CreatedAt:   time.Now(),
		IsActive:    false,
	}

	created, err := store.EnsureAuthToken(ctx, token)
	if err != nil {
		t.Fatalf("EnsureAuthToken failed: %v", err)
	}
	if created {
		t.Fatal("duplicate token reported as created")
	}
	if token.ID != existing.ID ||
		token.Description != existing.Description ||
		token.CostLimitMicroUSD != existing.CostLimitMicroUSD ||
		token.MaxConcurrency != existing.MaxConcurrency ||
		len(token.AllowedModels) != 1 || token.AllowedModels[0] != "gpt-4o" ||
		len(token.AllowedChannelIDs) != 1 || token.AllowedChannelIDs[0] != 42 {
		t.Fatalf("token was not backfilled from existing row: %+v", token)
	}
}
