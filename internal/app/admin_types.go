package app

import (
	"encoding/json"
	"fmt"
	neturl "net/url"
	"slices"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"
)

// ==================== 共享数据结构 ====================
// 从admin.go提取共享类型,遵循SRP原则

// ChannelRequest 渠道创建/更新请求结构
type ChannelRequest struct {
	Name                  string                     `json:"name" binding:"required"`
	APIKey                string                     `json:"api_key"`
	APIKeys               []ChannelAPIKeyRequest     `json:"api_keys,omitempty"`
	ChannelType           string                     `json:"channel_type,omitempty"` // 渠道类型:anthropic, codex, gemini
	ProtocolTransformMode string                     `json:"protocol_transform_mode,omitempty"`
	ProtocolTransforms    []string                   `json:"protocol_transforms,omitempty"`
	ProtocolCapabilities  map[string]map[string]bool `json:"protocol_capabilities,omitempty"`
	KeyStrategy           string                     `json:"key_strategy,omitempty"` // Key使用策略:sequential, round_robin
	URL                   string                     `json:"url" binding:"required"`
	Priority              int                        `json:"priority"`
	RPMLimit              int                        `json:"rpm_limit"`                       // 每分钟请求数限制，0表示无限制
	MaxConcurrency        int                        `json:"max_concurrency"`                 // 最大并发请求数，0表示无限制
	Models                []model.ModelEntry         `json:"models" binding:"required,min=1"` // 模型配置（包含重定向）
	Enabled               bool                       `json:"enabled"`
	ScheduledCheckEnabled bool                       `json:"scheduled_check_enabled"`
	ScheduledCheckModel   string                     `json:"scheduled_check_model"`
	DailyCostLimit        float64                    `json:"daily_cost_limit"` // 每日成本限额（美元），0表示无限制
	CostMultiplier        float64                    `json:"cost_multiplier"`  // 成本倍率（默认1，0=免费，>=0）
	CustomRequestRules    *model.CustomRequestRules  `json:"custom_request_rules,omitempty"`
	ProxyURL              string                     `json:"proxy_url,omitempty"` // 渠道级代理（http/https/socks5/socks5h）
}

// ChannelAPIKeyRequest describes one submitted API key and its admin-only note.
type ChannelAPIKeyRequest struct {
	APIKey string `json:"api_key"`
	Note   string `json:"note,omitempty"`
}

const maxAPIKeyNoteLength = 512

func (cr *ChannelRequest) normalizeAPIKeys() []ChannelAPIKeyRequest {
	if len(cr.APIKeys) > 0 {
		keys := make([]ChannelAPIKeyRequest, 0, len(cr.APIKeys))
		for _, item := range cr.APIKeys {
			apiKey := strings.TrimSpace(item.APIKey)
			if apiKey == "" {
				continue
			}
			keys = append(keys, ChannelAPIKeyRequest{
				APIKey: apiKey,
				Note:   strings.TrimSpace(item.Note),
			})
		}
		return keys
	}

	legacyKeys := util.ParseAPIKeys(cr.APIKey)
	keys := make([]ChannelAPIKeyRequest, 0, len(legacyKeys))
	for _, apiKey := range legacyKeys {
		keys = append(keys, ChannelAPIKeyRequest{APIKey: apiKey})
	}
	return keys
}

func apiKeyStrings(keys []ChannelAPIKeyRequest) []string {
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		values = append(values, key.APIKey)
	}
	return values
}

func validateChannelBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url cannot be empty")
	}

	exactURL := model.HasExactUpstreamURLMarker(raw)
	parseRaw := raw
	if exactURL {
		parseRaw = model.StripExactUpstreamURLMarker(raw)
	}

	u, err := neturl.Parse(parseRaw)
	if err != nil || u == nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid url: %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid url scheme: %q (allowed: http, https)", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("url must not contain user info")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("url must not contain query or fragment")
	}

	// [FIX] 只禁止包含 /v1 的 path（防止误填 API endpoint 如 /v1/messages）
	// 允许其他 path（如 /api, /openai 等用于反向代理或 API gateway）
	if !exactURL && strings.Contains(u.Path, "/v1") {
		return "", fmt.Errorf("url should not contain API endpoint path like /v1 (current path: %q)", u.Path)
	}

	// 强制返回标准化格式（scheme://host+path，移除 trailing slash）
	// 例如: "https://example.com/api/" → "https://example.com/api"
	normalizedPath := strings.TrimSuffix(u.Path, "/")
	normalized := u.Scheme + "://" + u.Host + normalizedPath
	if exactURL {
		normalized += model.ExactUpstreamURLMarker
	}
	return normalized, nil
}

// validateChannelURLs 校验换行分隔的多URL字段，逐个验证并标准化
func validateChannelURLs(raw string) (string, error) {
	if !strings.Contains(raw, "\n") {
		return validateChannelBaseURL(raw)
	}
	lines := strings.Split(raw, "\n")
	var normalized []string
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		u, err := validateChannelBaseURL(line)
		if err != nil {
			return "", err
		}
		if _, exists := seen[u]; exists {
			continue
		}
		seen[u] = struct{}{}
		normalized = append(normalized, u)
	}
	if len(normalized) == 0 {
		return "", fmt.Errorf("url cannot be empty")
	}
	return strings.Join(normalized, "\n"), nil
}

