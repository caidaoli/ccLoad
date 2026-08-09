package httpwire

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
)

func TestOrderedRequestConnPreservesBodyAndAppliesWireOrder(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	ordered := NewOrderedRequestConn(client, func(_, target string) []string {
		if !strings.HasPrefix(target, "/v1/messages") {
			return nil
		}
		return []string{"Accept", "Authorization", "Connection", "Host", "Content-Length"}
	})

	errCh := make(chan error, 1)
	go func() {
		_, err := ordered.Write([]byte("POST /v1/messages?beta=true HTTP/1.1\r\nHost: api.anthropic.com\r\nContent-Length: 2\r\nAuthorization: Bearer token\r\nAccept: application/json\r\nConnection: keep-alive\r\n\r\n{}"))
		errCh <- err
	}()

	reader := bufio.NewReader(server)
	want := "POST /v1/messages?beta=true HTTP/1.1\r\n" +
		"Accept: application/json\r\n" +
		"Authorization: Bearer token\r\n" +
		"Connection: keep-alive\r\n" +
		"Host: api.anthropic.com\r\n" +
		"Content-Length: 2\r\n\r\n{}"
	got := make([]byte, len(want))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("read ordered request: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("write ordered request: %v", err)
	}
	if string(got) != want {
		t.Fatalf("ordered request = %q, want %q", got, want)
	}
}
