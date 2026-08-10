package app

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"

	"github.com/bytedance/sonic"
)

type debugBuffer struct {
	mu    sync.RWMutex
	buf   bytes.Buffer
	limit int
}

func (b *debugBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLen := len(p)
	if b.limit > 0 {
		remaining := b.limit - b.buf.Len()
		if remaining <= 0 {
			return originalLen, nil
		}
		if len(p) > remaining {
			p = p[:remaining]
		}
	}
	_, err := b.buf.Write(p)
	return originalLen, err
}

func (b *debugBuffer) Snapshot() []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func newDebugBuffer() *debugBuffer {
	return &debugBuffer{limit: config.DefaultMaxBodyBytes}
}

func cloneDebugBody(body []byte) []byte {
	if len(body) > config.DefaultMaxBodyBytes {
		body = body[:config.DefaultMaxBodyBytes]
	}
	return append([]byte(nil), body...)
}

// debugCapture 持有请求捕获数据和响应体缓冲区
type debugCapture struct {
	mu                    sync.RWMutex
	reqMethod             string
	reqURL                string
	reqHeaders            string // JSON
	reqBody               []byte
	respStatus            int
	respHeaders           string       // JSON
	respBuf               *debugBuffer // 上游原始响应 TeeReader 写入端
	upstreamError         string
	protocolTransformed   bool
	originalReqURL        string
	originalReqHeaders    string
	originalReqBody       []byte
	translatedRespStatus  int
	translatedRespHeaders string
	translatedResponseBuf *debugBuffer
}

func encodeDebugHeaders(header http.Header) string {
	headers := make(map[string]string, len(header))
	for k, vs := range header {
		if len(vs) > 0 {
			headers[k] = vs[0]
		}
	}
	headers = maskSensitiveHeaderMap(headers)
	hdrJSON, _ := sonic.Marshal(headers)
	return string(hdrJSON)
}

// captureDebugRequest 在发送上游请求前捕获请求信息，返回 nil 如果 debug 未开启
func (s *Server) captureDebugRequest(req *http.Request, bodyToSend []byte) *debugCapture {
	if !s.configService.GetBool("debug_log_enabled", false) {
		return nil
	}

	return &debugCapture{
		reqMethod:  req.Method,
		reqURL:     req.URL.String(),
		reqHeaders: encodeDebugHeaders(req.Header),
		reqBody:    cloneDebugBody(bodyToSend),
		respBuf:    newDebugBuffer(),
	}
}

func (dc *debugCapture) markProtocolTransform(originalReqURL string, originalReqHeaders http.Header, originalReqBody []byte) {
	if dc == nil {
		return
	}
	dc.mu.Lock()
	dc.protocolTransformed = true
	dc.originalReqURL = originalReqURL
	dc.originalReqHeaders = encodeDebugHeaders(originalReqHeaders)
	dc.originalReqBody = cloneDebugBody(originalReqBody)
	if dc.translatedResponseBuf == nil {
		dc.translatedResponseBuf = newDebugBuffer()
	}
	dc.mu.Unlock()
}

func (dc *debugCapture) captureTranslatedResponseMeta(status int, header http.Header) {
	if dc == nil {
		return
	}
	dc.mu.Lock()
	dc.translatedRespStatus = status
	dc.translatedRespHeaders = encodeDebugHeaders(header)
	dc.mu.Unlock()
}

func (dc *debugCapture) captureTranslatedResponse(body []byte) {
	if dc == nil || len(body) == 0 {
		return
	}
	dc.mu.RLock()
	buf := dc.translatedResponseBuf
	dc.mu.RUnlock()
	if buf != nil {
		_, _ = buf.Write(body)
	}
}

func (dc *debugCapture) wrapTranslatedResponseWriter(w http.ResponseWriter) http.ResponseWriter {
	if dc == nil || w == nil {
		return w
	}
	dc.mu.RLock()
	buf := dc.translatedResponseBuf
	dc.mu.RUnlock()
	if buf == nil {
		return w
	}
	return &debugTranslatedResponseWriter{ResponseWriter: w, capture: dc}
}

func (dc *debugCapture) captureResponseMeta(resp *http.Response) {
	if dc == nil || resp == nil {
		return
	}
	dc.mu.Lock()
	dc.respStatus = resp.StatusCode
	dc.respHeaders = encodeDebugHeaders(resp.Header)
	dc.mu.Unlock()
}

