package builtin

import "testing"

func TestCliproxyCodexCompletedResponsePreservesInnerObjectOrder(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"z":1,"a":{"y":2,"x":3}}`)
	got, err := cliproxyCodexCompletedResponse(raw)
	if err != nil {
		t.Fatalf("cliproxyCodexCompletedResponse() error = %v", err)
	}
	want := `{"type":"response.completed","response":{"z":1,"a":{"y":2,"x":3}}}`
	if string(got) != want {
		t.Fatalf("wrapped response = %s, want %s", got, want)
	}
}

func TestCliproxyCodexCompletedResponseRejectsTruncatedJSON(t *testing.T) {
	t.Parallel()
	if _, err := cliproxyCodexCompletedResponse([]byte(`{"z":1`)); err == nil {
		t.Fatal("accepted truncated JSON")
	}
}

func TestCliproxyCodexCompletedResponseDoesNotDoubleWrap(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)
	got, err := cliproxyCodexCompletedResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("already wrapped response was modified: %s", got)
	}
}
