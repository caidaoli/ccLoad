package cursorauth

import (
	"encoding/json"
	"strings"
)

func asString(value any) string {
	text, _ := value.(string)
	return text
}

// RequestModelID reads the top-level model field from a JSON request body.
func RequestModelID(body []byte) string {
	var request struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	return strings.TrimSpace(request.Model)
}
