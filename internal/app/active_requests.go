// Package app 实现 ccLoad 应用的核心业务逻辑
package app

import (
	"context"
	"errors"
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

// errOperatorAbort 是管理员从日志页手动中断上游尝试时注入的 cancel cause。
//
// 文案刻意与真实的上游连接重置一致，这是本机制的核心：Go 的 context.WithCancelCause
// 会让 HTTP 传输层和响应体读取直接返回 cause 本身（而不是 context.Canceled），
// 于是 util.ClassifyError、util.IsModelScopedNetworkError 和 buildStreamDiagnostics
// 全都无需针对手动中断加特判，中断自动走完与真实断链完全相同的分类、冷却和故障切换
// 路径——未提交时 502 模型级冷却并切下一渠道，已提交时 599 流中断。
//
// 别改成 context.Canceled 或不含 "connection reset by peer" 的文案：前者会被判成
// 客户端取消（499、不冷却、不重试），后者会掉进通用 502 分支而丢掉模型级作用域。
var errOperatorAbort = errors.New("read: connection reset by peer (aborted by operator)")

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
	Abortable           bool    `json:"abortable,omitempty"` // 当前上游尝试是否登记了可中断句柄
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
	// abort 取消当前这次上游尝试（不是整条请求链路），由 BeginAttempt 登记。
	// 同渠道同 URL 内的内部重试共用它，因此 Retry 不得清空。
	abort context.CancelCauseFunc

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
	// Abort 取消这次上游尝试。为 nil 时该尝试不可中断（前端不显示中断入口）。
	Abort context.CancelCauseFunc
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
	req.abort = attempt.Abort
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

// Abort 以「上游连接被重置」的语义中断指定请求当前正在进行的上游尝试。
// 中断只作用于这一次尝试：后续的故障切换、冷却与重试全部交给既有的网络故障
// 处置链路，因此中断的可见结果取决于时机——上游尚未提交响应时会切换到下一个
// 渠道，已经在向客户端输出时只能按流中断收尾。
//
// 命中返回 true；请求已结束或当前尝试未登记中断句柄时返回 false。
func (m *activeRequestManager) Abort(id int64) bool {
	m.mu.RLock()
	var abort context.CancelCauseFunc
	if req := m.requests[id]; req != nil {
		abort = req.abort
	}
	m.mu.RUnlock()

	if abort == nil {
		return false
	}
	// 在锁外触发：取消会唤醒正在转发的 goroutine，它可能立刻回头更新本管理器。
	abort(errOperatorAbort)
	return true
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
			Abortable:         req.abort != nil,
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
