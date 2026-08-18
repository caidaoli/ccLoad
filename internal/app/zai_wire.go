package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/zaiauth"

	"github.com/google/uuid"
)

// Z.ai Coding Plan wire contract.
//
// ZCode never calls the public Coding Plan origin directly: it rewrites the
// endpoint through the routing table published by zcode.z.ai and stamps every
// request with its client identity plus a metadata.user_id device fingerprint.
// ccLoad replicates all three so Coding Plan traffic proxied here is the same
// traffic ZCode itself would have sent.

// isZAICodingPlanRequest reports whether this attempt is a Coding Plan
// Anthropic Messages call owned by a Z.ai channel.
func isZAICodingPlanRequest(cfg *model.Config, upstream protocol.Protocol, requestPath string) bool {
	return cfg != nil && cfg.UsesZAIOAuth() && isAnthropicMessagesRequest(upstream, requestPath)
}

// zaiRequestIdentity is ZCode's metadata.user_id payload. Field order matches
// the official client because the value travels as an opaque JSON string.
type zaiRequestIdentity struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

// finalizeZAICodingPlanBody replaces the caller's metadata.user_id with the
// channel's ZCode fingerprint. A foreign client fingerprint (Claude Code's, for
// example) must never reach the Coding Plan upstream.
func finalizeZAICodingPlanBody(body []byte, cfg *model.Config) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, errors.New("finalize z.ai Coding Plan request: invalid JSON body")
	}
	identity, err := json.Marshal(zaiRequestIdentity{
		DeviceID:  zaiDeviceID(cfg),
		SessionID: resolveZAISessionID(body, cfg),
	})
	if err != nil {
		return nil, errors.New("finalize z.ai Coding Plan request: invalid identity")
	}
	metadata, _ := request["metadata"].(map[string]any)
	if metadata == nil {
		metadata = make(map[string]any, 1)
	}
	metadata["user_id"] = string(identity)
	request["metadata"] = metadata
	finalized, err := json.Marshal(request)
	if err != nil {
		return nil, errors.New("finalize z.ai Coding Plan request: encode failed")
	}
	return finalized, nil
}

// injectZAICodingPlanHeaders rebuilds the request headers as ZCode sends them.
// It runs after the channel's custom header rules so the client identity cannot
// be rewritten by a rule, matching the Codex contract.
func injectZAICodingPlanHeaders(req *http.Request, cfg *model.Config, apiKey string, body []byte, incoming http.Header) {
	if req == nil {
		return
	}
	accept := anthropicHeaderValue(incoming, "Accept")
	if accept == "" {
		accept = "application/json"
	}
	anthropicVersion := anthropicHeaderValue(incoming, "anthropic-version")
	if anthropicVersion == "" {
		anthropicVersion = "2023-06-01"
	}
	for name := range req.Header {
		delete(req.Header, name)
	}
	setRawHeader(req.Header, "Accept", accept)
	setRawHeader(req.Header, "Content-Type", "application/json")
	setRawHeader(req.Header, "anthropic-version", anthropicVersion)
	// ZCode's Anthropic provider authenticates with x-api-key only.
	setRawHeader(req.Header, "x-api-key", strings.TrimSpace(apiKey))
	for _, entry := range zaiauth.SourceHeaders() {
		setRawHeader(req.Header, entry[0], entry[1])
	}
	setRawHeader(req.Header, "x-request-id", uuid.NewString())
	setRawHeader(req.Header, "x-zcode-trace-id", uuid.NewString())
	setRawHeader(req.Header, "x-session-id", resolveZAISessionID(body, cfg))
}

// resolveZAISessionID keeps one conversation on a single session identifier:
// the caller's own session when it sends one, otherwise a value derived from
// the channel fingerprint and the opening user message.
func resolveZAISessionID(body []byte, cfg *model.Config) string {
	if sessionID := anthropicSessionIDFromBody(body); sessionID != "" {
		return sessionID
	}
	if deviceID := zaiDeviceID(cfg); deviceID != "" {
		request, _ := decodeAnthropicRequest(body)
		messages, _ := request["messages"].([]any)
		return anthropicStableSessionID(deviceID, anthropicFirstUserText(messages))
	}
	return uuid.NewString()
}

func zaiDeviceID(cfg *model.Config) string {
	if cfg == nil {
		return ""
	}
	if deviceID := strings.TrimSpace(cfg.ZAIDeviceID); deviceID != "" {
		return deviceID
	}
	credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return ""
	}
	return credential.DeviceID
}

// zaiCredentialRejected reports whether an upstream response means the Coding
// Plan key itself was refused. Z.ai answers 401 with its own error envelope.
func zaiCredentialRejected(status int) bool {
	return status == http.StatusUnauthorized
}
