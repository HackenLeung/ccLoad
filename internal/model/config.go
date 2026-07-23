package model

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	protocolpkg "ccLoad/internal/protocol"
)

const (
	// ProtocolTransformModeLocal keeps extra exposed protocols on the existing local-translation path.
	ProtocolTransformModeLocal = "local"
	// ProtocolTransformModeUpstream forwards extra exposed protocols to upstream natively.
	ProtocolTransformModeUpstream     = "upstream"
	ProtocolCapabilityFunctionTools   = "function_tools"
	ProtocolCapabilityHostedWebSearch = "hosted_web_search"
	ProtocolCapabilityToolSearch      = "tool_search"
	ProtocolCapabilityReasoning       = "reasoning"
	ProtocolCapabilityPromptCache     = "prompt_cache"
	// ExactUpstreamURLMarker marks a configured channel URL as the exact upstream request URL.
	ExactUpstreamURLMarker = "#"
)

var CodexToOpenAICapabilities = []string{
	ProtocolCapabilityFunctionTools,
	ProtocolCapabilityHostedWebSearch,
	ProtocolCapabilityToolSearch,
	ProtocolCapabilityReasoning,
	ProtocolCapabilityPromptCache,
}

// HasExactUpstreamURLMarker reports whether raw ends with the exact upstream URL marker.
func HasExactUpstreamURLMarker(raw string) bool {
	return strings.HasSuffix(strings.TrimSpace(raw), ExactUpstreamURLMarker)
}

// StripExactUpstreamURLMarker trims spaces and removes the exact upstream URL marker when present.
func StripExactUpstreamURLMarker(raw string) string {
	return strings.TrimSuffix(strings.TrimSpace(raw), ExactUpstreamURLMarker)
}

// NormalizeProtocolTransformMode normalizes admin or persisted values and returns an empty string for invalid modes.
func NormalizeProtocolTransformMode(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", ProtocolTransformModeUpstream:
		return ProtocolTransformModeUpstream
	case ProtocolTransformModeLocal:
		return ProtocolTransformModeLocal
	default:
		return ""
	}
}

// ModelEntry 模型配置条目
type ModelEntry struct {
	Model               string              `json:"model"`                           // 真实上游模型名称
	RedirectModel       string              `json:"redirect_model,omitempty"`        // 旧版兼容字段：对外模型 -> 上游模型
	RedirectEnabled     bool                `json:"redirect_enabled,omitempty"`      // 是否启用协议级对外模型
	ProtocolAliases     map[string][]string `json:"protocol_aliases,omitempty"`      // 客户端协议 -> 对外模型列表
	VisionAssistEnabled bool                `json:"vision_assist_enabled,omitempty"` // 收到图片时先调用视觉池转为文本
	VisionPoolEnabled   bool                `json:"vision_pool_enabled,omitempty"`   // 可作为视觉辅助模型
	VisionPriority      int                 `json:"vision_priority,omitempty"`       // 视觉池内优先级，越大越优先
}

// VisionPoolUpdate describes one model's membership in the global vision pool.
type VisionPoolUpdate struct {
	ChannelID int64
	Model     string
	Priority  int
}

