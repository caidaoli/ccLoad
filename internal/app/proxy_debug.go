package app

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/model"

	"github.com/bytedance/sonic"
)

type debugBuffer struct {
	mu  sync.RWMutex
	buf bytes.Buffer
}

func (b *debugBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *debugBuffer) Snapshot() []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// debugCapture 持有请求捕获数据和响应体缓冲区
type debugCapture struct {
	mu          sync.RWMutex
	reqMethod   string
	reqURL      string
	reqHeaders  string // JSON
	reqBody     []byte
	respStatus  int
	respHeaders string       // JSON
	respBuf     *debugBuffer // TeeReader 写入端
}

// captureDebugRequest 在发送上游请求前捕获请求信息，返回 nil 如果 debug 未开启
func (s *Server) captureDebugRequest(req *http.Request, bodyToSend []byte) *debugCapture {
	if !s.configService.GetBool("debug_log_enabled", false) {
		return nil
	}

	headers := make(map[string]string, len(req.Header))
	for k, vs := range req.Header {
		if len(vs) == 1 {
			headers[k] = vs[0]
		} else if len(vs) > 1 {
			headers[k] = vs[0] // 取第一个值
		}
	}
	hdrJSON, _ := sonic.Marshal(headers)

	return &debugCapture{
		reqMethod:  req.Method,
		reqURL:     req.URL.String(),
		reqHeaders: string(hdrJSON),
		reqBody:    append([]byte(nil), bodyToSend...),
		respBuf:    &debugBuffer{},
	}
}

func (dc *debugCapture) captureResponseMeta(resp *http.Response) {
	if dc == nil || resp == nil {
		return
	}
	respHeaders := make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		if len(vs) == 1 {
			respHeaders[k] = vs[0]
		} else if len(vs) > 1 {
			respHeaders[k] = vs[0]
		}
	}
	hdrJSON, _ := sonic.Marshal(respHeaders)

	dc.mu.Lock()
	dc.respStatus = resp.StatusCode
	dc.respHeaders = string(hdrJSON)
	dc.mu.Unlock()
}

// wrapResponseBody 用 TeeReader 包装响应体以捕获内容
func (dc *debugCapture) wrapResponseBody(resp *http.Response) {
	if dc == nil || resp == nil {
		return
	}
	dc.captureResponseMeta(resp)
	if dc.respBuf == nil {
		dc.respBuf = &debugBuffer{}
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
		CreatedAt:   time.Now().Unix(),
		ReqMethod:   dc.reqMethod,
		ReqURL:      dc.reqURL,
		ReqHeaders:  dc.reqHeaders,
		ReqBody:     append([]byte(nil), dc.reqBody...),
		RespStatus:  dc.respStatus,
		RespHeaders: dc.respHeaders,
	}
	dc.mu.RUnlock()

	if dc.respBuf != nil {
		entry.RespBody = dc.respBuf.Snapshot()
	}

	if resp != nil {
		entry.RespStatus = resp.StatusCode
		respHeaders := make(map[string]string, len(resp.Header))
		for k, vs := range resp.Header {
			if len(vs) == 1 {
				respHeaders[k] = vs[0]
			} else if len(vs) > 1 {
				respHeaders[k] = vs[0]
			}
		}
		hdrJSON, _ := sonic.Marshal(respHeaders)
		entry.RespHeaders = string(hdrJSON)
	}

	return entry
}

// debugReadCloser 包装 ReadCloser，通过 TeeReader 同时写入缓冲区
type debugReadCloser struct {
	io.ReadCloser
	tee io.Reader
}

func (d *debugReadCloser) Read(p []byte) (int, error) {
	return d.tee.Read(p)
}