// Validate 实现RequestValidator接口
// [FIX] P0-1: 添加白名单校验和标准化（Fail-Fast + 边界防御）
func (cr *ChannelRequest) Validate() error {
	// 必填字段校验（现有逻辑保留）
	if strings.TrimSpace(cr.Name) == "" {
		return fmt.Errorf("name cannot be empty")
	}
	apiKeys := cr.normalizeAPIKeys()
	if len(apiKeys) == 0 {
		return fmt.Errorf("api_key cannot be empty")
	}
	for i, key := range apiKeys {
		if strings.ContainsAny(key.APIKey, "\x00\r\n") {
			return fmt.Errorf("api_keys[%d].api_key contains illegal characters", i)
		}
		if len(key.Note) > maxAPIKeyNoteLength {
			return fmt.Errorf("api_keys[%d].note is too long (max %d bytes)", i, maxAPIKeyNoteLength)
		}
		if strings.Contains(key.Note, "\x00") {
			return fmt.Errorf("api_keys[%d].note contains illegal characters", i)
		}
	}
	cr.APIKeys = apiKeys
	cr.APIKey = strings.Join(apiKeyStrings(apiKeys), ",")
	if len(cr.Models) == 0 {
		return fmt.Errorf("models cannot be empty")
	}
	seenModels, err := validateChannelModelEntries(cr.Models)
	if err != nil {
		return err
	}

	cr.ScheduledCheckModel = strings.TrimSpace(cr.ScheduledCheckModel)
	if cr.ScheduledCheckModel != "" {
		if _, exists := seenModels[strings.ToLower(cr.ScheduledCheckModel)]; !exists {
			return fmt.Errorf("scheduled_check_model %q must exist in models", cr.ScheduledCheckModel)
		}
	}

	// URL 验证：支持换行分隔的多URL，逐个校验并标准化
	normalizedURL, err := validateChannelURLs(cr.URL)
	if err != nil {
		return err
	}
	cr.URL = normalizedURL

	// [FIX] channel_type 白名单校验 + 标准化
	// 设计：空值允许（使用默认值anthropic），非空值必须合法
	cr.ChannelType = strings.TrimSpace(cr.ChannelType)
	if cr.ChannelType != "" {
		// 先标准化（小写化）
		normalized := util.NormalizeChannelType(cr.ChannelType)
		// 再白名单校验
		if !util.IsValidChannelType(normalized) {
			return fmt.Errorf("invalid channel_type: %q (allowed: anthropic, openai, gemini, codex)", cr.ChannelType)
		}
		cr.ChannelType = normalized // 应用标准化结果
	}
	rawProtocolTransformMode := cr.ProtocolTransformMode
	cr.ProtocolTransformMode = model.NormalizeProtocolTransformMode(cr.ProtocolTransformMode)
	if cr.ProtocolTransformMode == "" {
		return fmt.Errorf("invalid protocol_transform_mode: %q (allowed: local, upstream)", rawProtocolTransformMode)
	}
	if model.HasExactUpstreamURLMarker(cr.URL) && cr.ProtocolTransformMode == model.ProtocolTransformModeUpstream {
		return fmt.Errorf("protocol_transform_mode upstream is not allowed when url uses exact upstream marker #")
	}
	if err := validateProtocolTransforms(cr.ChannelType, cr.ProtocolTransformMode, cr.ProtocolTransforms); err != nil {
		return err
	}
	cr.ProtocolTransforms = normalizeProtocolTransforms(cr.ChannelType, cr.ProtocolTransformMode, cr.ProtocolTransforms)
	cr.ProtocolCapabilities, err = normalizeProtocolCapabilities(
		cr.ChannelType,
		cr.ProtocolTransformMode,
		cr.ProtocolTransforms,
		cr.ProtocolCapabilities,
	)
	if err != nil {
		return err
	}

	// [FIX] key_strategy 白名单校验 + 标准化
	// 设计：空值允许（使用默认值sequential），非空值必须合法
	cr.KeyStrategy = strings.TrimSpace(cr.KeyStrategy)
	if cr.KeyStrategy != "" {
		// 先标准化（小写化）
		normalized := strings.ToLower(cr.KeyStrategy)
		// 再白名单校验
		if !model.IsValidKeyStrategy(normalized) {
			return fmt.Errorf("invalid key_strategy: %q (allowed: sequential, round_robin)", cr.KeyStrategy)
		}
		cr.KeyStrategy = normalized // 应用标准化结果
	}

	if err := validateCustomRequestRules(cr.CustomRequestRules); err != nil {
		return err
	}
	if cr.CustomRequestRules != nil && cr.CustomRequestRules.IsEmpty() {
		cr.CustomRequestRules = nil
	}

	cr.ProxyURL = strings.TrimSpace(cr.ProxyURL)
	if cr.ProxyURL != "" {
		pu, err := neturl.Parse(cr.ProxyURL)
		if err != nil || pu.Host == "" {
			return fmt.Errorf("invalid proxy_url: %q", cr.ProxyURL)
		}
		switch pu.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("invalid proxy_url scheme: %q (allowed: http, https, socks5, socks5h)", pu.Scheme)
		}
	}

	if cr.RPMLimit < 0 {
		return fmt.Errorf("rpm_limit must be >= 0 (got %d)", cr.RPMLimit)
	}
	if cr.MaxConcurrency < 0 {
		return fmt.Errorf("max_concurrency must be >= 0 (got %d)", cr.MaxConcurrency)
	}

	// CostMultiplier: 未传视为默认 1；0 表示免费渠道；负数拒绝
	if cr.CostMultiplier == 0 {
		// 0 是合法值（免费渠道），保持不变
	} else if cr.CostMultiplier < 0 {
		return fmt.Errorf("cost_multiplier must be >= 0 (got %v)", cr.CostMultiplier)
	}

	return nil
}