// Validate 验证并规范化模型条目
// 返回 error 如果验证失败，否则返回 nil
// 副作用：会 trim 空白字符并写回 Model 和 RedirectModel 字段
func (e *ModelEntry) Validate() error {
	e.Model = strings.TrimSpace(e.Model)
	if e.Model == "" {
		return errors.New("model cannot be empty")
	}
	if strings.ContainsAny(e.Model, "\x00\r\n") {
		return errors.New("model contains illegal characters")
	}

	e.RedirectModel = strings.TrimSpace(e.RedirectModel)
	if strings.ContainsAny(e.RedirectModel, "\x00\r\n") {
		return errors.New("redirect_model contains illegal characters")
	}
	if len(e.ProtocolAliases) > 0 {
		normalized := make(map[string][]string, len(e.ProtocolAliases))
		for rawProtocol, aliases := range e.ProtocolAliases {
			protocolName := strings.ToLower(strings.TrimSpace(rawProtocol))
			switch protocolName {
			case "anthropic", "openai", "gemini", "codex":
			default:
				return errors.New("protocol_aliases contains invalid protocol")
			}
			seen := make(map[string]struct{}, len(aliases))
			for _, rawAlias := range aliases {
				alias := strings.TrimSpace(rawAlias)
				if alias == "" {
					continue
				}
				if strings.ContainsAny(alias, "\x00\r\n") {
					return errors.New("protocol_aliases contains illegal characters")
				}
				key := strings.ToLower(alias)
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
				normalized[protocolName] = append(normalized[protocolName], alias)
			}
		}
		e.ProtocolAliases = normalized
	}
	if e.VisionPriority < 0 {
		return errors.New("vision_priority must be >= 0")
	}
	return nil
}

// FindModelEntry resolves a public model name to its upstream model entry.
func (c *Config) FindModelEntry(modelName, clientProtocol string) *ModelEntry {
	if c == nil {
		return nil
	}
	if redirected, ok := c.GetRedirectModel(modelName, clientProtocol); ok {
		modelName = redirected
	}
	for i := range c.ModelEntries {
		if strings.EqualFold(c.ModelEntries[i].Model, modelName) {
			return &c.ModelEntries[i]
		}
	}
	return nil
}

// 自定义请求规则动作常量
const (
	RuleActionRemove   = "remove"
	RuleActionOverride = "override"
	RuleActionAppend   = "append"
)

// CustomHeaderRule 单条自定义 HTTP 请求头规则
type CustomHeaderRule struct {
	Action string `json:"action"`          // remove | override | append
	Name   string `json:"name"`            // header 名，保持原大小写
	Value  string `json:"value,omitempty"` // remove 时忽略
}

// CustomBodyRule 单条自定义 JSON 请求体规则
type CustomBodyRule struct {
	Action string          `json:"action"`          // remove | override
	Path   string          `json:"path"`            // 点分路径，支持整数数组索引
	Value  json.RawMessage `json:"value,omitempty"` // remove 时忽略；任意 JSON 字面量
}

// CustomRequestRules 渠道级自定义请求改写规则集
type CustomRequestRules struct {
	Headers []CustomHeaderRule `json:"headers,omitempty"`
	Body    []CustomBodyRule   `json:"body,omitempty"`
}

// IsEmpty 当两类规则均为空时返回 true
func (r *CustomRequestRules) IsEmpty() bool {
	if r == nil {
		return true
	}
	return len(r.Headers) == 0 && len(r.Body) == 0
}

