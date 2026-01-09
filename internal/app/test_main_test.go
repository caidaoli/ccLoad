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

	code := m.Run()

	if hadPass {
		_ = os.Setenv("CCLOAD_PASS", originalPass)
	} else {
		_ = os.Unsetenv("CCLOAD_PASS")
	}
	os.Exit(code)
}
