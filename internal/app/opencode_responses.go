package app

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

type openCodeToolIdentity struct{ namespace, name string }

type openCodeResponsesPlan struct {
	names      map[openCodeToolIdentity]string
	identities map[string]openCodeToolIdentity
	required   map[string][]string
}

// Keep this workaround at the observed provider/model boundary, after body rules.
// Registry's native Responses path and other OpenCode models remain transparent.
func prepareOpenCodeResponsesRequest(target *url.URL, upstream protocol.Protocol, path string, body []byte, p *openCodeResponsesPlan) ([]byte, *openCodeResponsesPlan) {
	if target == nil || !isOpenCodeEndpoint(target.String()) || upstream != protocol.Codex ||
		protocol.DetectRequestFamily(path) != protocol.RequestFamilyResponses ||
		gjson.GetBytes(body, "model").String() != "muse-spark-1.3-contributor" {
		return body, nil
	}
	if p == nil {
		p = &openCodeResponsesPlan{
			names:      make(map[openCodeToolIdentity]string),
			identities: make(map[string]openCodeToolIdentity),
			required:   make(map[string][]string),
		}
	}
	root := gjson.ParseBytes(body)
	var identities []openCodeToolIdentity
	var declarations []struct {
		identity openCodeToolIdentity
		tool     gjson.Result
	}
	var collect func(gjson.Result, string)
	collect = func(tools gjson.Result, namespace string) {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() == "namespace" {
				collect(tool.Get("tools"), tool.Get("name").String())
				continue
			}
			if typ := tool.Get("type").String(); typ != "function" && typ != "custom" {
				continue
			}
			id := openCodeToolIdentity{namespace, tool.Get("name").String()}
			identities = append(identities, id)
			declarations = append(declarations, struct {
				identity openCodeToolIdentity
				tool     gjson.Result
			}{id, tool})
		}
	}
	collect(root.Get("tools"), "")
	for _, item := range root.Get("input").Array() {
		switch item.Get("type").String() {
		case "additional_tools", "tool_search_output":
			collect(item.Get("tools"), "")
		case "function_call", "custom_tool_call":
			identities = append(identities, openCodeToolIdentity{item.Get("namespace").String(), item.Get("name").String()})
		}
	}
	// Reserve direct names before assigning aliases, including history-only names.
	for _, id := range identities {
		if id.namespace == "" {
			p.names[id] = id.name
			// A replay already contains wire names. Preserve their reverse identity.
			if _, exists := p.identities[id.name]; !exists {
				p.identities[id.name] = id
			}
		}
	}
	for _, id := range identities {
		if _, exists := p.names[id]; exists {
			continue
		}
		name := id.namespace + "__" + id.name
		if len(name) > 64 || strings.IndexFunc(name, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
		}) >= 0 {
			name = ""
		}
		for attempt := 0; ; attempt++ {
			if _, taken := p.identities[name]; name != "" && !taken {
				break
			}
			digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", id.namespace, id.name, attempt)))
			name = fmt.Sprintf("ns_%x", digest[:24])
		}
		p.names[id], p.identities[name] = name, id
	}
	for _, declaration := range declarations {
		name := p.names[declaration.identity]
		if _, exists := p.required[name]; exists {
			continue
		}
		p.required[name] = nil
		for _, key := range declaration.tool.Get("parameters.required").Array() {
			p.required[name] = append(p.required[name], key.String())
		}
	}
	var flatten func(gjson.Result, string) []string
	flatten = func(tools gjson.Result, namespace string) []string {
		var out []string
		for _, tool := range tools.Array() {
			if tool.Get("type").String() == "namespace" {
				out = append(out, flatten(tool.Get("tools"), tool.Get("name").String())...)
				continue
			}
			raw := []byte(tool.Raw)
			if name, ok := p.names[openCodeToolIdentity{namespace, tool.Get("name").String()}]; ok {
				raw = setJSONValue(raw, "name", name)
				raw = deleteJSONPath(raw, "namespace")
			}
			out = append(out, string(raw))
		}
		return out
	}
	if root.Get("tools").IsArray() {
		body = setJSONRaw(body, "tools", joinJSONRaw(flatten(root.Get("tools"), "")))
	}
	for i, item := range root.Get("input").Array() {
		path := fmt.Sprintf("input.%d", i)
		switch item.Get("type").String() {
		case "additional_tools", "tool_search_output":
			if item.Get("tools").IsArray() {
				body = setJSONRaw(body, path+".tools", joinJSONRaw(flatten(item.Get("tools"), "")))
			}
		case "function_call", "custom_tool_call":
			body = setJSONRaw(body, path, string(p.flattenChoice([]byte(item.Raw))))
		}
	}
	if choice := root.Get("tool_choice"); choice.IsObject() {
		body = setJSONRaw(body, "tool_choice", string(p.flattenChoice([]byte(choice.Raw))))
	}
	return body, p
}