func validateChannelModelEntries(entries []model.ModelEntry) (map[string]int, error) {
	seenModels := make(map[string]int, len(entries))
	for i := range entries {
		if err := entries[i].Validate(); err != nil {
			return nil, fmt.Errorf("models[%d]: %w", i, err)
		}
		modelKey := strings.ToLower(entries[i].Model)
		if firstIdx, exists := seenModels[modelKey]; exists {
			return nil, fmt.Errorf("models[%d]: duplicate model %q (already defined at models[%d])", i, entries[i].Model, firstIdx)
		}
		seenModels[modelKey] = i
	}

	seenProtocolAliases := make(map[string]int)
	for i := range entries {
		if !entries[i].RedirectEnabled {
			continue
		}
		aliasCount := 0
		for protocolName, aliases := range entries[i].ProtocolAliases {
			for _, alias := range aliases {
				aliasCount++
				aliasModelKey := strings.ToLower(alias)
				if upstreamIdx, exists := seenModels[aliasModelKey]; exists && upstreamIdx != i {
					return nil, fmt.Errorf("models[%d]: public model %q for protocol %q conflicts with upstream model at models[%d]", i, alias, protocolName, upstreamIdx)
				}
				key := protocolName + "\x00" + aliasModelKey
				if firstIdx, exists := seenProtocolAliases[key]; exists {
					return nil, fmt.Errorf("models[%d]: duplicate public model %q for protocol %q (already defined at models[%d])", i, alias, protocolName, firstIdx)
				}
				seenProtocolAliases[key] = i
			}
		}
		if aliasCount == 0 {
			return nil, fmt.Errorf("models[%d]: redirect_enabled requires at least one protocol alias", i)
		}
	}
	return seenModels, nil
}

// ToConfig 转换为Config结构(不包含API Key,API Key单独处理)
func (cr *ChannelRequest) ToConfig() *model.Config {
	protocols := append([]string{util.NormalizeChannelType(cr.ChannelType)}, cr.ProtocolTransforms...)
	if protocols[0] == "" {
		protocols[0] = string(protocol.Anthropic)
	}
	normalizedModels, legacyRedirects := normalizeModelEntries(cr.Models, protocols)
	scheduledCheckModel := cr.ScheduledCheckModel
	if redirected, ok := legacyRedirects[scheduledCheckModel]; ok {
		scheduledCheckModel = redirected
	}

	return &model.Config{
		Name:                  strings.TrimSpace(cr.Name),
		ChannelType:           strings.TrimSpace(cr.ChannelType), // 传递渠道类型
		ProtocolTransformMode: cr.ProtocolTransformMode,
		ProtocolTransforms:    append([]string(nil), cr.ProtocolTransforms...),
		ProtocolCapabilities:  cr.ProtocolCapabilities,
		URL:                   strings.TrimSpace(cr.URL),
		Priority:              cr.Priority,
		RPMLimit:              cr.RPMLimit,
		MaxConcurrency:        cr.MaxConcurrency,
		ModelEntries:          normalizedModels,
		Enabled:               cr.Enabled,
		ScheduledCheckEnabled: cr.ScheduledCheckEnabled,
		ScheduledCheckModel:   scheduledCheckModel,
		DailyCostLimit:        cr.DailyCostLimit,
		CostMultiplier:        cr.CostMultiplier,
		CustomRequestRules:    cr.CustomRequestRules,
		ProxyURL:              cr.ProxyURL,
	}
}

type normalizedModelGroup struct {
	entry          model.ModelEntry
	legacyNames    []string
	legacyRedirect bool
}

