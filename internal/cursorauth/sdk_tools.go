package cursorauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
)

/**
 * @brief 完成一次挂起的 SDK 工具回调。
 */
type toolCallbackResult struct {
	value *structpb.Struct ///< 已交回的客户端工具结果。
	err   error            ///< 工具已被本轮策略禁用时为权限错误。
}

/**
 * @brief 等待客户端结果的原生工具调用。
 */
type pendingToolCall struct {
	session *sdkSession             ///< 发出调用的 Run。
	name    string                  ///< SDK 注册的工具名。
	result  chan toolCallbackResult ///< 提交结果或撤销工具权限时移出待处理表，再通知回调。
}

type toolCallbackService struct {
	sdkv1connect.UnimplementedSdkCustomToolCallbackServiceHandler
	runner *SDKRunner
}

/**
 * @brief 工具调用在当前 Run 已被客户端禁用时的回调错误。
 */
var errCustomToolsDisabled = errors.New("cursor custom tools are disabled for this turn")

/**
 * @brief 判断进程内是否存在可以尝试续接的工具调用记录。
 * @return 至少有一条待处理或近期完成的工具调用时为 true。
 * @note 只用于跳过多余的请求转换；具体资格由 ToolContinuationModel 验证。
 */
func (r *SDKRunner) HasToolContinuation() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls) != 0 || len(r.completedCalls) != 0
}

/**
 * @brief 查询已知工具结果应恢复或重放的 Cursor 模型。
 * @param scope 候选渠道、已认证的调用方与客户端会话归属。
 * @param credential 候选渠道的当前凭证，必须非 nil。
 * @param results 下游请求尾部的工具结果，必须非空。
 * @return 原 Run 的实际模型；无匹配时为空。
 * @note 只读查询；提交结果时仍会重新验证会话。
 */
func (r *SDKRunner) ToolContinuationModel(scope SessionScope, credential *Credential, results []ToolResult) string {
	if !scope.reusable() {
		return ""
	}
	r.resumeMu.Lock()
	defer r.resumeMu.Unlock()
	turn, err := r.classifyCursorToolTurn(scope, strings.TrimSpace(credential.APIKey), "", results)
	if err != nil || turn.session == nil {
		return ""
	}
	return turn.session.model
}

/**
 * @brief 把 Cursor 自定义工具请求交给客户端，并等待同一会话提交结果。
 * @param ctx Bridge 对本次回调的上下文；取消时结束等待。
 * @param request 含 Agent 身份、工具名与参数的回调请求。
 * @return 客户端工具执行结果；回调失败时为 nil。
 * @return 请求无效、工具禁用或未声明、会话终止时返回 Connect 错误。
 */
func (s *toolCallbackService) CallCustomTool(
	ctx context.Context,
	request *connect.Request[sdkv1.CallCustomToolRequest],
) (*connect.Response[sdkv1.CallCustomToolResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("custom tool callback request is empty"))
	}
	message := request.Msg
	agentID := strings.TrimSpace(message.GetAgentId())
	name := strings.TrimSpace(message.GetToolName())
	if agentID == "" || name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("custom tool callback is missing agent_id or tool_name"))
	}
	session := s.runner.sessionByAgentID(agentID)
	if session == nil {
		return nil, connect.NewError(connect.CodeNotFound, ErrToolSessionNotFound)
	}
	if session.tools[name] == nil {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("cursor requested undeclared custom tool %q", name))
	}
	// Cursor's tool_call_id is scoped to its Agent and is not guaranteed to be
	// globally unique. The downstream protocols only return call_id, so expose a
	// process-unique ID and use the callback request itself for upstream routing.
	callID := newNativeToolCallID()
	argumentMap := map[string]any{}
	if message.GetArgs() != nil {
		argumentMap = message.GetArgs().AsMap()
	}
	arguments, err := json.Marshal(argumentMap)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("encode custom tool arguments: %w", err))
	}
	pending := &pendingToolCall{session: session, name: name, result: make(chan toolCallbackResult, 1)}
	if err := s.runner.registerToolCall(callID, pending); err != nil {
		if errors.Is(err, errCustomToolsDisabled) {
			return nil, connect.NewError(connect.CodePermissionDenied, err)
		}
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	defer s.runner.unregisterToolCall(callID, pending)

	event := Event{ToolCall: &ToolCall{ID: callID, Name: name, Arguments: arguments}}
	if !session.emit(event) {
		return nil, connect.NewError(connect.CodeCanceled, errors.New("cursor custom tool session ended"))
	}

	var result toolCallbackResult
	select {
	case result = <-pending.result:
	case <-session.done:
		return nil, connect.NewError(connect.CodeCanceled, errors.New("cursor custom tool session ended"))
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeCanceled, context.Cause(ctx))
	}
	if result.err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, result.err)
	}
	return connect.NewResponse(&sdkv1.CallCustomToolResponse{Result: result.value}), nil
}

