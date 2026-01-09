package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

// resetCostNotFoundStore 模拟令牌不存在的情况
type resetCostNotFoundStore struct {
	storage.Store
}

func (resetCostNotFoundStore) ResetTokenCost(ctx context.Context, tokenID int64) error {
	return sql.ErrNoRows
}

// resetCostSuccessStore 模拟成功重置的情况
type resetCostSuccessStore struct {
	storage.Store
	calledWithID int64
}

func (s *resetCostSuccessStore) ResetTokenCost(ctx context.Context, tokenID int64) error {
	s.calledWithID = tokenID
	return nil
}

// resetCostDBErrorStore 模拟数据库错误的情况
type resetCostDBErrorStore struct {
	storage.Store
}

func (resetCostDBErrorStore) ResetTokenCost(ctx context.Context, tokenID int64) error {
	return errors.New("database connection failed")
}

func TestHandleResetTokenCost_NotFound_Returns404(t *testing.T) {
	t.Parallel()

	r := gin.New()

	srv := &Server{
		store: resetCostNotFoundStore{},
	}
	r.POST("/admin/auth-tokens/:id/reset-cost", srv.HandleResetTokenCost)

	req := httptest.NewRequest(http.MethodPost, "/admin/auth-tokens/123/reset-cost", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("unexpected status: got=%d want=%d body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleResetTokenCost_InvalidID_Returns400(t *testing.T) {
	t.Parallel()

	r := gin.New()

	srv := &Server{
		store: resetCostNotFoundStore{},
	}
	r.POST("/admin/auth-tokens/:id/reset-cost", srv.HandleResetTokenCost)

	// 测试非数字ID
	req := httptest.NewRequest(http.MethodPost, "/admin/auth-tokens/abc/reset-cost", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status for invalid id: got=%d want=%d body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleResetTokenCost_DBError_Returns500(t *testing.T) {
	t.Parallel()

	r := gin.New()

	srv := &Server{
		store: resetCostDBErrorStore{},
	}
	r.POST("/admin/auth-tokens/:id/reset-cost", srv.HandleResetTokenCost)

	req := httptest.NewRequest(http.MethodPost, "/admin/auth-tokens/123/reset-cost", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unexpected status for db error: got=%d want=%d body=%s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

func TestHandleResetTokenCost_Success_Returns200(t *testing.T) {
	t.Parallel()

	r := gin.New()

	store := &resetCostSuccessStore{}
	srv := &Server{
		store:       store,
		authService: nil, // ReloadAuthTokens会失败但只是警告，不影响主流程
	}
	r.POST("/admin/auth-tokens/:id/reset-cost", srv.HandleResetTokenCost)

	req := httptest.NewRequest(http.MethodPost, "/admin/auth-tokens/456/reset-cost", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status for success: got=%d want=%d body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	// 验证传入了正确的ID
	if store.calledWithID != 456 {
		t.Errorf("expected ResetTokenCost called with id=456, got=%d", store.calledWithID)
	}
}
