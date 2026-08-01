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

var (
	errManualChannelSkip             = errors.New("manual channel skip")
	errActiveRequestNotFound         = errors.New("active request not found")
	errActiveRequestAttemptMismatch  = errors.New("active request attempt changed")
	errActiveRequestSkipNotAvailable = errors.New("active request cannot be skipped")
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
	ChannelType         string  `json:"channel_type,omitempty"` // 渠道类型（用于前端筛选）
	UpstreamProtocol    string  `json:"upstream_protocol,omitempty"`
	APIKeyUsed          string  `json:"api_key_used,omitempty"`           // 脱敏后的key
	TokenID             int64   `json:"token_id,omitempty"`               // 令牌ID（用于前端筛选，0表示无令牌）
	BaseURL             string  `json:"base_url,omitempty"`               // 当前使用的上游URL
	BytesReceived       int64   `json:"bytes_received,omitempty"`         // 上游已返回的字节数（快照）
	ClientFirstByteTime float64 `json:"client_first_byte_time,omitempty"` // 客户端侧首字节响应时间（秒），流式请求有效
	CostMultiplier      float64 `json:"cost_multiplier"`                  // 渠道成本倍率
	DebugLogAvailable   bool    `json:"debug_log_available,omitempty"`    // 运行中请求是否已有可读取的调试快照
	ThinkingEffort      string  `json:"thinking_effort,omitempty"`
	AttemptID           int64   `json:"attempt_id,omitempty"`     // 当前上游尝试编号，用于防止过期操作取消新尝试
	CanSkip             bool    `json:"can_skip,omitempty"`       // 尚未向客户端提交响应时允许跳过当前渠道
	SkipRequested       bool    `json:"skip_requested,omitempty"` // 已收到跳过请求，等待代理循环切换渠道
}

type activeRequest struct {
	ID               int64
	Model            string
	ClientIP         string
	StartTime        int64 // Unix毫秒
	Streaming        bool
	ChannelID        int64
	ChannelName      string
	ChannelType      string
	UpstreamProtocol string
	APIKeyUsed       string
	TokenID          int64
	BaseURL          string

	CostMultiplier float64 // 渠道成本倍率
	ThinkingEffort string
	debugCapture   *debugCapture

	attemptID     int64
	attemptCancel context.CancelCauseFunc
	canSkip       bool
	skipRequested bool

	bytesCounter            atomic.Int64 // 上游已返回的字节数（原子累加）
	clientFirstByteTimeUsec atomic.Int64 // 客户端侧首字节响应时间（微秒），CAS保证只写一次，0表示未设置
}

func (m *activeRequestManager) SetThinkingEffort(id int64, thinkingEffort string) {
	m.mu.Lock()
	if req, ok := m.requests[id]; ok {
		req.ThinkingEffort = normalizeThinkingEffort(thinkingEffort)
	}
	m.mu.Unlock()
}

// activeRequestManager 管理进行中的请求（内存状态，不持久化）
type activeRequestManager struct {
	mu            sync.RWMutex
	requests      map[int64]*activeRequest
	nextID        atomic.Int64
	nextAttemptID atomic.Int64
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
func (m *activeRequestManager) Update(id int64, channelID int64, channelName, channelType, upstreamProtocol, apiKey string, tokenID int64, costMultiplier float64) {
	m.mu.Lock()
	if req, ok := m.requests[id]; ok {
		req.ChannelID = channelID
		req.ChannelName = channelName
		req.ChannelType = channelType
		req.UpstreamProtocol = upstreamProtocol
		req.APIKeyUsed = util.MaskAPIKey(apiKey)
		req.TokenID = tokenID
		req.CostMultiplier = costMultiplier
		req.StartTime = time.Now().UnixMilli()
		req.clientFirstByteTimeUsec.Store(0)
		req.bytesCounter.Store(0)
	}
	m.mu.Unlock()
}

// SetBaseURL 更新活跃请求的上游URL（在URL循环中调用）
func (m *activeRequestManager) SetBaseURL(id int64, baseURL string) {
	m.mu.Lock()
	if req, ok := m.requests[id]; ok {
		req.BaseURL = baseURL
	}
	m.mu.Unlock()
}

// BeginAttempt 注册当前正在执行的上游尝试。attemptID 仅在该尝试存活期间暴露给前端，
// 管理端必须带回它，避免旧轮询页面误取消已经切换后的渠道。
func (m *activeRequestManager) BeginAttempt(id int64, cancel context.CancelCauseFunc) (int64, bool) {
	if cancel == nil {
		return 0, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	req, ok := m.requests[id]
	if !ok {
		return 0, false
	}

	attemptID := m.nextAttemptID.Add(1)
	req.attemptID = attemptID
	req.attemptCancel = cancel
	req.canSkip = true
	req.skipRequested = false
	return attemptID, true
}

// EndAttempt 清除已经结束的尝试控制信息。只清理匹配的编号，避免慢路径覆盖新尝试。
func (m *activeRequestManager) EndAttempt(id, attemptID int64) {
	if attemptID <= 0 {
		return
	}

	m.mu.Lock()
	if req, ok := m.requests[id]; ok && req.attemptID == attemptID {
		req.attemptID = 0
		req.attemptCancel = nil
		req.canSkip = false
		req.skipRequested = false
	}
	m.mu.Unlock()
}

// PrepareResponseCommit 原子地关闭当前尝试的跳过权限。
// 调用方必须在实际写入客户端响应头之前调用；若管理员已请求跳过，则返回专用原因并保持未提交状态。
func (m *activeRequestManager) PrepareResponseCommit(id, attemptID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	req, ok := m.requests[id]
	if !ok || req.attemptID != attemptID {
		return errActiveRequestAttemptMismatch
	}
	if req.skipRequested {
		return errManualChannelSkip
	}
	req.canSkip = false
	return nil
}

// RequestChannelSkip 取消当前上游尝试。取消函数必须在解锁后调用，避免取消回调与状态锁互相等待。
func (m *activeRequestManager) RequestChannelSkip(id, attemptID int64) error {
	m.mu.Lock()
	req, ok := m.requests[id]
	if !ok {
		m.mu.Unlock()
		return errActiveRequestNotFound
	}
	if req.attemptID != attemptID {
		m.mu.Unlock()
		return errActiveRequestAttemptMismatch
	}
	if !req.canSkip || req.attemptCancel == nil || req.skipRequested {
		m.mu.Unlock()
		return errActiveRequestSkipNotAvailable
	}

	cancel := req.attemptCancel
	req.skipRequested = true
	req.canSkip = false
	m.mu.Unlock()

	cancel(errManualChannelSkip)
	return nil
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
			ID:                req.ID,
			Model:             req.Model,
			ClientIP:          req.ClientIP,
			StartTime:         req.StartTime,
			Streaming:         req.Streaming,
			ChannelID:         req.ChannelID,
			ChannelName:       req.ChannelName,
			ChannelType:       req.ChannelType,
			UpstreamProtocol:  req.UpstreamProtocol,
			APIKeyUsed:        req.APIKeyUsed,
			TokenID:           req.TokenID,
			BaseURL:           req.BaseURL,
			BytesReceived:     req.bytesCounter.Load(),
			CostMultiplier:    req.CostMultiplier,
			DebugLogAvailable: req.debugCapture != nil,
			ThinkingEffort:    req.ThinkingEffort,
			AttemptID:         req.attemptID,
			CanSkip:           req.canSkip,
			SkipRequested:     req.skipRequested,
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
