package cursorauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
)

type toolCallbackResult struct {
	value *structpb.Struct
}

type pendingToolCall struct {
	session *sdkSession
	result  chan toolCallbackResult

	mu       sync.Mutex
	resolved bool
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
	if s == nil || s.runner == nil || request == nil || request.Msg == nil {
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
	if session.toolsAreDisabled() {
		return nil, connect.NewError(connect.CodePermissionDenied, errCustomToolsDisabled)
	}
	if !session.allowsTool(name) {
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
	pending := &pendingToolCall{session: session, result: make(chan toolCallbackResult, 1)}
	if err := s.runner.registerToolCall(callID, pending); err != nil {
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
	case <-session.toolsDisabled:
		// 提交已有结果早于禁用新调用；两者同时可读时交付已有结果。
		select {
		case result = <-pending.result:
		default:
			return nil, connect.NewError(connect.CodePermissionDenied, errCustomToolsDisabled)
		}
	case <-session.done:
		return nil, connect.NewError(connect.CodeCanceled, errors.New("cursor custom tool session ended"))
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeCanceled, context.Cause(ctx))
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
	if request.ToolChoice == "none" {
		return nil, nil
	}
	choice := strings.TrimSpace(request.ToolChoice)
	named := choice != "" && choice != "auto" && choice != "required"
	if request.Tools == nil && len(inherited) > 0 {
		if !named {
			return inherited, nil
		}
		if definition := inherited[choice]; definition != nil {
			return map[string]*sdkv1.CustomToolDefinition{choice: definition}, nil
		}
		return nil, fmt.Errorf("%w: tool_choice references undeclared tool %q", ErrInvalidRequest, choice)
	}
	if len(request.Tools) == 0 {
		return nil, nil
	}
	definitions := make(map[string]*sdkv1.CustomToolDefinition)
	for _, tool := range request.Tools {
		if named && tool.Name != choice {
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
	if named && len(definitions) == 0 {
		return nil, fmt.Errorf("%w: tool_choice references undeclared tool %q", ErrInvalidRequest, choice)
	}
	return definitions, nil
}

func callbackResult(result ToolResult) (*structpb.Struct, error) {
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