// Config 渠道配置
type Config struct {
	ID                    int64                      `json:"id"`
	Name                  string                     `json:"name"`
	ChannelType           string                     `json:"channel_type"` // 渠道类型: "anthropic" | "codex" | "openai" | "gemini"，默认anthropic
	ProtocolTransformMode string                     `json:"protocol_transform_mode,omitempty"`
	ProtocolTransforms    []string                   `json:"protocol_transforms,omitempty"`
	ProtocolCapabilities  map[string]map[string]bool `json:"protocol_capabilities,omitempty"`
	// ProtocolPriorities 协议级优先级覆盖。key 为客户端协议（anthropic/codex/openai/gemini）。
	// 缺省时回退到 Priority。
	ProtocolPriorities    map[string]int `json:"protocol_priorities,omitempty"`
	URL                   string         `json:"url"`
	Priority              int            `json:"priority"`
	RPMLimit              int            `json:"rpm_limit"`       // 每分钟请求数限制，0表示无限制
	MaxConcurrency        int            `json:"max_concurrency"` // 最大并发请求数，0表示无限制
	Enabled               bool           `json:"enabled"`
	ScheduledCheckEnabled bool           `json:"scheduled_check_enabled"`
	ScheduledCheckModel   string         `json:"scheduled_check_model"`

	// 模型配置（统一管理模型和重定向）
	ModelEntries []ModelEntry `json:"models"`

	// 渠道级冷却（从cooldowns表迁移）
	CooldownUntil      int64 `json:"cooldown_until"`       // Unix秒时间戳，0表示无冷却
	CooldownDurationMs int64 `json:"cooldown_duration_ms"` // 冷却持续时间（毫秒）

	// 每日成本限额
	DailyCostLimit float64 `json:"daily_cost_limit"` // 每日成本限额（美元），0表示无限制

	// 成本倍率：标准成本×倍率=实际计费成本，默认1
	CostMultiplier float64 `json:"cost_multiplier"`

	// 自定义请求规则（nil 表示无改写）
	CustomRequestRules *CustomRequestRules `json:"custom_request_rules,omitempty"`

	// 渠道级代理（http/https/socks5/socks5h），空串=环境变量代理
	ProxyURL string `json:"proxy_url,omitempty"`

	CreatedAt JSONTime `json:"created_at"` // 使用JSONTime确保序列化格式一致（RFC3339）
	UpdatedAt JSONTime `json:"updated_at"` // 使用JSONTime确保序列化格式一致（RFC3339）

	// 缓存Key数量，避免冷却判断时的N+1查询
	KeyCount int `json:"key_count"` // API Key数量（查询时JOIN计算）

	// 运行时路由标记：该候选来自“所有渠道冷却”兜底，不持久化、不序列化。
	CooldownFallback bool `json:"-"`

	// 模型查找索引（懒加载，不序列化）
	modelIndex map[string]*ModelEntry `json:"-"`
	aliasIndex map[string]*ModelEntry `json:"-"`
	indexMu    sync.RWMutex           `json:"-"` // 保护索引的并发访问
}

// Clone 返回 Config 的深拷贝。
// 拷贝所有可变字段（ModelEntries / ProtocolTransforms / ProtocolCapabilities），
// 重置懒加载索引（modelIndex + indexMu），避免共享 sync.RWMutex 与指向旧 slice 的 map。
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	dst := &Config{
		ID:                    c.ID,
		Name:                  c.Name,
		ChannelType:           c.ChannelType,
		ProtocolTransformMode: c.ProtocolTransformMode,
		ProtocolTransforms:    append([]string(nil), c.ProtocolTransforms...),
		URL:                   c.URL,
		Priority:              c.Priority,
		RPMLimit:              c.RPMLimit,
		MaxConcurrency:        c.MaxConcurrency,
		Enabled:               c.Enabled,
		ScheduledCheckEnabled: c.ScheduledCheckEnabled,
		ScheduledCheckModel:   c.ScheduledCheckModel,
		CooldownUntil:         c.CooldownUntil,
		CooldownDurationMs:    c.CooldownDurationMs,
		DailyCostLimit:        c.DailyCostLimit,
		CostMultiplier:        c.CostMultiplier,
		CustomRequestRules:    c.CustomRequestRules,
		ProxyURL:              c.ProxyURL,
		CreatedAt:             c.CreatedAt,
		UpdatedAt:             c.UpdatedAt,
		KeyCount:              c.KeyCount,
		CooldownFallback:      c.CooldownFallback,
	}
	if c.ModelEntries != nil {
		dst.ModelEntries = make([]ModelEntry, len(c.ModelEntries))
		for i := range c.ModelEntries {
			dst.ModelEntries[i] = c.ModelEntries[i]
			if len(c.ModelEntries[i].ProtocolAliases) > 0 {
				dst.ModelEntries[i].ProtocolAliases = make(map[string][]string, len(c.ModelEntries[i].ProtocolAliases))
				for protocolName, aliases := range c.ModelEntries[i].ProtocolAliases {
					dst.ModelEntries[i].ProtocolAliases[protocolName] = append([]string(nil), aliases...)
				}
			}
		}
	}
	if len(c.ProtocolPriorities) > 0 {
		dst.ProtocolPriorities = make(map[string]int, len(c.ProtocolPriorities))
		for protocol, priority := range c.ProtocolPriorities {
			dst.ProtocolPriorities[protocol] = priority
		}
	}
	if len(c.ProtocolCapabilities) > 0 {
		dst.ProtocolCapabilities = make(map[string]map[string]bool, len(c.ProtocolCapabilities))
		for protocolName, capabilities := range c.ProtocolCapabilities {
			copied := make(map[string]bool, len(capabilities))
			for capability, enabled := range capabilities {
				copied[capability] = enabled
			}
			dst.ProtocolCapabilities[protocolName] = copied
		}
	}
	return dst
}