/**
 * @brief 为当前 Bridge 注册进程内的已鉴权工具回调地址。
 * @param ctx 当前请求上下文。
 * @param client 当前 Bridge 客户端。
 * @param apiKey 当前请求的 User API Key，用于遮蔽 Bridge 诊断。
 * @return 已分类的注册错误，成功时为 nil。
 * @note 并发调用只会为同一 Bridge 注册一次。
 */
func (r *SDKRunner) ensureToolCallback(ctx context.Context, client *bridgeClient, apiKey string) error {
	r.callbackMu.Lock()
	defer r.callbackMu.Unlock()
	if r.callbackServer == nil {
		token, err := randomHexToken(32)
		if err != nil {
			return fmt.Errorf("create cursor custom-tool callback token: %w", err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("listen for cursor custom-tool callbacks: %w", err)
		}
		_, handler := sdkv1connect.NewSdkCustomToolCallbackServiceHandler(&toolCallbackService{runner: r})
		server := &http.Server{
			Handler:           requireCallbackToken(token, handler),
			ReadHeaderTimeout: 5 * time.Second,
		}
		r.callbackToken = token
		r.callbackURL = "http://" + listener.Addr().String()
		r.callbackServer = server
		r.callbackListener = listener
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				r.failAllSessions(fmt.Errorf("cursor custom-tool callback server: %w", err))
			}
		}()
	}
	if r.callbackClient == client {
		return nil
	}
	registerCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	started := time.Now()
	_, err := client.control.SetToolCallback(registerCtx, connect.NewRequest(&sdkv1.SetToolCallbackRequest{
		Url: r.callbackURL, AuthToken: r.callbackToken,
	}))
	if err != nil {
		return classifyBridgeOperationError("SetToolCallback", registerCtx, RequestTimeout, started, err, apiKey)
	}
	r.callbackClient = client
	return nil
}

func requireCallbackToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		got := []byte(request.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, request)
	})
}

func randomHexToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func newNativeToolCallID() string {
	token, err := randomHexToken(12)
	if err != nil {
		return fmt.Sprintf("call_%d", time.Now().UnixNano())
	}
	return "call_" + token
}

/**
 * @brief 转换客户端选中的工具声明，以便在 Local Agent 中注册。
 * @param request 已解析的客户端请求。
 * @param inherited 原 Run 的工具定义；显式声明工具时由本次请求覆盖。
 * @return Bridge 工具定义；客户端禁用工具时为 nil。
 * @return 客户端声明的 schema 或选择无效时返回 ErrInvalidRequest；协议编码失败时返回原始错误。
 */