func normalizeModelEntries(entries []model.ModelEntry, protocols []string) ([]model.ModelEntry, map[string]string) {
	groups := make([]normalizedModelGroup, 0, len(entries))
	groupIndex := make(map[string]int, len(entries))
	legacyRedirects := make(map[string]string)

	for _, submitted := range entries {
		isProtocolModel := submitted.RedirectEnabled || len(submitted.ProtocolAliases) > 0
		upstreamModel := submitted.Model
		legacyRedirect := !isProtocolModel && submitted.RedirectModel != "" && submitted.RedirectModel != submitted.Model
		if legacyRedirect {
			upstreamModel = submitted.RedirectModel
			legacyRedirects[submitted.Model] = upstreamModel
		}
		key := strings.ToLower(upstreamModel)
		idx, exists := groupIndex[key]
		if !exists {
			idx = len(groups)
			groupIndex[key] = idx
			groups = append(groups, normalizedModelGroup{entry: model.ModelEntry{
				Model:           upstreamModel,
				RedirectEnabled: submitted.RedirectEnabled,
				ProtocolAliases: make(map[string][]string),
			}})
		}
		group := &groups[idx]
		group.entry.RedirectEnabled = group.entry.RedirectEnabled || submitted.RedirectEnabled
		for protocolName, aliases := range submitted.ProtocolAliases {
			group.entry.ProtocolAliases[protocolName] = appendUniqueModelAliases(group.entry.ProtocolAliases[protocolName], aliases...)
		}
		if !isProtocolModel {
			group.legacyNames = appendUniqueModelAliases(group.legacyNames, submitted.Model)
			group.legacyRedirect = group.legacyRedirect || legacyRedirect
		}
	}

	normalized := make([]model.ModelEntry, 0, len(groups))
	for i := range groups {
		group := &groups[i]
		if group.legacyRedirect {
			group.entry.RedirectEnabled = true
			for _, protocolName := range protocols {
				protocolName = strings.ToLower(strings.TrimSpace(protocolName))
				if protocolName == "" {
					continue
				}
				group.entry.ProtocolAliases[protocolName] = appendUniqueModelAliases(
					group.entry.ProtocolAliases[protocolName], group.legacyNames...,
				)
			}
		}
		if len(group.entry.ProtocolAliases) == 0 {
			group.entry.ProtocolAliases = nil
		}
		normalized = append(normalized, group.entry)
	}
	return normalized, legacyRedirects
}

func appendUniqueModelAliases(existing []string, values ...string) []string {
	seen := make(map[string]struct{}, len(existing)+len(values))
	for _, value := range existing {
		seen[strings.ToLower(value)] = struct{}{}
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		existing = append(existing, value)
	}
	return existing
}

func normalizeProtocolCapabilities(
	channelType, transformMode string,
	transforms []string,
	raw map[string]map[string]bool,
) (map[string]map[string]bool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if channelType != util.ChannelTypeOpenAI ||
		transformMode != model.ProtocolTransformModeLocal ||
		!slices.Contains(transforms, string(protocol.Codex)) {
		return nil, fmt.Errorf("protocol_capabilities are only allowed for local Codex to OpenAI transforms")
	}
	allowed := make(map[string]struct{}, len(model.CodexToOpenAICapabilities))
	for _, capability := range model.CodexToOpenAICapabilities {
		allowed[capability] = struct{}{}
	}
	normalized := make(map[string]map[string]bool, 1)
	for protocolName, capabilities := range raw {
		protocolName = strings.TrimSpace(strings.ToLower(protocolName))
		if protocolName != string(protocol.Codex) {
			return nil, fmt.Errorf("unsupported protocol_capabilities protocol %q", protocolName)
		}
		values := make(map[string]bool, len(capabilities))
		for capability, enabled := range capabilities {
			capability = strings.TrimSpace(strings.ToLower(capability))
			if _, ok := allowed[capability]; !ok {
				return nil, fmt.Errorf("unsupported Codex to OpenAI capability %q", capability)
			}
			values[capability] = enabled
		}
		normalized[protocolName] = values
	}
	return normalized, nil
}

const (
	maxCustomRuleEntries = 32
	maxCustomRuleValue   = 8 * 1024
	maxCustomRuleName    = 256
)

