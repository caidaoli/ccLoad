package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

func canonicalCodexImageModel(raw string) (string, bool) {
	name := model.RoutingModelName(strings.TrimSpace(raw))
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "gpt-image-1.5", "gpt-image-2", "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst",
		"gpt-image-2.5-flare-2026-09-08", "gpt-image-2.5-sunburst-2026-09-08":
		return name, true
	default:
		return "", false
	}
}

func (s *Server) codexDirectImagesModel(cfg *model.Config, reqCtx *proxyRequestContext) (string, bool) {
	if cfg == nil || reqCtx == nil || !cfg.UsesCodexOAuth() || reqCtx.requestMethod != http.MethodPost ||
		reqCtx.clientProtocol != protocol.OpenAI || cfg.GetProtocolTransformMode() == model.ProtocolTransformModeUpstream {
		return "", false
	}
	path := strings.TrimRight(reqCtx.requestPath, "/")
	if path != openAIImagesGenerationsPath && path != "/v1/images/edits" {
		return "", false
	}
	actual := s.resolveFinalUpstreamModel(cfg, reqCtx.originalModel, string(protocol.Codex))
	canonical, _ := canonicalCodexImageModel(actual)
	switch canonical {
	case "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst":
		return canonical, true
	default:
		return "", false
	}
}

// Codex channel URLs normally point at the exact Responses endpoint.
func codexImagesURL(baseURL, imagePath, rawQuery string) string {
	baseURL = strings.TrimRight(model.StripExactUpstreamURLMarker(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return buildUpstreamURL(baseURL, imagePath, rawQuery)
	}
	for _, suffix := range []string{"/responses", "/images/generations", "/images/edits"} {
		if strings.HasSuffix(parsed.Path, suffix) {
			parsed.Path = strings.TrimSuffix(parsed.Path, suffix)
			break
		}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + imagePath
	parsed.RawPath = ""
	if rawQuery != "" {
		if parsed.RawQuery != "" {
			parsed.RawQuery += "&"
		}
		parsed.RawQuery += rawQuery
	}
	return parsed.String()
}

func prepareCodexDirectImagesBody(raw []byte, contentType, imageModel string) ([]byte, error) {
	mediaType, params, _ := mime.ParseMediaType(contentType)
	var body map[string]any
	if mediaType == "multipart/form-data" {
		var err error
		body, err = codexImageEditMultipart(raw, params["boundary"])
		if err != nil {
			return nil, err
		}
	} else {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&body); err != nil || body == nil {
			return nil, errors.New("images request must be a JSON object")
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return nil, errors.New("images request contains trailing JSON")
		}
	}
	if prompt, ok := body["prompt"].(string); !ok || strings.TrimSpace(prompt) == "" {
		return nil, errors.New("images request requires a non-empty string prompt")
	}
	if stream, exists := body["stream"]; exists {
		if _, ok := stream.(bool); !ok {
			return nil, errors.New("images stream must be a boolean")
		}
	}
	if n, exists := body["n"]; exists {
		value, ok := n.(json.Number)
		count, err := value.Int64()
		if !ok || err != nil || count < 1 {
			return nil, errors.New("images n must be a positive integer")
		}
	}
	body["model"] = imageModel
	return json.Marshal(body)
}

// The incoming request is already bounded by max_body_bytes. Read parts in
// wire order without temporary files or fetching remote image URLs.
func codexImageEditMultipart(raw []byte, boundary string) (map[string]any, error) {
	if boundary == "" {
		return nil, errors.New("multipart boundary is missing")
	}
	body := make(map[string]any)
	var images []any
	reader := multipart.NewReader(bytes.NewReader(raw), boundary)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read image multipart: %w", err)
		}
		data, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			return nil, err
		}
		name := part.FormName()
		if part.FileName() != "" {
			mimeType := part.Header.Get("Content-Type")
			if mimeType == "" || mimeType == "application/octet-stream" {
				mimeType = http.DetectContentType(data)
			}
			dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
			switch name {
			case "image", "image[]":
				images = append(images, map[string]any{"image_url": dataURL})
			case "mask":
				body["mask"] = map[string]any{"image_url": dataURL}
			default:
				return nil, fmt.Errorf("unsupported image file field %q", name)
			}
			continue
		}
		value := string(data)
		switch name {
		case "n", "output_compression", "partial_images":
			if _, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err != nil {
				return nil, fmt.Errorf("images %s must be an integer", name)
			}
			body[name] = json.Number(strings.TrimSpace(value))
		case "stream":
			if _, exists := body[name]; exists {
				return nil, errors.New("images stream must not be repeated")
			}
			stream, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				return nil, errors.New("images stream must be a boolean")
			}
			body[name] = stream
		case "mask[file_id]", "mask[image_url]":
			mask, _ := body["mask"].(map[string]any)
			if mask == nil {
				mask = make(map[string]any)
			}
			mask[strings.TrimSuffix(strings.TrimPrefix(name, "mask["), "]")] = value
			body["mask"] = mask
		case "images":
			var refs []any
			if err := json.Unmarshal(data, &refs); err != nil {
				return nil, errors.New("images must be a JSON array")
			}
			images = append(images, refs...)
		default:
			body[name] = value
		}
	}
	if len(images) > 0 {
		body["images"] = images
	}
	return body, nil
}

// Dated snapshots retain the Responses contract until direct support is verified.
func codexImageUsesResponses(model string) bool {
	canonical, _ := canonicalCodexImageModel(model)
	return canonical == "gpt-image-2.5-flare-2026-09-08" || canonical == "gpt-image-2.5-sunburst-2026-09-08"
}

func buildCodexImagesResponsesRequest(raw []byte, imageModel string) ([]byte, error) {
	canonical, supported := canonicalCodexImageModel(imageModel)
	if !supported || !codexImageUsesResponses(canonical) {
		return nil, codexImageUnsupportedModelError(imageModel)
	}
	// Channel selection has already resolved the image model's routing prefix.
	return buildImagesResponsesRequest(raw, "gpt-5.6-luna", canonical)
}
