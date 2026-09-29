package app

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestCodexSSEFramingRepairsBlankLineBetweenEventAndData(t *testing.T) {
	before := sseFramingRepairs.Load()
	input := "event: response.created\n\ndata: {\"type\":\"response.created\"}\n\nevent: response.in_progress\n\ndata: {\"type\":\"response.in_progress\"}\n\n"
	r := wrapCodexSSEBody(io.NopCloser(strings.NewReader(input)))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}

	expected := "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n"
	if string(got) != expected {
		t.Fatalf("framing repair failed:\ngot:\n%q\nwant:\n%q", string(got), expected)
	}
	after := sseFramingRepairs.Load()
	if after < before+2 {
		t.Fatalf("expected at least 2 repairs, got %d (before: %d, after: %d)", after-before, before, after)
	}
}

func TestCodexSSEFramingRepairsMultipleBlankLinesBetweenEventAndData(t *testing.T) {
	input := "event: response.created\n\n\ndata: {\"type\":\"response.created\"}\n\n\n"
	r := wrapCodexSSEBody(io.NopCloser(strings.NewReader(input)))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}

	expected := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n\n"
	if string(got) != expected {
		t.Fatalf("multiple blank lines repair failed:\ngot:\n%q\nwant:\n%q", string(got), expected)
	}
}

func TestCodexSSEFramingRepairsCRLFBlankLineBetweenEventAndData(t *testing.T) {
	input := "event: response.created\r\n\r\ndata: {\"type\":\"response.created\"}\r\n\r\n"
	r := wrapCodexSSEBody(io.NopCloser(strings.NewReader(input)))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}

	expected := "event: response.created\r\ndata: {\"type\":\"response.created\"}\r\n\r\n"
	if string(got) != expected {
		t.Fatalf("CRLF framing repair failed:\ngot:\n%q\nwant:\n%q", string(got), expected)
	}
}

func TestCodexSSEFramingPreservesCleanStream(t *testing.T) {
	before := sseFramingRepairs.Load()
	clean := "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	r := wrapCodexSSEBody(io.NopCloser(strings.NewReader(clean)))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}

	if string(got) != clean {
		t.Fatalf("clean stream altered:\ngot:\n%q\nwant:\n%q", string(got), clean)
	}
	after := sseFramingRepairs.Load()
	if after != before {
		t.Fatalf("clean stream should not trigger repairs, before: %d, after: %d", before, after)
	}
}

func TestCodexSSEFramingPreservesGluedRepair(t *testing.T) {
	glued := "event: response.created\ndata: {\"type\":\"response.created\"}\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	r := wrapCodexSSEBody(io.NopCloser(strings.NewReader(glued)))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}

	expected := "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	if string(got) != expected {
		t.Fatalf("glued events repair failed:\ngot:\n%q\nwant:\n%q", string(got), expected)
	}
}

type byteByByteReader struct {
	data []byte
	off  int
}

func (b *byteByByteReader) Read(p []byte) (int, error) {
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = b.data[b.off]
	b.off++
	return 1, nil
}

func TestCodexSSEFramingByteByByte(t *testing.T) {
	input := "event: response.created\n\ndata: {\"type\":\"response.created\"}\n\n"
	r := wrapCodexSSEBody(io.NopCloser(&byteByByteReader{data: []byte(input)}))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}

	expected := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"
	if string(got) != expected {
		t.Fatalf("byte-by-byte repair failed:\ngot:\n%q\nwant:\n%q", string(got), expected)
	}
}

func TestCodexSSEFramingByteByByteCRLF(t *testing.T) {
	input := "event: response.created\r\n\r\ndata: {\"type\":\"response.created\"}\r\n\r\n"
	r := wrapCodexSSEBody(io.NopCloser(&byteByByteReader{data: []byte(input)}))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}

	expected := "event: response.created\r\ndata: {\"type\":\"response.created\"}\r\n\r\n"
	if string(got) != expected {
		t.Fatalf("byte-by-byte CRLF repair failed:\ngot:\n%q\nwant:\n%q", string(got), expected)
	}
}

func TestCodexSSEFramingRealWorldHybgzsPayload(t *testing.T) {
	// Exact pattern from log 98592:
	// event: ...\n\ndata: ...\n\n\n
	var buf bytes.Buffer
	buf.WriteString("event: response.created\n\ndata: {\"type\":\"response.created\",\"id\":\"test-1\"}\n\n\n")
	buf.WriteString("event: response.in_progress\n\ndata: {\"type\":\"response.in_progress\",\"id\":\"test-1\"}\n\n\n")
	buf.WriteString("event: response.output_item.added\n\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\"}}\n\n\n")
	buf.WriteString("event: response.reasoning_summary_text.delta\n\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"hello\"}\n\n\n")

	r := wrapCodexSSEBody(io.NopCloser(bytes.NewReader(buf.Bytes())))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}

	// Verify that in the repaired output, every "event: response." is immediately followed by "\ndata:"
	s := string(got)
	if strings.Contains(s, "event: response.created\n\n") ||
		strings.Contains(s, "event: response.in_progress\n\n") ||
		strings.Contains(s, "event: response.output_item.added\n\n") ||
		strings.Contains(s, "event: response.reasoning_summary_text.delta\n\n") {
		t.Fatalf("output still contains blank line after event header:\n%s", s)
	}

	if !strings.Contains(s, "event: response.created\ndata:") ||
		!strings.Contains(s, "event: response.in_progress\ndata:") ||
		!strings.Contains(s, "event: response.output_item.added\ndata:") ||
		!strings.Contains(s, "event: response.reasoning_summary_text.delta\ndata:") {
		t.Fatalf("output missing joined event/data pair:\n%s", s)
	}
}
