package app

import (
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMain(m *testing.M) {
	originalPass, hadPass := os.LookupEnv("CCLOAD_PASS")
	_ = os.Setenv("CCLOAD_PASS", "test_password_123")
	gin.SetMode(gin.TestMode)
	// 上游 Transport 走 http.ProxyFromEnvironment；宿主（开发机代理、Claude Code
	// 沙箱）注入的代理变量会把测试里的伪造上游主机劫持出去，测试必须与之隔离。
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		_ = os.Unsetenv(key)
	}

	code := m.Run()

	if hadPass {
		_ = os.Setenv("CCLOAD_PASS", originalPass)
	} else {
		_ = os.Unsetenv("CCLOAD_PASS")
	}
	os.Exit(code)
}
