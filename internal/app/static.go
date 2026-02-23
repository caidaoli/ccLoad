package app

import (
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"strings"

	"ccLoad/internal/version"

	"github.com/gin-gonic/gin"
)

// embedFS 是嵌入的 web 静态资源文件系统
// 通过 SetEmbedFS 在 main 包中初始化
var embedFS fs.FS

// SetEmbedFS 设置嵌入的静态资源文件系统
// embedRoot: 嵌入的 embed.FS
// subDir: 子目录名称（如 "web"），因为 //go:embed web 会保留 web/ 前缀
func SetEmbedFS(embedRoot fs.FS, subDir string) {
	subFS, err := fs.Sub(embedRoot, subDir)
	if err != nil {
		log.Fatalf("[FATAL] 无法访问嵌入的 %s 目录: %v", subDir, err)
	}
	embedFS = subFS
}

// setupStaticFiles 配置静态文件服务
// - HTML 文件：不缓存，动态替换版本号占位符
// - CSS/JS/字体：长缓存（1年），依赖版本号刷新
// - dev 版本：不缓存，方便开发调试
// - 支持 zstd 压缩（根据 Accept-Encoding 自动启用）
func setupStaticFiles(r *gin.Engine) {
	// 检查嵌入的文件系统是否已初始化
	if embedFS == nil {
		if isTestMode() {
			log.Printf("[WARN] 嵌入文件系统未初始化（测试环境忽略）")
			return
		}
		log.Fatalf("[FATAL] 嵌入文件系统未初始化，请在 main 中调用 SetEmbedFS")
	}

	// 使用路由组为静态文件启用 zstd 压缩
	// 已压缩的文件类型（图片、字体等）在中间件内自动跳过
	webGroup := r.Group("/web", ZstdMiddleware())
	webGroup.GET("/*filepath", serveStaticFile)
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
	if embedFS == nil {
		c.Status(http.StatusNotFound)
		return
	}

	// Gin wildcard 参数带前导斜杠，如 "/index.html"
	reqPath := c.Param("filepath")

	// 去除前导斜杠，确保是相对路径
	reqPath = strings.TrimPrefix(reqPath, "/")

	// Clean 处理 .. 和多余的斜杠
	reqPath = path.Clean(reqPath)

	// 防止路径遍历：Clean 后仍以 .. 开头说明试图逃逸
	if reqPath == ".." || strings.HasPrefix(reqPath, "../") {
		c.Status(http.StatusForbidden)
		return
	}

	// 空路径时默认返回 index.html
	if reqPath == "." || reqPath == "" {
		reqPath = "index.html"
	}

	// 检查文件是否存在
	info, err := fs.Stat(embedFS, reqPath)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	// 如果是目录，尝试返回 index.html
	if info.IsDir() {
		reqPath = path.Join(reqPath, "index.html")
		if _, err = fs.Stat(embedFS, reqPath); err != nil {
			c.Status(http.StatusNotFound)
			return
		}
	}

	ext := strings.ToLower(path.Ext(reqPath))

	// 根据文件类型设置缓存策略
	if ext == ".html" {
		serveHTMLWithVersion(c, reqPath)
	} else {
		serveStaticWithCache(c, reqPath, ext)
	}
}

// serveHTMLWithVersion 处理 HTML 文件，替换版本号占位符
func serveHTMLWithVersion(c *gin.Context, filePath string) {
	content, err := fs.ReadFile(embedFS, filePath)
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
	fileName := path.Base(filePath)

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

	// 读取文件内容
	content, err := fs.ReadFile(embedFS, filePath)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}

	// 设置 Content-Type
	contentType := getContentType(ext)
	c.Header("Content-Type", contentType)
	c.Data(http.StatusOK, contentType, content)
}

// getContentType 根据文件扩展名返回 MIME 类型
func getContentType(ext string) string {
	switch ext {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".eot":
		return "application/vnd.ms-fontobject"
	default:
		return "application/octet-stream"
	}
}
