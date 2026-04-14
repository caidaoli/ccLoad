package model

// DebugLogEntry 调试日志条目（记录上游请求/响应原始数据）
type DebugLogEntry struct {
	ID          int64  `json:"id"`
	LogID       int64  `json:"log_id"`
	CreatedAt   int64  `json:"created_at"`
	ReqMethod   string `json:"req_method"`
	ReqURL      string `json:"req_url"`
	ReqHeaders  string `json:"req_headers"` // JSON string
	ReqBody     []byte `json:"req_body"`
	RespStatus  int    `json:"resp_status"`
	RespHeaders string `json:"resp_headers"` // JSON string
	RespBody    []byte `json:"resp_body"`
}
