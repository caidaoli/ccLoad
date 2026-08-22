package cursorauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

const bridgeReadyPrefix = "cursor-sdk-bridge ready "

type bridgeReady struct {
	SchemaVersion int    `json:"schemaVersion"`
	Transport     string `json:"transport"`
	Protocol      string `json:"protocol"`
	URL           string `json:"url"`
	AuthTokenFile string `json:"authTokenFile"`
	PID           int    `json:"pid"`
	ServerVersion string `json:"serverVersion"`
}

func parseBridgeReadyLine(line string) (*bridgeReady, bool, error) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, bridgeReadyPrefix) {
		return nil, false, nil
	}
	var ready bridgeReady
	decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(line, bridgeReadyPrefix)))
	if err := decoder.Decode(&ready); err != nil {
		return nil, true, fmt.Errorf("decode cursor-sdk-bridge ready line: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, true, errors.New("cursor-sdk-bridge ready line contains trailing JSON")
	}
	if ready.SchemaVersion != 1 || ready.Transport != "tcp" || ready.Protocol != "connect" {
		return nil, true, fmt.Errorf(
			"unsupported cursor-sdk-bridge discovery contract: schema=%d transport=%q protocol=%q",
			ready.SchemaVersion, ready.Transport, ready.Protocol,
		)
	}
	if ready.PID <= 0 {
		return nil, true, errors.New("cursor-sdk-bridge ready line has invalid pid")
	}
	if strings.TrimSpace(ready.AuthTokenFile) == "" || !filepath.IsAbs(ready.AuthTokenFile) {
		return nil, true, errors.New("cursor-sdk-bridge ready line has invalid authTokenFile")
	}
	if err := validateBridgeURL(ready.URL); err != nil {
		return nil, true, err
	}
	return &ready, true, nil
}

func validateBridgeURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse cursor-sdk-bridge URL: %w", err)
	}
	if u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return fmt.Errorf("unsafe cursor-sdk-bridge URL %q", raw)
	}
	host := u.Hostname()
	port := u.Port()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() || port == "" {
		return fmt.Errorf("cursor-sdk-bridge URL must use a literal loopback address and port: %q", raw)
	}
	return nil
}