func customToolDefinitions(request Request, inherited map[string]*sdkv1.CustomToolDefinition) (map[string]*sdkv1.CustomToolDefinition, error) {
	choice := strings.TrimSpace(request.ToolChoice)
	var selected []string
	switch {
	case request.toolChoiceNamed:
		if choice == "" {
			return nil, fmt.Errorf("%w: tool_choice must name a supported custom tool", ErrInvalidRequest)
		}
		selected = []string{choice}
	case choice == "none":
		return nil, nil
	case choice == "allowed_tools":
		selected = request.allowedToolNames
	case choice != "" && choice != "auto" && choice != "required":
		selected = []string{choice}
	}
	if selected == nil && request.Tools == nil {
		return inherited, nil
	}

	definitions := make(map[string]*sdkv1.CustomToolDefinition)
	for _, tool := range request.Tools {
		if selected != nil && !slices.Contains(selected, tool.Name) {
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(bytesTrimSpace(tool.Parameters), &schema); err != nil || schema == nil {
			return nil, fmt.Errorf("%w: cursor custom tool %q has an invalid input schema", ErrInvalidRequest, tool.Name)
		}
		inputSchema, err := structpb.NewStruct(schema)
		if err != nil {
			return nil, fmt.Errorf("cursor custom tool %q input schema: %w", tool.Name, err)
		}
		definition := &sdkv1.CustomToolDefinition{InputSchema: inputSchema}
		if description := strings.TrimSpace(tool.Description); description != "" {
			definition.Description = &description
		}
		definitions[tool.Name] = definition
	}
	for _, name := range selected {
		if request.Tools == nil {
			definitions[name] = inherited[name]
		}
		if definitions[name] == nil {
			return nil, fmt.Errorf("%w: tool_choice references undeclared tool %q", ErrInvalidRequest, name)
		}
	}
	return definitions, nil
}

/**
 * @brief 比较两份权限中的工具及其注册定义。
 * @param tools 本次请求允许的工具。
 * @param available 原 Run 可用的工具。
 * @return 每个请求工具均以相同定义存在时为 true。
 */
func toolDefinitionsSubset(tools, available map[string]*sdkv1.CustomToolDefinition) bool {
	for name, definition := range tools {
		if !proto.Equal(definition, available[name]) {
			return false
		}
	}
	return true
}

/**
 * @brief 把客户端工具结果编码成 Cursor SDK 回调对象。
 * @param result 已解析的工具执行结果。
 * @return 结构化回调对象；编码失败时为 nil。
 * @return 原生回调中的远程图片或无效 base64 返回 ErrInvalidRequest，其余错误为回调对象编码失败。
 */
func callbackResult(result ToolResult) (*structpb.Struct, error) {
	if len(result.Content) > 0 {
		content := make([]any, 0, len(result.Content))
		for _, value := range result.Content {
			block, _ := value.(map[string]any)
			if image, isImage := imageFromContentBlock(block); isImage {
				if image.Data == "" && image.URL != "" {
					return nil, fmt.Errorf("%w: %s", ErrInvalidRequest, ErrURLImageUnsupported)
				}
				if !validToolImageBase64(image.Data) {
					return nil, fmt.Errorf("%w: tool result image must contain valid base64 data", ErrInvalidRequest)
				}
				part := map[string]any{"type": "image", "data": image.Data}
				if image.MimeType != "" {
					part["mimeType"] = image.MimeType
				}
				content = append(content, part)
				continue
			}
			text, ok := block["text"].(string)
			kind := strings.ToLower(asString(block["type"]))
			if !ok || (kind != "text" && kind != "input_text" && kind != "output_text") {
				text = stringifyToolOutput(value)
			}
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		return structpb.NewStruct(map[string]any{"content": content, "isError": result.IsError})
	}
	if !result.IsError {
		var object map[string]any
		if json.Unmarshal([]byte(result.Output), &object) == nil && object != nil {
			return structpb.NewStruct(object)
		}
	}
	key := "value"
	if result.IsError {
		key = "error"
	}
	return structpb.NewStruct(map[string]any{key: result.Output})
}

/**
 * @brief 检查原生工具图片的 base64 语法，避免提交后消耗暂停中的调用。
 * @param data 图片的 base64 字节串，可带尾部填充。
 * @return 语法有效且解码结果非空时为 true。
 */
func validToolImageBase64(data string) bool {
	encoding := base64.RawStdEncoding
	if _, padding, padded := strings.Cut(data, "="); padded {
		if strings.Trim(padding, "=\r\n") != "" {
			return false
		}
		encoding = base64.StdEncoding
	}
	size, err := io.Copy(io.Discard, base64.NewDecoder(encoding, strings.NewReader(data)))
	return err == nil && size > 0
}
