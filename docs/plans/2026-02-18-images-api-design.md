# Images API 代理支持实施计划

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 支持 `/v1/images/generations` 和 `/v1/images/edits` 两个 OpenAI Images API 端点的透明代理。

**Architecture:** 最小化路径扩展方案——仅修改路由配置和请求解析逻辑，完全复用现有的渠道选择、故障切换、计费架构。核心改动：OpenAI PathPatterns 增加 images 路径、parseIncomingRequest 增加 multipart model 提取、body 大小按路径区分。

**Tech Stack:** Go, mime/multipart (stdlib), gin, sonic (JSON)

---

## Task 1: 路由匹配 — 增加 Images 路径到 OpenAI 渠道

**Files:**
- Modify: `internal/util/channel_types.go:34` (OpenAI PathPatterns)
- Test: `internal/util/channel_types_test.go`

**Step 1: 写失败测试**

在 `internal/util/channel_types_test.go` 的 `TestDetectChannelTypeFromPath` 测试表中增加 images 路径用例：

```go
// OpenAI Images paths
{"OpenAI Images Generations", "/v1/images/generations", ChannelTypeOpenAI},
{"OpenAI Images Edits", "/v1/images/edits", ChannelTypeOpenAI},
{"OpenAI Images Variations", "/v1/images/variations", ChannelTypeOpenAI},
```

**Step 2: 运行测试确认失败**

```bash
go test -tags go_json ./internal/util/ -run TestDetectChannelTypeFromPath -v
```

预期：FAIL — images 路径返回 `""` 而非 `"openai"`

**Step 3: 实现最小改动**

在 `internal/util/channel_types.go:34` 的 OpenAI PathPatterns 中增加 `"/v1/images/"`:

```go
PathPatterns: []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/images/"},
```

**Step 4: 运行测试确认通过**

```bash
go test -tags go_json ./internal/util/ -run TestDetectChannelTypeFromPath -v
```

预期：PASS

**Step 5: 提交**

```bash
git add internal/util/channel_types.go internal/util/channel_types_test.go
git commit -m "feat: add /v1/images/ path to OpenAI channel routing"
```

---

## Task 2: Body 大小按路径区分

**Files:**
- Modify: `internal/config/defaults.go` (增加 DefaultMaxImageBodyBytes 常量)
- Modify: `internal/app/proxy_handler.go:58-78` (parseIncomingRequest body 大小逻辑)
- Test: `internal/app/proxy_handler_test.go`

**Step 1: 写失败测试**

在 `internal/app/proxy_handler_test.go` 增加测试：大于 10MB 小于 20MB 的 images 请求不应报错。

```go
// TestParseIncomingRequest_ImagesLargerBodyAllowed 测试 images 路径允许更大的请求体
func TestParseIncomingRequest_ImagesLargerBodyAllowed(t *testing.T) {
	// 不设置 CCLOAD_MAX_BODY_BYTES，使用默认值
	// 创建 15MB 的 multipart 请求体（超过默认 10MB，但在 images 20MB 限制内）
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("model", "gpt-image-1")
	_ = writer.WriteField("prompt", "test")
	// 添加大尺寸图片数据（约 15MB）
	part, _ := writer.CreateFormFile("image", "test.png")
	largeData := make([]byte, 15*1024*1024)
	_, _ = part.Write(largeData)
	writer.Close()

	req := newRequest(http.MethodPost, "/v1/images/edits", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	c, _ := newTestContext(t, req)

	model, _, _, err := parseIncomingRequest(c)
	if err != nil {
		t.Fatalf("images 路径 15MB 请求体不应报错, 实际: %v", err)
	}
	if model != "gpt-image-1" {
		t.Fatalf("模型名应为 gpt-image-1, 实际: %s", model)
	}
}
```

需要在测试文件头部增加 `"mime/multipart"` import。

**Step 2: 运行测试确认失败**

```bash
go test -tags go_json ./internal/app/ -run TestParseIncomingRequest_ImagesLargerBodyAllowed -v
```

预期：FAIL — 因为当前 parseIncomingRequest 不区分路径，且 multipart model 提取不支持

**Step 3: 实现改动**

3a. 在 `internal/config/defaults.go` 增加常量：

```go
// DefaultMaxImageBodyBytes Images API 默认最大请求体字节数（支持图片上传）
DefaultMaxImageBodyBytes = 20 * 1024 * 1024 // 20MB
```

3b. 修改 `internal/app/proxy_handler.go` 的 `parseIncomingRequest` 函数，在确定 maxBody 时按路径区分：

```go
func parseIncomingRequest(c *gin.Context) (string, []byte, bool, error) {
	requestPath := c.Request.URL.Path
	requestMethod := c.Request.Method

	// 读取请求体（带上限，防止大包打爆内存）
	// 默认 10MB，images 路径 20MB，可通过 CCLOAD_MAX_BODY_BYTES 覆盖
	maxBody := int64(config.DefaultMaxBodyBytes)
	if strings.HasPrefix(requestPath, "/v1/images/") {
		maxBody = int64(config.DefaultMaxImageBodyBytes)
	}
	if v := os.Getenv("CCLOAD_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxBody = int64(n)
		}
	}
	// ... 后续逻辑不变
```

需要在 proxy_handler.go import 中增加 `"strings"`（检查是否已有）。

**Step 4: 运行测试确认通过**

注意：此步骤测试同时依赖 Task 3 的 multipart model 提取，因此此测试需要在 Task 3 完成后才能完全通过。可以先验证 body 大小不报错的部分。

```bash
go test -tags go_json ./internal/app/ -run TestParseIncomingRequest -v
```

**Step 5: 提交**（与 Task 3 合并提交）

---

## Task 3: Multipart Model 提取

