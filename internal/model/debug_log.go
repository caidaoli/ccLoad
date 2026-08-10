package model

// DebugLogEntry 调试日志条目。
// LogID 与 logs.id 1:1 对应，直接作为 debug_logs 主键
type DebugLogEntry struct {
	LogID                 int64  `json:"log_id"`
	CreatedAt             int64  `json:"created_at"`
	ReqMethod             string `json:"req_method"`
	ReqURL                string `json:"req_url"`
	ReqHeaders            string `json:"req_headers"` // JSON string
	ReqBody               []byte `json:"req_body"`    // 实际发往上游的请求体
	RespStatus            int    `json:"resp_status"`
	RespHeaders           string `json:"resp_headers"`   // JSON string
	RespBody              []byte `json:"resp_body"`      // 上游原始响应体
	UpstreamError         string `json:"upstream_error"` // 未形成完整 HTTP 响应时的传输/解析错误
	ProtocolTransformed   bool   `json:"protocol_transformed"`
	OriginalReqURL        string `json:"original_req_url"`        // 客户端原始请求目标
	OriginalReqHeaders    string `json:"original_req_headers"`    // 客户端原始请求头，JSON string
	OriginalReqBody       []byte `json:"original_req_body"`       // 客户端原始请求体
	TranslatedRespStatus  int    `json:"translated_resp_status"`  // 最终写给客户端的响应状态
	TranslatedRespHeaders string `json:"translated_resp_headers"` // 最终写给客户端的响应头，JSON string
	TranslatedRespBody    []byte `json:"translated_resp_body"`    // 转换后写给客户端的响应体
}
