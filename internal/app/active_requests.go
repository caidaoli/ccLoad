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
	ID            int64  `json:"id"`
	Model         string `json:"model"`
	ClientIP      string `json:"client_ip"`
	StartTime     int64  `json:"start_time"` // Unix秒
	Streaming     bool   `json:"is_streaming"`
	ChannelID     int64  `json:"channel_id,omitempty"`
	ChannelName   string `json:"channel_name,omitempty"`
	ChannelType   string `json:"channel_type,omitempty"`   // 渠道类型（用于前端筛选）
	APIKeyUsed    string `json:"api_key_used,omitempty"`   // 脱敏后的key
	TokenID       int64  `json:"token_id,omitempty"`       // 令牌ID（用于前端筛选，0表示无令牌）
	BytesReceived int64  `json:"bytes_received,omitempty"` // 上游已返回的字节数（从 bytesCounter 拷贝）

	bytesCounter *atomic.Int64 `json:"-"` // 内部字节计数器（不序列化）
}

// activeRequestManager 管理进行中的请求（内存状态，不持久化）
type activeRequestManager struct {
	mu       sync.RWMutex
	requests map[int64]*ActiveRequest
	nextID   atomic.Int64
}

func newActiveRequestManager() *activeRequestManager {
	return &activeRequestManager{
		requests: make(map[int64]*ActiveRequest),
	}
}

// Register 注册一个新的活跃请求，返回请求ID（用于后续移除）
func (m *activeRequestManager) Register(model, clientIP string, streaming bool) int64 {
	id := m.nextID.Add(1)
	req := &ActiveRequest{
		ID:           id,
		Model:        model,
		ClientIP:     clientIP,
		StartTime:    time.Now().Unix(),
		Streaming:    streaming,
		bytesCounter: &atomic.Int64{}, // 初始化字节计数器
	}
	m.mu.Lock()
	m.requests[id] = req
	m.mu.Unlock()
	return id
}

// Update 更新活跃请求的渠道信息（在选择渠道/key后调用）
func (m *activeRequestManager) Update(id int64, channelID int64, channelName, channelType, apiKey string, tokenID int64) {
	m.mu.Lock()
	if req, ok := m.requests[id]; ok {
		req.ChannelID = channelID
		req.ChannelName = channelName
		req.ChannelType = channelType
		req.APIKeyUsed = util.MaskAPIKey(apiKey)
		req.TokenID = tokenID
	}
	m.mu.Unlock()
}

// Remove 移除一个活跃请求
func (m *activeRequestManager) Remove(id int64) {
	m.mu.Lock()
	delete(m.requests, id)
	m.mu.Unlock()
}

// AddBytes 原子地增加指定请求的字节数（线程安全，避免TOCTTOU竞态）
func (m *activeRequestManager) AddBytes(id int64, n int64) {
	if n <= 0 {
		return
	}
	m.mu.RLock()
	req := m.requests[id]
	m.mu.RUnlock()
	if req != nil && req.bytesCounter != nil {
		req.bytesCounter.Add(n)
	}
}

// List 返回所有活跃请求的快照（按开始时间降序，最新的在前）
func (m *activeRequestManager) List() []*ActiveRequest {
	m.mu.RLock()
	result := make([]*ActiveRequest, 0, len(m.requests))
	for _, req := range m.requests {
		// 安全拷贝结构体（不拷贝 bytesCounter 指针）
		copied := *req
		copied.bytesCounter = nil // 清空内部字段，避免外部访问
		// 从 bytesCounter 读取字节数（原子操作，无需额外锁）
		if req.bytesCounter != nil {
			copied.BytesReceived = req.bytesCounter.Load()
		}
		result = append(result, &copied)
	}
	m.mu.RUnlock()
	// 按开始时间降序排序
	sort.Slice(result, func(i, j int) bool {
		return result[i].StartTime > result[j].StartTime
	})
	return result
}
