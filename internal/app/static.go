package app

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"ccLoad/internal/version"

	"github.com/gin-gonic/gin"
)

// webRoot 是 web 目录的真实绝对路径，启动时初始化
var webRoot string

// setupStaticFiles 配置静态文件服务
// - HTML 文件：不缓存，动态替换版本号占位符
// - CSS/JS/字体：长缓存（1年），依赖版本号刷新
// - dev 版本：不缓存，方便开发调试
func setupStaticFiles(r *gin.Engine) {
	// 初始化 web 目录真实绝对路径（解析符号链接，用于安全检查）
	absPath, err := filepath.Abs("./web")
	if err != nil {
		log.Fatalf("[FATAL] 无法解析 web 目录路径: %v", err)
	}

	// 解析符号链接获取真实路径
	webRoot, err = filepath.EvalSymlinks(absPath)
	if err != nil {
		// web 目录不存在：生产环境 Fatal，测试环境警告
		if isTestMode() {
			log.Printf("[WARN] web 目录不存在: %v（测试环境忽略）", err)
			webRoot = absPath // 请求时会返回 404
		} else {
			log.Fatalf("[FATAL] web 目录不存在或无法访问: %v", err)
		}
	}

	r.GET("/web/*filepath", serveStaticFile)
}

// isTestMode 检测是否在 Go 测试环境中运行
func isTestMode() bool {
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "-test.") {
			return true
		}
	}
	return false
}

// serveStaticFile 处理静态文件请求
func serveStaticFile(c *gin.Context) {
	// Gin wildcard 参数带前导斜杠，如 "/index.html"
	reqPath := c.Param("filepath")

	// 去除前导斜杠，确保是相对路径
	reqPath = strings.TrimPrefix(reqPath, "/")

	// Clean 处理 .. 和多余的斜杠
	reqPath = filepath.Clean(reqPath)

	// 防止路径遍历：Clean 后仍以 .. 开头说明试图逃逸
	if reqPath == ".." || strings.HasPrefix(reqPath, ".."+string(filepath.Separator)) {
		c.Status(http.StatusForbidden)
		return
	}

	// 构建完整文件路径
	filePath := filepath.Join(webRoot, reqPath)

	// 检查文件是否存在
	info, err := os.Stat(filePath)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	// 如果是目录，尝试返回 index.html
	if info.IsDir() {
		filePath = filepath.Join(filePath, "index.html")
		if _, err = os.Stat(filePath); err != nil {
			c.Status(http.StatusNotFound)
			return
		}
	}

	// 最终防线：解析符号链接后验证真实路径在 webRoot 下
	realPath, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		c.Status(http.StatusForbidden)
		return
	}

	// 使用 filepath.Rel 检查是否逃逸（比 HasPrefix 更可靠，处理大小写不敏感文件系统）
	if !isPathUnder(realPath, webRoot) {
		c.Status(http.StatusForbidden)
		return
	}

	ext := strings.ToLower(filepath.Ext(realPath))

	// 根据文件类型设置缓存策略
	if ext == ".html" {
		serveHTMLWithVersion(c, realPath)
	} else {
		serveStaticWithCache(c, realPath, ext)
	}
}

// isPathUnder 检查 path 是否在 base 目录下（含 base 自身）
// 使用 filepath.Rel 而非 HasPrefix，正确处理大小写不敏感文件系统
func isPathUnder(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return false
	}
	// 相对路径不能以 .. 开头（表示逃逸）
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// serveHTMLWithVersion 处理 HTML 文件，替换版本号占位符
func serveHTMLWithVersion(c *gin.Context, filePath string) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}

	// 替换版本号占位符
	html := strings.ReplaceAll(string(content), "__VERSION__", version.Version)

	// HTML 不缓存，确保用户总能获取最新版本号引用
	c.Header("Cache-Control", "no-cache, must-revalidate")
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, html)
}

// serveStaticWithCache 处理静态资源，设置缓存策略
func serveStaticWithCache(c *gin.Context, filePath, ext string) {
	// 缓存策略：
	// - dev 版本：不缓存，方便开发调试
	// - manifest.json/favicon：短缓存（无版本号控制）
	// - 其他静态资源：长缓存（通过 URL 版本号刷新）
	fileName := filepath.Base(filePath)

	if version.Version == "dev" {
		// 开发环境：不缓存，避免前端修改看不到
		c.Header("Cache-Control", "no-cache, must-revalidate")
	} else if fileName == "manifest.json" || ext == ".ico" {
		// 元数据文件：1小时缓存 + 必须验证
		c.Header("Cache-Control", "public, max-age=3600, must-revalidate")
	} else {
		// 静态资源：1年缓存，immutable 表示内容不会变化（通过版本号刷新）
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	}

	// 使用 c.File() 替代手写 Open+io.Copy
	// 自动处理：Content-Type、Content-Length、HEAD、Range、If-Modified-Since/304
	c.File(filePath)
}
