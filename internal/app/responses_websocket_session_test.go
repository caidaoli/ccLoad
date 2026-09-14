package app

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesWebsocketSessionDoesNotInheritInstructions(t *testing.T) {
	session := newResponsesWebsocketSession(1 << 20)

	first, err := session.normalizeRequest([]byte(`{"type":"response.create","model":"gpt-5.4","instructions":"first-turn","input":[{"type":"message","role":"user","content":"first"}]}`))
	if err != nil {
		t.Fatalf("normalize first request: %v", err)
	}
	session.commit(first, responsesWebsocketTurnResult{completedResponseID: "resp-1"})

	second, err := session.normalizeRequest([]byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"message","role":"user","content":"second"}]}`))
	if err != nil {
		t.Fatalf("normalize incremental request: %v", err)
	}
	if gjson.GetBytes(second, "instructions").Exists() {
		t.Fatalf("incremental request inherited instructions: %s", second)
	}

	fullReplacement, err := session.normalizeRequest([]byte(`{"type":"response.create","model":"gpt-5.4","input":[{"type":"message","role":"assistant","content":"prior"},{"type":"message","role":"user","content":"replacement"}]}`))
	if err != nil {
		t.Fatalf("normalize full replacement request: %v", err)
	}
	if gjson.GetBytes(fullReplacement, "instructions").Exists() {
		t.Fatalf("full replacement request inherited instructions: %s", fullReplacement)
	}
}

func TestResponsesWebsocketSessionPreservesExplicitInstructions(t *testing.T) {
	session := newResponsesWebsocketSession(1 << 20)

	first, err := session.normalizeRequest([]byte(`{"type":"response.create","model":"gpt-5.4","instructions":"first-turn","input":[{"type":"message","role":"user","content":"first"}]}`))
	if err != nil {
		t.Fatalf("normalize first request: %v", err)
	}
	session.commit(first, responsesWebsocketTurnResult{completedResponseID: "resp-1"})

	second, err := session.normalizeRequest([]byte(`{"type":"response.create","previous_response_id":"resp-1","instructions":"second-turn","input":[{"type":"message","role":"user","content":"second"}]}`))
	if err != nil {
		t.Fatalf("normalize explicit request: %v", err)
	}
	if got := gjson.GetBytes(second, "instructions").String(); got != "second-turn" {
		t.Fatalf("instructions = %q, want %q", got, "second-turn")
	}
}

func TestResponsesWebsocketSessionKeepsHTTPInstructionCompatibility(t *testing.T) {
	session := newResponsesWebsocketSession(1 << 20)

	first, _, localContinuation, err := session.normalizeHTTPRequests([]byte(`{"model":"gpt-5.4","instructions":"first-turn","input":[{"type":"message","role":"user","content":"first"}]}`))
	if err != nil {
		t.Fatalf("normalize first HTTP request: %v", err)
	}
	if !localContinuation {
		t.Fatal("first HTTP request was not treated as a local continuation")
	}
	session.commit(first, responsesWebsocketTurnResult{completedResponseID: "resp-1"})

	second, _, localContinuation, err := session.normalizeHTTPRequests([]byte(`{"previous_response_id":"resp-1","input":[{"type":"message","role":"user","content":"second"}]}`))
	if err != nil {
		t.Fatalf("normalize HTTP continuation: %v", err)
	}
	if !localContinuation {
		t.Fatal("HTTP continuation was not treated as local")
	}
	if got := gjson.GetBytes(second, "instructions").String(); got != "first-turn" {
		t.Fatalf("HTTP instructions = %q, want %q", got, "first-turn")
	}
}
