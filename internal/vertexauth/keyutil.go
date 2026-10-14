package vertexauth

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"strings"
)

// NormalizeServiceAccountMap returns a copy of the service account whose
// private_key is a valid "RSA PRIVATE KEY" PEM block. Keys pasted through
// terminals or chat tools (CRLF, ANSI escapes, lost line breaks) are repaired.
func NormalizeServiceAccountMap(serviceAccount map[string]any) (map[string]any, error) {
	if serviceAccount == nil {
		return nil, errors.New("service account payload is empty")
	}
	privateKey, _ := serviceAccount["private_key"].(string)
	if strings.TrimSpace(privateKey) == "" {
		return nil, errors.New("service account missing private_key")
	}
	normalized, err := sanitizePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	clone := maps.Clone(serviceAccount)
	clone["private_key"] = normalized
	return clone, nil
}

func parseRSAPrivateKey(normalizedPEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(normalizedPEM))
	if block == nil {
		return nil, errors.New("private_key pem decode failed")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private_key invalid rsa: %w", err)
	}
	return key, nil
}

func sanitizePrivateKey(raw string) (string, error) {
	privateKey := strings.ReplaceAll(raw, "\r\n", "\n")
	privateKey = strings.ReplaceAll(privateKey, "\r", "\n")
	privateKey = stripANSIEscape(privateKey)
	privateKey = strings.ToValidUTF8(privateKey, "")
	privateKey = strings.TrimSpace(privateKey)

	normalized := privateKey
	if block, _ := pem.Decode([]byte(privateKey)); block == nil {
		reconstructed, err := rebuildPEM(privateKey)
		if err != nil {
			return "", fmt.Errorf("private_key is not valid pem: %w", err)
		}
		normalized = reconstructed
	}
	block, _ := pem.Decode([]byte(normalized))
	if block == nil {
		return "", errors.New("private_key pem decode failed")
	}
	rsaBlock, err := ensureRSAPrivateKey(block)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(rsaBlock)), nil
}

func ensureRSAPrivateKey(block *pem.Block) (*pem.Block, error) {
	switch block.Type {
	case "RSA PRIVATE KEY":
		if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return nil, fmt.Errorf("private_key invalid rsa: %w", err)
		}
		return block, nil
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("private_key invalid pkcs8: %w", err)
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("private_key is not an RSA key")
		}
		return &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}, nil
	}
	// Unknown block type: try PKCS#1 first, then PKCS#8.
	if rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}, nil
		}
	}
	return nil, errors.New("private_key uses unsupported format")
}

func rebuildPEM(raw string) (string, error) {
	kind := "PRIVATE KEY"
	if strings.Contains(raw, "RSA PRIVATE KEY") {
		kind = "RSA PRIVATE KEY"
	}
	header := "-----BEGIN " + kind + "-----"
	footer := "-----END " + kind + "-----"
	start := strings.Index(raw, header)
	end := strings.Index(raw, footer)
	if start < 0 || end <= start {
		return "", errors.New("missing pem markers")
	}
	payload := filterBase64(raw[start+len(header) : end])
	if payload == "" {
		return "", errors.New("private_key base64 payload empty")
	}
	der, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("private_key base64 decode failed: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})), nil
}

func filterBase64(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '/', r == '=':
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func stripANSIEscape(value string) string {
	in := []rune(value)
	out := make([]rune, 0, len(in))
	for i := 0; i < len(in); i++ {
		if in[i] != 0x1b {
			out = append(out, in[i])
			continue
		}
		if i+1 >= len(in) {
			continue
		}
		switch in[i+1] {
		case ']':
			// OSC: terminated by BEL or ESC \.
			for i += 2; i < len(in); i++ {
				if in[i] == 0x07 {
					break
				}
				if in[i] == 0x1b && i+1 < len(in) && in[i+1] == '\\' {
					i++
					break
				}
			}
		case '[':
			// CSI: terminated by the first letter.
			for i += 2; i < len(in); i++ {
				if (in[i] >= 'A' && in[i] <= 'Z') || (in[i] >= 'a' && in[i] <= 'z') {
					break
				}
			}
		}
	}
	return string(out)
}