// ProtocolCapabilityEnabled preserves legacy behavior by treating missing
// capability configuration as enabled.
func (c *Config) ProtocolCapabilityEnabled(protocolName, capability string) bool {
	if c == nil {
		return true
	}
	protocolName = strings.TrimSpace(strings.ToLower(protocolName))
	capability = strings.TrimSpace(strings.ToLower(capability))
	capabilities, configured := c.ProtocolCapabilities[protocolName]
	if !configured {
		return true
	}
	enabled, configured := capabilities[capability]
	if !configured {
		return true
	}
	return enabled
}

// GetModels 获取所有支持的模型名称列表
func (c *Config) GetModels() []string {
	models := make([]string, 0, len(c.ModelEntries))
	for _, e := range c.ModelEntries {
		models = append(models, e.Model)
	}
	return models
}

// GetProtocolTransforms 返回去重后的额外协议转换集合。
func (c *Config) GetProtocolTransforms() []string {
	if len(c.ProtocolTransforms) == 0 {
		return nil
	}
	base := c.GetChannelType()
	mode := c.GetProtocolTransformMode()
	seen := make(map[string]struct{}, len(c.ProtocolTransforms))
	transforms := make([]string, 0, len(c.ProtocolTransforms))
	for _, protocol := range c.ProtocolTransforms {
		protocol = strings.TrimSpace(strings.ToLower(protocol))
		if protocol == "" || protocol == base {
			continue
		}
		if mode == ProtocolTransformModeLocal && !protocolpkg.SupportsTransform(protocolpkg.Protocol(protocol), protocolpkg.Protocol(base)) {
			continue
		}
		if _, ok := seen[protocol]; ok {
			continue
		}
		seen[protocol] = struct{}{}
		transforms = append(transforms, protocol)
	}
	slices.Sort(transforms)
	return transforms
}

// GetProtocolTransformMode returns the normalized transform mode and defaults to upstream mode.
func (c *Config) GetProtocolTransformMode() string {
	mode := NormalizeProtocolTransformMode(c.ProtocolTransformMode)
	if mode == "" {
		return ProtocolTransformModeUpstream
	}
	return mode
}

// ResolveUpstreamProtocol returns the runtime upstream protocol for the current client protocol under this channel config.
func (c *Config) ResolveUpstreamProtocol(clientProtocol string) string {
	clientProtocol = strings.TrimSpace(strings.ToLower(clientProtocol))
	if clientProtocol == "" {
		return c.GetChannelType()
	}
	if c.GetProtocolTransformMode() == ProtocolTransformModeUpstream && c.SupportsProtocol(clientProtocol) {
		return clientProtocol
	}
	return c.GetChannelType()
}

// PriorityForProtocol 返回指定客户端协议下的调度优先级。
// 有协议级覆盖时使用覆盖值，否则回退到全局 Priority。
func (c *Config) PriorityForProtocol(protocol string) int {
	if c == nil {
		return 0
	}
	protocol = strings.TrimSpace(strings.ToLower(protocol))
	if protocol != "" && len(c.ProtocolPriorities) > 0 {
		if priority, ok := c.ProtocolPriorities[protocol]; ok {
			return priority
		}
	}
	return c.Priority
}