func (dc *debugCapture) captureUpstreamError(err error) {
	if dc == nil || err == nil {
		return
	}
	dc.mu.Lock()
	dc.upstreamError = err.Error()
	dc.mu.Unlock()
}

// wrapResponseBody 用 TeeReader 包装响应体以捕获内容
func (dc *debugCapture) wrapResponseBody(resp *http.Response) {
	if dc == nil || resp == nil {
		return
	}
	dc.captureResponseMeta(resp)
	if dc.respBuf == nil {
		dc.respBuf = newDebugBuffer()
	}
	resp.Body = &debugReadCloser{
		ReadCloser: resp.Body,
		tee:        io.TeeReader(resp.Body, dc.respBuf),
	}
}

// buildEntry 从捕获数据构建 DebugLogEntry
func (dc *debugCapture) buildEntry(resp *http.Response) *model.DebugLogEntry {
	if dc == nil {
		return nil
	}

	dc.mu.RLock()
	entry := &model.DebugLogEntry{
		CreatedAt:             time.Now().Unix(),
		ReqMethod:             dc.reqMethod,
		ReqURL:                dc.reqURL,
		ReqHeaders:            dc.reqHeaders,
		ReqBody:               append([]byte(nil), dc.reqBody...),
		RespStatus:            dc.respStatus,
		RespHeaders:           dc.respHeaders,
		UpstreamError:         dc.upstreamError,
		ProtocolTransformed:   dc.protocolTransformed,
		OriginalReqURL:        dc.originalReqURL,
		OriginalReqHeaders:    dc.originalReqHeaders,
		OriginalReqBody:       append([]byte(nil), dc.originalReqBody...),
		TranslatedRespStatus:  dc.translatedRespStatus,
		TranslatedRespHeaders: dc.translatedRespHeaders,
	}
	translatedResponseBuf := dc.translatedResponseBuf
	dc.mu.RUnlock()

	if dc.respBuf != nil {
		entry.RespBody = dc.respBuf.Snapshot()
	}
	if translatedResponseBuf != nil {
		entry.TranslatedRespBody = translatedResponseBuf.Snapshot()
	}

	if resp != nil {
		entry.RespStatus = resp.StatusCode
		entry.RespHeaders = encodeDebugHeaders(resp.Header)
	}

	return entry
}

type debugTranslatedResponseWriter struct {
	http.ResponseWriter
	capture     *debugCapture
	wroteHeader bool
}

func (w *debugTranslatedResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.capture.captureTranslatedResponseMeta(statusCode, w.Header())
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *debugTranslatedResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.capture.captureTranslatedResponse(p[:n])
	}
	return n, err
}

func (w *debugTranslatedResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *debugTranslatedResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func annotateNativeWebsocketDebug(entry *model.DebugLogEntry, snapshot codexWebsocketDebugSnapshot) {
	if entry == nil {
		return
	}
	entry.RespStatus = snapshot.ResponseStatus
	if entry.RespStatus == 0 {
		entry.RespStatus = http.StatusSwitchingProtocols
	}
	headers := make(map[string]string)
	for name, values := range snapshot.ResponseHeaders {
		if len(values) > 0 {
			headers[name] = values[0]
		}
	}
	headers["X-CCLoad-Upstream-Transport"] = "websocket"
	headers["X-CCLoad-WebSocket-Handshake-Status"] = strconv.Itoa(entry.RespStatus)
	headers["X-CCLoad-WebSocket-Reconnects"] = strconv.FormatUint(snapshot.Reconnects, 10)
	if snapshot.LastReconnectReason != "" {
		headers["X-CCLoad-WebSocket-Last-Reconnect-Reason"] = snapshot.LastReconnectReason
	}
	if snapshot.LastCloseReason != "" {
		headers["X-CCLoad-WebSocket-Last-Close-Reason"] = snapshot.LastCloseReason
	}
	headers = maskSensitiveHeaderMap(headers)
	encoded, _ := sonic.Marshal(headers)
	entry.RespHeaders = string(encoded)
}

// debugReadCloser 包装 ReadCloser，通过 TeeReader 同时写入缓冲区
type debugReadCloser struct {
	io.ReadCloser
	tee io.Reader
}

func (d *debugReadCloser) Read(p []byte) (int, error) {
	return d.tee.Read(p)
}
