// Package app 实现 ccLoad 应用的核心业务逻辑
package app

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

const (
	activeRequestStatusRequesting = "requesting"
	activeRequestStatusReceiving  = "receiving"
	activeRequestStatusRetrying   = "retrying"
)

// ActiveRequest 表示一个进行中的请求
type ActiveRequest struct {
	ID                  int64   `json:"id"`
	Model               string  `json:"model"`
	ClientIP            string  `json:"client_ip"`
	StartTime           int64   `json:"start_time"` // Unix毫秒
	Streaming           bool    `json:"is_streaming"`
	ChannelID           int64   `json:"channel_id,omitempty"`
	ChannelName         string  `json:"channel_name,omitempty"`
	ClientProtocol      string  `json:"client_protocol,omitempty"`        // 客户端入口协议
	UpstreamProtocol    string  `json:"upstream_protocol,omitempty"`      // 当前尝试的实际上游协议
	APIKeyUsed          string  `json:"api_key_used,omitempty"`           // 脱敏后的key
	TokenID             int64   `json:"token_id,omitempty"`               // 令牌ID（用于前端筛选，0表示无令牌）
	BaseURL             string  `json:"base_url,omitempty"`               // 当前使用的上游URL
	BytesReceived       int64   `json:"bytes_received,omitempty"`         // 上游已返回的字节数（快照）
	ClientFirstByteTime float64 `json:"client_first_byte_time,omitempty"` // 客户端侧首字节响应时间（秒），流式请求有效
	CostMultiplier      float64 `json:"cost_multiplier"`                  // 渠道成本倍率
	UpstreamWebsocket   bool    `json:"upstream_websocket,omitempty"`     // 实际上游请求是否使用WebSocket
	DebugLogAvailable   bool    `json:"debug_log_available,omitempty"`    // 运行中请求是否已有可读取的调试快照
	ThinkingEffort      string  `json:"thinking_effort,omitempty"`
	UpstreamStatus      string  `json:"upstream_status"`
}

type activeRequest struct {
	ID               int64
	Model            string
	ClientIP         string
	StartTime        int64 // Unix毫秒
	Streaming        bool
	ChannelID        int64
	ChannelName      string
	ClientProtocol   string
	UpstreamProtocol string
	APIKeyUsed       string
	TokenID          int64
	BaseURL          string

	CostMultiplier    float64 // 渠道成本倍率
	UpstreamWebsocket bool
	ThinkingEffort    string
	UpstreamStatus    string
	debugCapture      *debugCapture

	bytesCounter            atomic.Int64 // 上游已返回的字节数（原子累加）
	clientFirstByteTimeUsec atomic.Int64 // 客户端侧首字节响应时间（微秒），CAS保证只写一次，0表示未设置
}

type activeRequestAttempt struct {
	StartTime        time.Time
	Model            string
	ClientIP         string
	Streaming        bool
	ChannelID        int64
	ChannelName      string
	ClientProtocol   string
	UpstreamProtocol string
	APIKey           string
	TokenID          int64
	BaseURL          string
	CostMultiplier   float64
	ThinkingEffort   string
}

// activeRequestManager 管理进行中的请求（内存状态，不持久化）
type activeRequestManager struct {
	mu       sync.RWMutex
	requests map[int64]*activeRequest
	nextID   atomic.Int64
}

func newActiveRequestManager() *activeRequestManager {
	return &activeRequestManager{
		requests: make(map[int64]*activeRequest),
	}
}

// BeginAttempt 在上游渠道、Key 和 URL 均已确定后登记当前尝试。
// id=0 表示首次尝试；已有 id 表示故障切换后的重试。
func (m *activeRequestManager) BeginAttempt(id int64, attempt activeRequestAttempt) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	req := m.requests[id]
	if req == nil {
		id = m.nextID.Add(1)
		req = &activeRequest{ID: id, UpstreamStatus: activeRequestStatusRequesting}
		m.requests[id] = req
	} else {
		req.UpstreamStatus = activeRequestStatusRetrying
	}

	req.Model = attempt.Model
	req.ClientIP = attempt.ClientIP
	req.StartTime = attempt.StartTime.UnixMilli()
	req.Streaming = attempt.Streaming
	req.ChannelID = attempt.ChannelID
	req.ChannelName = attempt.ChannelName
	req.ClientProtocol = attempt.ClientProtocol
	req.UpstreamProtocol = attempt.UpstreamProtocol
	req.APIKeyUsed = util.MaskAPIKey(attempt.APIKey)
	req.TokenID = attempt.TokenID
	req.BaseURL = attempt.BaseURL
	req.CostMultiplier = attempt.CostMultiplier
	req.UpstreamWebsocket = false
	req.ThinkingEffort = normalizeThinkingEffort(attempt.ThinkingEffort)
	req.debugCapture = nil
	req.clientFirstByteTimeUsec.Store(0)
	req.bytesCounter.Store(0)
	return id
}