// ApplyProtocolPriority 将指定协议的优先级写回 Config.Priority（仅用于当前请求候选列表）。
func (c *Config) ApplyProtocolPriority(protocol string) {
	if c == nil {
		return
	}
	c.Priority = c.PriorityForProtocol(protocol)
}

// SupportsProtocol 检查渠道是否暴露指定客户端协议。
func (c *Config) SupportsProtocol(protocol string) bool {
	protocol = strings.TrimSpace(strings.ToLower(protocol))
	if protocol == "" {
		return false
	}
	if c.GetChannelType() == protocol {
		return true
	}
	return slices.Contains(c.GetProtocolTransforms(), protocol)
}

// SupportedProtocols 返回渠道对外暴露的全部客户端协议集合。
func (c *Config) SupportedProtocols() []string {
	protocols := append([]string{c.GetChannelType()}, c.GetProtocolTransforms()...)
	slices.Sort(protocols)
	return slices.Compact(protocols)
}

// GetURLs 解析URL字段，返回URL列表
// 支持换行分隔多个URL，向后兼容单URL场景
func (c *Config) GetURLs() []string {
	raw := c.URL
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if !strings.Contains(raw, "\n") {
		return []string{trimmed}
	}
	lines := strings.Split(raw, "\n")
	urls := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, exists := seen[line]; exists {
			continue
		}
		seen[line] = struct{}{}
		urls = append(urls, line)
	}
	return urls
}

// buildIndexIfNeeded 懒加载构建模型查找索引（性能优化：O(n) → O(1)）
// 使用双重检查锁定（DCL）模式保证并发安全
func (c *Config) buildIndexIfNeeded() {
	// 快路径：读锁检查
	c.indexMu.RLock()
	if c.modelIndex != nil {
		c.indexMu.RUnlock()
		return
	}
	c.indexMu.RUnlock()

	// 慢路径：写锁构建
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	// 双重检查：可能其他 goroutine 已构建
	if c.modelIndex != nil {
		return
	}
	c.modelIndex = make(map[string]*ModelEntry, len(c.ModelEntries))
	c.aliasIndex = make(map[string]*ModelEntry)
	for i := range c.ModelEntries {
		entry := &c.ModelEntries[i]
		c.modelIndex[entry.Model] = entry
		if !entry.RedirectEnabled {
			continue
		}
		for protocolName, aliases := range entry.ProtocolAliases {
			for _, alias := range aliases {
				c.aliasIndex[modelAliasIndexKey(protocolName, alias)] = entry
			}
		}
	}
}

func modelAliasIndexKey(protocolName, alias string) string {
	return strings.ToLower(strings.TrimSpace(protocolName)) + "\x00" + strings.ToLower(strings.TrimSpace(alias))
}

// GetRedirectModel 获取模型的重定向目标
// 返回 (目标模型, 是否有重定向)
func (c *Config) GetRedirectModel(model string, clientProtocol ...string) (string, bool) {
	c.buildIndexIfNeeded()
	c.indexMu.RLock()
	defer c.indexMu.RUnlock()
	if len(clientProtocol) > 0 {
		if entry, exists := c.aliasIndex[modelAliasIndexKey(clientProtocol[0], model)]; exists {
			return entry.Model, entry.Model != model
		}
	}
	// 兼容旧配置对象：旧语义是 Model 为对外模型、RedirectModel 为上游模型。
	if entry, exists := c.modelIndex[model]; exists && entry.RedirectModel != "" {
		return entry.RedirectModel, true
	}
	if len(clientProtocol) == 0 {
		for _, entry := range c.ModelEntries {
			if !entry.RedirectEnabled {
				continue
			}
			for _, aliases := range entry.ProtocolAliases {
				if slices.ContainsFunc(aliases, func(alias string) bool {
					return strings.EqualFold(alias, model)
				}) {
					return entry.Model, entry.Model != model
				}
			}
		}
	}
	return "", false
}

