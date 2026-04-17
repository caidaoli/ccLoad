package model

// DebugLogEntry 调试日志条目（记录上游请求/响应原始数据）
// LogID 与 logs.id 1:1 对应，直接作为 debug_logs 主键
type DebugLogEntry struct {
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