func (m *activeRequestManager) SetUpstreamProtocol(id int64, upstreamProtocol string) {
	m.mu.Lock()
	if req := m.requests[id]; req != nil {
		req.UpstreamProtocol = upstreamProtocol
	}
	m.mu.Unlock()
}

// Retry 标记同一渠道、Key 和 URL 上的内部重试。
func (m *activeRequestManager) Retry(id int64) {
	m.mu.Lock()
	if req := m.requests[id]; req != nil {
		req.StartTime = time.Now().UnixMilli()
		req.UpstreamStatus = activeRequestStatusRetrying
		req.UpstreamWebsocket = false
		req.debugCapture = nil
		req.clientFirstByteTimeUsec.Store(0)
		req.bytesCounter.Store(0)
	}
	m.mu.Unlock()
}

// SetUpstreamWebsocket records the transport actually used by the current upstream attempt.
func (m *activeRequestManager) SetUpstreamWebsocket(id int64, upstreamWebsocket bool) {
	m.mu.Lock()
	if req, ok := m.requests[id]; ok {
		req.UpstreamWebsocket = upstreamWebsocket
	}
	m.mu.Unlock()
}

// SetDebugCapture 绑定运行中请求的调试捕获器。
// 调试日志关闭时 dc 为 nil；列表只暴露 bool，正文按需通过独立接口读取。
func (m *activeRequestManager) SetDebugCapture(id int64, dc *debugCapture) {
	m.mu.Lock()
	if req, ok := m.requests[id]; ok {
		req.debugCapture = dc
	}
	m.mu.Unlock()
}

// GetDebugLogSnapshot 返回运行中请求当前调试快照。
func (m *activeRequestManager) GetDebugLogSnapshot(id int64) (*model.DebugLogEntry, bool) {
	m.mu.RLock()
	req := m.requests[id]
	var dc *debugCapture
	if req != nil {
		dc = req.debugCapture
	}
	m.mu.RUnlock()

	if dc == nil {
		return nil, false
	}
	return dc.buildEntry(nil), true
}

// Remove 移除一个活跃请求
func (m *activeRequestManager) Remove(id int64) {
	m.mu.Lock()
	delete(m.requests, id)
	m.mu.Unlock()
}

func (m *activeRequestManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.requests)
}

// AddBytes 原子地增加指定请求的字节数（线程安全）
func (m *activeRequestManager) AddBytes(id int64, n int64) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	req := m.requests[id]
	if req != nil {
		req.bytesCounter.Add(n)
		req.UpstreamStatus = activeRequestStatusReceiving
	}
	m.mu.Unlock()
}

// SetClientFirstByteTime 设置客户端侧首字节响应时间（CAS保证只写一次，线程安全）
func (m *activeRequestManager) SetClientFirstByteTime(id int64, d time.Duration) {
	if d <= 0 {
		return
	}
	m.mu.Lock()
	req := m.requests[id]
	if req == nil {
		m.mu.Unlock()
		return
	}
	usec := d.Microseconds()
	if usec <= 0 {
		m.mu.Unlock()
		return
	}
	req.clientFirstByteTimeUsec.CompareAndSwap(0, usec) // 只有首次（0值）才写入
	req.UpstreamStatus = activeRequestStatusReceiving
	m.mu.Unlock()
}

// List 返回所有活跃请求的快照（按开始时间降序，最新的在前）
func (m *activeRequestManager) List() []*ActiveRequest {
	m.mu.RLock()
	result := make([]*ActiveRequest, 0, len(m.requests))
	for _, req := range m.requests {
		view := &ActiveRequest{
			ID:                req.ID,
			Model:             req.Model,
			ClientIP:          req.ClientIP,
			StartTime:         req.StartTime,
			Streaming:         req.Streaming,
			ChannelID:         req.ChannelID,
			ChannelName:       req.ChannelName,
			ClientProtocol:    req.ClientProtocol,
			UpstreamProtocol:  req.UpstreamProtocol,
			APIKeyUsed:        req.APIKeyUsed,
			TokenID:           req.TokenID,
			BaseURL:           req.BaseURL,
			BytesReceived:     req.bytesCounter.Load(),
			CostMultiplier:    req.CostMultiplier,
			UpstreamWebsocket: req.UpstreamWebsocket,
			DebugLogAvailable: req.debugCapture != nil,
			ThinkingEffort:    req.ThinkingEffort,
			UpstreamStatus:    req.UpstreamStatus,
		}
		if usec := req.clientFirstByteTimeUsec.Load(); usec > 0 {
			view.ClientFirstByteTime = float64(usec) / 1e6
		}
		result = append(result, view)
	}
	m.mu.RUnlock()
	// 按开始时间降序排序
	sort.Slice(result, func(i, j int) bool {
		if result[i].StartTime != result[j].StartTime {
			return result[i].StartTime > result[j].StartTime
		}
		return result[i].ID > result[j].ID
	})
	return result
}