// SupportsModel 检查渠道是否支持指定模型
func (c *Config) SupportsModel(model string, clientProtocol ...string) bool {
	c.buildIndexIfNeeded()
	c.indexMu.RLock()
	defer c.indexMu.RUnlock()
	if len(clientProtocol) > 0 {
		if _, exists := c.aliasIndex[modelAliasIndexKey(clientProtocol[0], model)]; exists {
			return true
		}
	}
	if _, exists := c.modelIndex[model]; exists {
		// 真实上游模型始终直接可用；协议别名只是额外入口。
		return true
	}
	if len(clientProtocol) == 0 {
		for _, entry := range c.ModelEntries {
			for _, aliases := range entry.ProtocolAliases {
				if slices.ContainsFunc(aliases, func(alias string) bool {
					return strings.EqualFold(alias, model)
				}) {
					return true
				}
			}
		}
	}
	return false
}

// GetChannelType 默认返回"anthropic"（Claude API）
func (c *Config) GetChannelType() string {
	if c.ChannelType == "" {
		return "anthropic"
	}
	return c.ChannelType
}

// IsCoolingDown 检查渠道是否处于冷却状态
func (c *Config) IsCoolingDown(now time.Time) bool {
	return c.CooldownUntil > now.Unix()
}

// KeyStrategy 常量定义
const (
	KeyStrategySequential = "sequential"  // 顺序选择：按索引顺序尝试Key
	KeyStrategyRoundRobin = "round_robin" // 轮询选择：均匀分布请求到各个Key
)

// IsValidKeyStrategy 验证KeyStrategy是否有效
func IsValidKeyStrategy(s string) bool {
	return s == "" || s == KeyStrategySequential || s == KeyStrategyRoundRobin
}

// APIKey 表示渠道的 API 密钥配置
type APIKey struct {
	ID        int64  `json:"id"`
	ChannelID int64  `json:"channel_id"`
	KeyIndex  int    `json:"key_index"`
	APIKey    string `json:"api_key"`
	Note      string `json:"note"`

	KeyStrategy string `json:"key_strategy"` // "sequential" | "round_robin"
	Disabled    bool   `json:"disabled"`

	// Key级冷却（从key_cooldowns表迁移）
	CooldownUntil      int64 `json:"cooldown_until"`
	CooldownDurationMs int64 `json:"cooldown_duration_ms"`

	CreatedAt JSONTime `json:"created_at"`
	UpdatedAt JSONTime `json:"updated_at"`
}

// IsCoolingDown 检查密钥是否处于冷却状态
func (k *APIKey) IsCoolingDown(now time.Time) bool {
	return k.CooldownUntil > now.Unix()
}

// ChannelWithKeys 渠道和API Keys的完整数据
// 用于批量导入导出等需要完整渠道数据的场景
type ChannelWithKeys struct {
	Config  *Config  `json:"config"`
	APIKeys []APIKey `json:"api_keys"` // 不使用指针避免额外分配
}

// FuzzyMatchModel 模糊匹配模型名称
// 当精确匹配失败时，查找包含 query 子串的模型，按版本排序返回最新的
// 返回 (匹配到的模型名, 是否匹配成功)
func (c *Config) FuzzyMatchModel(query string, clientProtocol ...string) (string, bool) {
	if query == "" {
		return "", false
	}

	queryLower := strings.ToLower(query)
	var matches []string

	for _, entry := range c.ModelEntries {
		if entry.RedirectEnabled && len(clientProtocol) > 0 {
			for _, alias := range entry.ProtocolAliases[strings.ToLower(strings.TrimSpace(clientProtocol[0]))] {
				if strings.Contains(strings.ToLower(alias), queryLower) {
					matches = append(matches, alias)
				}
			}
		}
		candidate := entry.Model
		if strings.Contains(strings.ToLower(candidate), queryLower) {
			matches = append(matches, candidate)
		}
	}

	if len(matches) == 0 {
		return "", false
	}
	if len(matches) == 1 {
		return matches[0], true
	}

	// 多个匹配：按版本排序，取最新
	sortModelsByVersion(matches)
	return matches[0], true
}