**Files:**
- Modify: `internal/app/proxy_handler.go:80-92` (parseIncomingRequest model 提取逻辑)
- Test: `internal/app/proxy_handler_test.go`

**Step 1: 写失败测试**

在 `internal/app/proxy_handler_test.go` 增加 multipart 和 images JSON 请求的测试：

```go
// TestParseIncomingRequest_MultipartModel 测试 multipart/form-data 中提取 model
func TestParseIncomingRequest_MultipartModel(t *testing.T) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("model", "dall-e-2")
	_ = writer.WriteField("prompt", "a cute cat")
	_ = writer.WriteField("n", "1")
	writer.Close()

	req := newRequest(http.MethodPost, "/v1/images/edits", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	c, _ := newTestContext(t, req)

	model, _, isStreaming, err := parseIncomingRequest(c)
	if err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	if model != "dall-e-2" {
		t.Fatalf("模型名应为 dall-e-2, 实际: %s", model)
	}
	if isStreaming {
		t.Fatal("images 请求不应为流式")
	}
}

// TestParseIncomingRequest_ImagesJSON 测试 images/generations 的标准 JSON 请求
func TestParseIncomingRequest_ImagesJSON(t *testing.T) {
	body := `{"model":"gpt-image-1","prompt":"a white cat","n":1,"size":"1024x1024"}`
	req := newRequest(http.MethodPost, "/v1/images/generations", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	c, _ := newTestContext(t, req)

	model, _, isStreaming, err := parseIncomingRequest(c)
	if err != nil {
		t.Fatalf("不期望错误: %v", err)
	}
	if model != "gpt-image-1" {
		t.Fatalf("模型名应为 gpt-image-1, 实际: %s", model)
	}
	if isStreaming {
		t.Fatal("images 请求不应为流式")
	}
}
```

**Step 2: 运行测试确认失败**

```bash
go test -tags go_json ./internal/app/ -run "TestParseIncomingRequest_MultipartModel|TestParseIncomingRequest_ImagesJSON" -v
```

预期：MultipartModel FAIL（无法提取 model），ImagesJSON PASS（现有 JSON 解析已支持）

**Step 3: 实现 multipart model 提取**

修改 `internal/app/proxy_handler.go` 的 `parseIncomingRequest` 函数。在 JSON unmarshal 之后、model 为空时增加 multipart 解析分支：

```go
	var reqModel struct {
		Model string `json:"model"`
	}
	_ = sonic.Unmarshal(all, &reqModel)

	// multipart/form-data 支持：当 JSON 解析无 model 时，尝试从 multipart 表单字段提取
	if reqModel.Model == "" {
		if ct := c.Request.Header.Get("Content-Type"); ct != "" {
			mediaType, params, _ := mime.ParseMediaType(ct)
			if mediaType == "multipart/form-data" {
				if boundary := params["boundary"]; boundary != "" {
					reqModel.Model = extractModelFromMultipart(all, boundary)
				}
			}
		}
	}
```

新增辅助函数 `extractModelFromMultipart`（放在 `proxy_handler.go` 同文件中）：

```go
// extractModelFromMultipart 从 multipart/form-data 原始字节中提取 model 字段
func extractModelFromMultipart(body []byte, boundary string) string {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			val, err := io.ReadAll(io.LimitReader(part, 256))
			_ = part.Close()
			if err == nil {
				return strings.TrimSpace(string(val))
			}
			break
		}
		_ = part.Close()
	}
	return ""
}
```

需要在 proxy_handler.go import 中增加：
- `"bytes"`
- `"mime"`
- `"mime/multipart"`
- `"strings"`（如 Task 2 未已加）

**Step 4: 运行全部 parseIncomingRequest 测试确认通过**

```bash
go test -tags go_json ./internal/app/ -run TestParseIncomingRequest -v
```

预期：全部 PASS

**Step 5: 提交（合并 Task 2 和 Task 3）**

```bash
git add internal/config/defaults.go internal/app/proxy_handler.go internal/app/proxy_handler_test.go
git commit -m "feat: support multipart model extraction and larger body for images API"
```

---

## Task 4: 全量测试与 lint 验证

**Step 1: 运行完整测试套件**

```bash
go test -tags go_json ./internal/... -v
```

预期：全部 PASS

**Step 2: 运行竞态检测**

```bash
go test -tags go_json -race ./internal/...
```

预期：无 data race

**Step 3: 运行 lint 检查**

```bash
golangci-lint run ./...
```

预期：零警告

**Step 4: 构建验证**

```bash
go build -tags go_json -ldflags "\
  -X ccLoad/internal/version.Version=$(git describe --tags --always) \
  -X ccLoad/internal/version.Commit=$(git rev-parse --short HEAD) \
  -X 'ccLoad/internal/version.BuildTime=$(date '+%Y-%m-%d %H:%M:%S %z')' \
  -X ccLoad/internal/version.BuiltBy=$(whoami)" -o ccload .
```

预期：编译成功

**Step 5: 最终提交（如有 lint 修复）**

---

## 改动总结

| 文件 | 改动类型 | 描述 |
|------|----------|------|
| `internal/util/channel_types.go` | 1 行修改 | OpenAI PathPatterns 增加 `"/v1/images/"` |
| `internal/util/channel_types_test.go` | ~3 行增加 | images 路径路由测试用例 |
| `internal/config/defaults.go` | ~2 行增加 | `DefaultMaxImageBodyBytes` 常量 |
| `internal/app/proxy_handler.go` | ~25 行增加 | 路径区分 body 上限 + multipart model 提取 + `extractModelFromMultipart` 函数 |
| `internal/app/proxy_handler_test.go` | ~40 行增加 | multipart model 提取测试 + images JSON 测试 + 大 body 测试 |
