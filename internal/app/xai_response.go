package app

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/tidwall/gjson"
)

// prepareXAIResponsesResponse normalizes xAI's Responses reasoning dialect at
// the provider boundary, before native forwarding or protocol conversion.
func prepareXAIResponsesResponse(resp *http.Response, streaming bool) {
	if resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	if !responseIsSSE(resp, streaming) {
		wrapJSONEventRewrite(resp, nil, normalizeXAIResponsesJSON)
		return
	}
	scanner := bufio.NewScanner(wrapCodexSSEBody(resp.Body))
	scanner.Buffer(make([]byte, SSEBufferSize), maxSSEEventSize)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if end := firstSSEEventEnd(data); end >= 0 {
			return end, data[:end], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	resp.Body = &xaiResponsesReader{ReadCloser: resp.Body, scanner: scanner}
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
}

type xaiResponsesReader struct {
	io.ReadCloser
	scanner        *bufio.Scanner
	pending        []byte
	sequenceOffset int64
	summaryParts   map[string]bool
}

func (r *xaiResponsesReader) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		frame := bytes.Clone(r.scanner.Bytes())
		_, data := parseSSEEventChunk(frame)
		if firstSSEEventEnd(frame) < 0 || !gjson.ValidBytes(data) {
			r.pending = frame
			continue
		}
		payloads := [][]byte{normalizeXAIResponsesJSON(data)}
		normalizedType := gjson.GetBytes(payloads[0], "type").String()
		if normalizedType == "response.reasoning_summary_part.added" || normalizedType == "response.reasoning_summary_text.delta" {
			if r.summaryParts == nil {
				r.summaryParts = make(map[string]bool)
			}
			item := gjson.GetBytes(data, "output_index").Raw
			if item == "" {
				item = gjson.GetBytes(data, "item_id").String()
			}
			key := item + ":" + gjson.GetBytes(payloads[0], "summary_index").Raw
			// Some xAI streams begin with a delta. Downstream translators use the
			// part-added event to distinguish streamed text from a final-item fallback.
			if normalizedType == "response.reasoning_summary_text.delta" && !r.summaryParts[key] {
				added := setJSONValue(payloads[0], "type", "response.reasoning_summary_part.added")
				added = setJSONRaw(deleteJSONPath(added, "delta"), "part", `{"type":"summary_text","text":""}`)
				payloads = append([][]byte{added}, payloads...)
			}
			r.summaryParts[key] = true
		}
		if gjson.GetBytes(data, "type").String() == "response.reasoning_text.done" {
			textDone := setJSONValue(data, "type", "response.reasoning_summary_text.done")
			payloads = append([][]byte{normalizeXAIResponsesIndex(textDone)}, payloads...)
		}
		if seq := gjson.GetBytes(data, "sequence_number"); seq.Exists() && (r.sequenceOffset != 0 || len(payloads) > 1) {
			for i := range payloads {
				payloads[i] = setJSONValue(payloads[i], "sequence_number", seq.Int()+r.sequenceOffset+int64(i))
			}
			r.sequenceOffset += int64(len(payloads) - 1)
		}
		if len(payloads) == 1 && bytes.Equal(payloads[0], data) {
			r.pending = frame
			continue
		}
		for _, payload := range payloads {
			wrote := false
			for _, line := range bytes.SplitAfter(frame, []byte{'\n'}) {
				switch {
				case bytes.HasPrefix(line, []byte("event:")):
					r.pending = append(r.pending, "event: "...)
					r.pending = append(r.pending, gjson.GetBytes(payload, "type").String()...)
					r.pending = append(r.pending, '\n')
				case bytes.HasPrefix(line, []byte("data:")):
					if !wrote {
						r.pending = append(r.pending, "data: "...)
						r.pending = append(r.pending, payload...)
						r.pending = append(r.pending, '\n')
						wrote = true
					}
				default:
					r.pending = append(r.pending, line...)
				}
			}
		}
	}
	n := copy(dst, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func normalizeXAIResponsesIndex(body []byte) []byte {
	if index := gjson.GetBytes(body, "content_index"); index.Exists() {
		if !gjson.GetBytes(body, "summary_index").Exists() {
			body = setJSONRaw(body, "summary_index", index.Raw)
		}
		body = deleteJSONPath(body, "content_index")
	}
	return body
}

func normalizeXAIResponsesJSON(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	root := gjson.ParseBytes(body)
	switch root.Get("type").String() {
	case "response.reasoning_text.delta":
		body = setJSONValue(body, "type", "response.reasoning_summary_text.delta")
		body = normalizeXAIResponsesIndex(body)
	case "response.reasoning_text.done":
		body = setJSONValue(body, "type", "response.reasoning_summary_part.done")
		body = setJSONValue(body, "part.type", "summary_text")
		if text := root.Get("text"); text.Exists() {
			body = setJSONValue(body, "part.text", text.String())
		}
		body = deleteJSONPath(body, "text")
		body = normalizeXAIResponsesIndex(body)
	case "response.content_part.added", "response.content_part.done":
		if root.Get("part.type").String() == "reasoning_text" {
			body = setJSONValue(body, "type", "response.reasoning_summary_part."+root.Get("type").String()[len("response.content_part."):])
			body = setJSONValue(body, "part.type", "summary_text")
			body = normalizeXAIResponsesIndex(body)
		}
	}
	normalizeItem := func(path string) {
		item := gjson.GetBytes(body, path)
		if item.Get("type").String() != "reasoning" {
			return
		}
		summary := item.Get("summary").Array()
		var content []string
		var reasoning []gjson.Result
		for _, part := range item.Get("content").Array() {
			if part.Get("type").String() == "reasoning_text" {
				reasoning = append(reasoning, part)
			} else {
				content = append(content, part.Raw)
			}
		}
		if len(summary) == 0 && len(reasoning) > 0 {
			summary = reasoning
		}
		var normalized []string
		for _, part := range summary {
			raw := []byte(part.Raw)
			if part.Get("type").String() == "reasoning_text" {
				raw = setJSONValue(raw, "type", "summary_text")
			}
			normalized = append(normalized, string(raw))
		}
		if len(normalized) > 0 {
			body = setJSONRaw(body, path+".summary", joinJSONRaw(normalized))
		}
		if len(reasoning) > 0 {
			if len(content) == 0 {
				body = deleteJSONPath(body, path+".content")
			} else {
				body = setJSONRaw(body, path+".content", joinJSONRaw(content))
			}
		}
	}
	normalizeItem("item")
	for _, prefix := range []string{"response.output", "output"} {
		for i := range root.Get(prefix).Array() {
			normalizeItem(fmt.Sprintf("%s.%d", prefix, i))
		}
	}
	return body
}
