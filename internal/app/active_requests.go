// Package app 实现 ccLoad 应用的核心业务逻辑
package app

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/util"
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
	ChannelType         string  `json:"channel_type,omitempty"`           // 渠道类型（用于前端筛选）
	APIKeyUsed          string  `json:"api_key_used,omitempty"`           // 脱敏后的key
	TokenID             int64   `json:"token_id,omitempty"`               // 令牌ID（用于前端筛选，0表示无令牌）
	BytesReceived       int64   `json:"bytes_received,omitempty"`         // 上游已返回的字节数（快照）
	ClientFirstByteTime float64 `json:"client_first_byte_time,omitempty"` // 客户端侧首字节响应时间（秒），流式请求有效
}

type activeRequest struct {
	ID          int64
	Model       string
	ClientIP    string
	StartTime   int64 // Unix毫秒
	Streaming   bool
	ChannelID   int64
	ChannelName string
	ChannelType string
	APIKeyUsed  string
	TokenID     int64

	bytesCounter            atomic.Int64 // 上游已返回的字节数（原子累加）
	clientFirstByteTimeUsec atomic.Int64 // 客户端侧首字节响应时间（微秒），CAS保证只写一次，0表示未设置
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

// Register 注册一个新的活跃请求，返回请求ID（用于后续移除）
func (m *activeRequestManager) Register(startTime time.Time, model, clientIP string, streaming bool) int64 {
	id := m.nextID.Add(1)
	req := &activeRequest{
		ID:        id,
		Model:     model,
		ClientIP:  clientIP,
		StartTime: startTime.UnixMilli(),
		Streaming: streaming,
	}
	m.mu.Lock()
	m.requests[id] = req
	m.mu.Unlock()
	return id
}

// Update 更新活跃请求的渠道信息（在选择渠道/key后调用）
// 每次切换渠道/Key 时重置首字节计时和已接收字节，避免前次失败尝试的残留数据误导前端显示
func (m *activeRequestManager) Update(id int64, channelID int64, channelName, channelType, apiKey string, tokenID int64) {
	m.mu.Lock()
	if req, ok := m.requests[id]; ok {
		req.ChannelID = channelID
		req.ChannelName = channelName
		req.ChannelType = channelType
		req.APIKeyUsed = util.MaskAPIKey(apiKey)
		req.TokenID = tokenID
		req.StartTime = time.Now().UnixMilli()
		req.clientFirstByteTimeUsec.Store(0)
		req.bytesCounter.Store(0)
	}
	m.mu.Unlock()
}

// Remove 移除一个活跃请求
func (m *activeRequestManager) Remove(id int64) {
	m.mu.Lock()
	delete(m.requests, id)
	m.mu.Unlock()
}

// AddBytes 原子地增加指定请求的字节数（线程安全）
func (m *activeRequestManager) AddBytes(id int64, n int64) {
	if n <= 0 {
		return
	}
	m.mu.RLock()
	req := m.requests[id]
	m.mu.RUnlock()
	if req != nil {
		req.bytesCounter.Add(n)
	}
}

// SetClientFirstByteTime 设置客户端侧首字节响应时间（CAS保证只写一次，线程安全）
func (m *activeRequestManager) SetClientFirstByteTime(id int64, d time.Duration) {
	if d <= 0 {
		return
	}
	m.mu.RLock()
	req := m.requests[id]
	m.mu.RUnlock()
	if req == nil {
		return
	}
	usec := d.Microseconds()
	if usec <= 0 {
		return
	}
	req.clientFirstByteTimeUsec.CompareAndSwap(0, usec) // 只有首次（0值）才写入
}

// List 返回所有活跃请求的快照（按开始时间降序，最新的在前）
func (m *activeRequestManager) List() []*ActiveRequest {
	m.mu.RLock()
	result := make([]*ActiveRequest, 0, len(m.requests))
	for _, req := range m.requests {
		view := &ActiveRequest{
			ID:            req.ID,
			Model:         req.Model,
			ClientIP:      req.ClientIP,
			StartTime:     req.StartTime,
			Streaming:     req.Streaming,
			ChannelID:     req.ChannelID,
			ChannelName:   req.ChannelName,
			ChannelType:   req.ChannelType,
			APIKeyUsed:    req.APIKeyUsed,
			TokenID:       req.TokenID,
			BytesReceived: req.bytesCounter.Load(),
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
