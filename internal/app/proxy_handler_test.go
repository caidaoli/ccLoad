package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHandleProxyRequest_UnknownPathReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := &Server{
		concurrencySem: make(chan struct{}, 1),
	}

	body := bytes.NewBufferString(`{"model":"gpt-4"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/unknown", body)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	srv.handleProxyRequest(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("预期状态码404，实际%d", w.Code)
	}

	if body := w.Body.String(); !bytes.Contains([]byte(body), []byte("unsupported path")) {
		t.Fatalf("响应内容缺少错误信息，实际: %s", body)
	}
}