// validateCustomRequestRules 校验渠道自定义请求规则；副作用：修剪名称/路径空白并丢弃 remove 规则的 value。
func validateCustomRequestRules(r *model.CustomRequestRules) error {
	if r == nil {
		return nil
	}
	if len(r.Headers) > maxCustomRuleEntries {
		return fmt.Errorf("custom_request_rules.headers: too many entries (max %d)", maxCustomRuleEntries)
	}
	if len(r.Body) > maxCustomRuleEntries {
		return fmt.Errorf("custom_request_rules.body: too many entries (max %d)", maxCustomRuleEntries)
	}

	for i := range r.Headers {
		h := &r.Headers[i]
		action := strings.ToLower(strings.TrimSpace(h.Action))
		if action != model.RuleActionRemove && action != model.RuleActionOverride && action != model.RuleActionAppend {
			return fmt.Errorf("custom_request_rules.headers[%d]: invalid action %q (allowed: remove, override, append)", i, h.Action)
		}
		h.Action = action

		name := strings.TrimSpace(h.Name)
		if name == "" {
			return fmt.Errorf("custom_request_rules.headers[%d]: name cannot be empty", i)
		}
		if len(name) > maxCustomRuleName {
			return fmt.Errorf("custom_request_rules.headers[%d]: name too long (max %d)", i, maxCustomRuleName)
		}
		if strings.ContainsAny(name, "\r\n\x00") {
			return fmt.Errorf("custom_request_rules.headers[%d]: name contains illegal characters", i)
		}
		h.Name = name

		// remove：value 为空=删整条；非空=按逗号 token 精确移除（与 override/append 同等做校验）
		if len(h.Value) > maxCustomRuleValue {
			return fmt.Errorf("custom_request_rules.headers[%d]: value too long (max %d bytes)", i, maxCustomRuleValue)
		}
		if strings.ContainsAny(h.Value, "\r\n\x00") {
			return fmt.Errorf("custom_request_rules.headers[%d]: value contains illegal characters", i)
		}
	}

	for i := range r.Body {
		b := &r.Body[i]
		action := strings.ToLower(strings.TrimSpace(b.Action))
		if action != model.RuleActionRemove && action != model.RuleActionOverride {
			return fmt.Errorf("custom_request_rules.body[%d]: invalid action %q (allowed: remove, override)", i, b.Action)
		}
		b.Action = action

		path := strings.TrimSpace(b.Path)
		if path == "" {
			return fmt.Errorf("custom_request_rules.body[%d]: path cannot be empty", i)
		}
		if len(path) > maxCustomRuleName {
			return fmt.Errorf("custom_request_rules.body[%d]: path too long (max %d)", i, maxCustomRuleName)
		}
		if !isValidCustomRulePath(path) {
			return fmt.Errorf("custom_request_rules.body[%d]: path contains illegal characters (allowed: letters, digits, _, -, .)", i)
		}
		b.Path = path

		if action == model.RuleActionRemove {
			b.Value = nil
			continue
		}
		if len(b.Value) == 0 {
			return fmt.Errorf("custom_request_rules.body[%d]: override requires value", i)
		}
		if len(b.Value) > maxCustomRuleValue {
			return fmt.Errorf("custom_request_rules.body[%d]: value too long (max %d bytes)", i, maxCustomRuleValue)
		}
		var parsed any
		if err := json.Unmarshal(b.Value, &parsed); err != nil {
			return fmt.Errorf("custom_request_rules.body[%d]: value is not valid JSON (%v)", i, err)
		}
	}
	return nil
}