// sortModelsByVersion 按版本排序模型列表（最新优先）
// 排序优先级：1.日期后缀 2.版本数字 3.字典序
// 使用标准库 slices.SortFunc，O(n log n) 复杂度
func sortModelsByVersion(models []string) {
	slices.SortFunc(models, func(a, b string) int {
		return -compareModelVersion(a, b) // 降序（最新优先）
	})
}

// compareModelVersion 比较两个模型版本
// 返回 >0 表示 a 更新，<0 表示 b 更新，0 表示相同
func compareModelVersion(a, b string) int {
	// 1. 日期后缀优先（YYYYMMDD）
	dateA := extractDateSuffix(a)
	dateB := extractDateSuffix(b)
	if dateA != dateB {
		if dateA > dateB {
			return 1
		}
		return -1
	}

	// 2. 版本数字序列比较
	verA := extractVersionNumbers(a)
	verB := extractVersionNumbers(b)
	maxLen := len(verA)
	if len(verB) > maxLen {
		maxLen = len(verB)
	}
	for i := 0; i < maxLen; i++ {
		va, vb := 0, 0
		if i < len(verA) {
			va = verA[i]
		}
		if i < len(verB) {
			vb = verB[i]
		}
		if va != vb {
			return va - vb
		}
	}

	// 3. 兜底：字典序
	if a > b {
		return 1
	} else if a < b {
		return -1
	}
	return 0
}

// extractDateSuffix 提取模型名称末尾的日期后缀（YYYYMMDD）
// 返回日期字符串，无日期返回空串
func extractDateSuffix(model string) string {
	// 查找最后一个分隔符
	lastDash := strings.LastIndexByte(model, '-')
	lastDot := strings.LastIndexByte(model, '.')
	lastSep := lastDash
	if lastDot > lastSep {
		lastSep = lastDot
	}
	if lastSep < 0 {
		return ""
	}

	suffix := model[lastSep+1:]
	if len(suffix) != 8 {
		return ""
	}

	// 验证是否全数字
	for i := 0; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return ""
		}
	}

	// 简单验证年份范围
	year := (int(suffix[0]-'0') * 1000) + (int(suffix[1]-'0') * 100) +
		(int(suffix[2]-'0') * 10) + int(suffix[3]-'0')
	if year < 2000 || year > 2100 {
		return ""
	}

	return suffix
}

// extractVersionNumbers 提取模型名称中的版本数字
// 例如：gpt-5.2 → [5,2], claude-sonnet-4-5-20250929 → [4,5]
func extractVersionNumbers(model string) []int {
	// 移除日期后缀避免干扰
	if date := extractDateSuffix(model); date != "" {
		model = model[:len(model)-len(date)-1]
	}

	var nums []int
	var current int
	inNumber := false

	for i := 0; i < len(model); i++ {
		c := model[i]
		if c >= '0' && c <= '9' {
			current = current*10 + int(c-'0')
			inNumber = true
		} else {
			if inNumber {
				nums = append(nums, current)
				current = 0
				inNumber = false
			}
		}
	}
	if inNumber {
		nums = append(nums, current)
	}

	return nums
}

// HeaderRules 返回自定义请求头规则，nil-safe
func (c *Config) HeaderRules() []CustomHeaderRule {
	if c == nil || c.CustomRequestRules == nil {
		return nil
	}
	return c.CustomRequestRules.Headers
}

// BodyRules 返回自定义请求体规则，nil-safe
func (c *Config) BodyRules() []CustomBodyRule {
	if c == nil || c.CustomRequestRules == nil {
		return nil
	}
	return c.CustomRequestRules.Body
}