func (p *openCodeResponsesPlan) flattenChoice(raw []byte) []byte {
	id := openCodeToolIdentity{gjson.GetBytes(raw, "namespace").String(), gjson.GetBytes(raw, "name").String()}
	if name, ok := p.names[id]; ok {
		raw = setJSONValue(raw, "name", name)
		raw = deleteJSONPath(raw, "namespace")
	}
	for i, tool := range gjson.GetBytes(raw, "tools").Array() {
		raw = setJSONRaw(raw, fmt.Sprintf("tools.%d", i), string(p.flattenChoice([]byte(tool.Raw))))
	}
	return raw
}

// The reader runs synchronously under the forwarding timeout/Close lifecycle.
// Only complete SSE events are rewritten; an EOF fragment is passed unchanged.
type openCodeResponsesReader struct {
	io.ReadCloser
	plan     *openCodeResponsesPlan
	scanner  *bufio.Scanner
	pending  []byte
	jsonRead bool
	terminal map[string][32]byte
	warned   bool
}

func prepareOpenCodeResponsesResponse(resp *http.Response, plan *openCodeResponsesPlan, streaming bool) {
	if plan == nil || resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	isSSE := responseIsSSE(resp, streaming)
	r := &openCodeResponsesReader{ReadCloser: resp.Body, plan: plan, terminal: make(map[string][32]byte)}
	if isSSE {
		r.scanner = bufio.NewScanner(wrapCodexSSEBody(resp.Body))
		r.scanner.Buffer(make([]byte, SSEBufferSize), maxSSEEventSize)
		r.scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
			if end := firstSSEEventEnd(data); end >= 0 {
				return end, data[:end], nil
			}
			if atEOF && len(data) > 0 {
				return len(data), data, nil
			}
			return 0, nil, nil
		})
	}
	resp.Body = r
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
}

func (r *openCodeResponsesReader) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if r.scanner == nil {
			if r.jsonRead {
				return 0, io.EOF
			}
			r.jsonRead = true
			body, err := io.ReadAll(r.ReadCloser)
			if err != nil {
				return 0, err
			}
			r.pending = r.rewrite(body)
		} else {
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
			updated := r.rewrite(data)
			if bytes.Equal(updated, data) {
				r.pending = frame
				continue
			}
			wrote := false
			for _, line := range bytes.SplitAfter(frame, []byte{'\n'}) {
				if bytes.HasPrefix(line, []byte("data:")) {
					if !wrote {
						r.pending = append(r.pending, []byte("data: ")...)
						r.pending = append(r.pending, updated...)
						r.pending = append(r.pending, '\n')
						wrote = true
					}
				} else {
					r.pending = append(r.pending, line...)
				}
			}
		}
	}
	n := copy(dst, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *openCodeResponsesReader) diagnose(item gjson.Result, key string) {
	args := item.Get("arguments").String()
	problem := ""
	if !gjson.Valid(args) || !gjson.Parse(args).IsObject() {
		problem = "invalid function arguments"
	}
	for _, field := range r.plan.required[item.Get("name").String()] {
		if _, ok := gjson.Parse(args).Map()[field]; !ok {
			problem = "missing required function argument"
			break
		}
	}
	digest := sha256.Sum256([]byte(args))
	if key != "" {
		if previous, ok := r.terminal[key]; ok && previous != digest {
			problem = "inconsistent terminal function arguments"
		}
		r.terminal[key] = digest
	}
	if item.Get("status").String() == "incomplete" {
		problem = "incomplete function call in terminal response"
	}
	if problem != "" && !r.warned {
		// Never log argument contents. Raw evidence remains in the debug capture.
		log.Printf("[WARN] OpenCode Go muse Responses: %s (upstream tool output; arguments preserved)", problem)
		r.warned = true
	}
}

func (r *openCodeResponsesReader) rewrite(body []byte) []byte {
	root := gjson.ParseBytes(body)
	typ := root.Get("type").String()
	if typ == "response.function_call_arguments.done" {
		r.diagnose(root, root.Get("item_id").String())
	}
	restore := func(path string, terminal bool) {
		item := gjson.GetBytes(body, path)
		if typ := item.Get("type").String(); typ != "function_call" && typ != "custom_tool_call" {
			return
		}
		if terminal && item.Get("type").String() == "function_call" {
			r.diagnose(item, item.Get("id").String())
		}
		if id, ok := r.plan.identities[item.Get("name").String()]; ok && id.namespace != "" {
			body = setJSONValue(body, path+".name", id.name)
			body = setJSONValue(body, path+".namespace", id.namespace)
		}
	}
	restore("item", typ == "response.output_item.done")
	for _, prefix := range []string{"response.output", "output"} {
		for i := range root.Get(prefix).Array() {
			restore(fmt.Sprintf("%s.%d", prefix, i), typ == "response.completed" || typ == "response.incomplete" || root.Get("object").String() == "response")
		}
	}
	if strings.HasPrefix(typ, "response.function_call_arguments.") {
		if id, ok := r.plan.identities[root.Get("name").String()]; ok && id.namespace != "" {
			body = setJSONValue(body, "name", id.name)
			body = setJSONValue(body, "namespace", id.namespace)
		}
	}
	return body
}