// isValidCustomRulePath 允许字符：字母、数字、下划线、连字符、点。
func isValidCustomRulePath(p string) bool {
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

func validateProtocolTransforms(channelType string, protocolTransformMode string, transforms []string) error {
	base := protocol.Protocol(util.NormalizeChannelType(channelType))
	mode := model.NormalizeProtocolTransformMode(protocolTransformMode)
	if mode == "" {
		mode = model.ProtocolTransformModeUpstream
	}
	seen := make(map[string]int, len(transforms))
	for i, rawProtocol := range transforms {
		rawProtocol = strings.TrimSpace(rawProtocol)
		if rawProtocol == "" {
			return fmt.Errorf("protocol_transforms[%d]: cannot be empty", i)
		}

		normalized := util.NormalizeChannelType(rawProtocol)
		if !util.IsValidChannelType(normalized) {
			return fmt.Errorf("protocol_transforms[%d]: invalid protocol %q (allowed: anthropic, openai, gemini, codex)", i, rawProtocol)
		}
		if normalized == string(base) {
			return fmt.Errorf("protocol_transforms[%d]: %q duplicates channel_type %q", i, normalized, base)
		}
		if mode == model.ProtocolTransformModeLocal && !protocol.SupportsTransform(protocol.Protocol(normalized), base) {
			return fmt.Errorf("protocol_transforms[%d]: unsupported protocol transform %s -> %s", i, normalized, base)
		}
		if firstIdx, exists := seen[normalized]; exists {
			return fmt.Errorf("protocol_transforms[%d]: duplicate protocol %q (already defined at protocol_transforms[%d])", i, normalized, firstIdx)
		}
		seen[normalized] = i
	}
	return nil
}

func normalizeProtocolTransforms(channelType string, protocolTransformMode string, transforms []string) []string {
	base := protocol.Protocol(util.NormalizeChannelType(channelType))
	mode := model.NormalizeProtocolTransformMode(protocolTransformMode)
	if mode == "" {
		mode = model.ProtocolTransformModeUpstream
	}
	seen := make(map[string]struct{}, len(transforms))
	normalized := make([]string, 0, len(transforms))
	for _, protocolName := range transforms {
		protocolName = strings.TrimSpace(protocolName)
		if protocolName == "" {
			continue
		}
		normalizedProtocol := util.NormalizeChannelType(protocolName)
		if !util.IsValidChannelType(normalizedProtocol) {
			continue
		}
		if normalizedProtocol == string(base) {
			continue
		}
		if mode == model.ProtocolTransformModeLocal && !protocol.SupportsTransform(protocol.Protocol(normalizedProtocol), base) {
			continue
		}
		if _, ok := seen[normalizedProtocol]; ok {
			continue
		}
		seen[normalizedProtocol] = struct{}{}
		normalized = append(normalized, normalizedProtocol)
	}
	slices.Sort(normalized)
	return normalized
}

// KeyCooldownInfo Key级别冷却信息
type KeyCooldownInfo struct {
	KeyIndex            int        `json:"key_index"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	CooldownRemainingMS int64      `json:"cooldown_remaining_ms,omitempty"`
}

// ChannelWithCooldown 带冷却状态的渠道响应结构
type ChannelWithCooldown struct {
	*model.Config
	KeyStrategy         string            `json:"key_strategy,omitempty"` // [INFO] 修复 (2025-10-11): 添加key_strategy字段
	CooldownUntil       *time.Time        `json:"cooldown_until,omitempty"`
	CooldownRemainingMS int64             `json:"cooldown_remaining_ms,omitempty"`
	KeyCooldowns        []KeyCooldownInfo `json:"key_cooldowns,omitempty"`
	EffectivePriority   *float64          `json:"effective_priority,omitempty"` // 健康度模式下的有效优先级
	SuccessRate         *float64          `json:"success_rate,omitempty"`       // 成功率(0-1)
}

// ChannelImportSummary 导入结果统计
type ChannelImportSummary struct {
	Created   int      `json:"created"`
	Updated   int      `json:"updated"`
	Skipped   int      `json:"skipped"`
	Processed int      `json:"processed"`
	Errors    []string `json:"errors,omitempty"`
}

// CooldownRequest 冷却设置请求
type CooldownRequest struct {
	DurationMs int64 `json:"duration_ms" binding:"required,min=1000"` // 最少1秒
}

// SettingUpdateRequest 系统配置更新请求
type SettingUpdateRequest struct {
	Value string `json:"value" binding:"required"`
}

// CheckDuplicateRequest 渠道重复检测请求
type CheckDuplicateRequest struct {
	ChannelType string   `json:"channel_type" binding:"required"`
	URLs        []string `json:"urls"         binding:"required,min=1"`
}

// Validate 实现 RequestValidator 接口，无额外业务约束
func (r *CheckDuplicateRequest) Validate() error { return nil }

// DuplicateChannelInfo 重复渠道信息
type DuplicateChannelInfo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	ChannelType string `json:"channel_type"`
	URL         string `json:"url"`
}

// CheckDuplicateResponse 重复检测响应
type CheckDuplicateResponse struct {
	Duplicates []DuplicateChannelInfo `json:"duplicates"`
}
