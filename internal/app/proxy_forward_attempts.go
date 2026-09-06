package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
)

func (s *Server) forwardAttempt(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	selectedKey string,
	reqCtx *proxyRequestContext,
	actualModel string, // [INFO] 重定向后的实际模型名称
	bodyToSend []byte,
	requestPath string, // [FIX] 2026-01: 可能经过模型名替换的请求路径
	baseURL string, // 显式传入的URL（多URL场景）
	w http.ResponseWriter,
	deferChannelCooldown bool, // 多URL场景下，非最后一个URL不应触发渠道级冷却
) (result *proxyResult, action cooldown.Action, attemptErr error) {
	// 记录渠道尝试开始时间（用于日志记录，每次渠道/Key切换时更新）
	reqCtx.attemptStartTime = time.Now()
	reqCtx.baseURL = baseURL

	// 转发请求（传递实际的API Key字符串和观测回调）
	// [FIX] 2026-01: 使用传入的 requestPath（可能已替换模型名）而非 reqCtx.requestPath
	upstreamProtocol := protocol.Protocol(cfg.ResolveUpstreamProtocol(string(reqCtx.clientProtocol)))
	reqCtx.upstreamProtocol = upstreamProtocol
	bodyToSend = applyCodexToOpenAICapabilities(cfg, reqCtx.clientProtocol, upstreamProtocol, requestPath, bodyToSend)
	bodyToSend = prepareCodexResponsesBodyForUpstream(cfg, upstreamProtocol, requestPath, bodyToSend)
	plan, err := protocol.BuildTransformPlan(
		reqCtx.clientProtocol,
		upstreamProtocol,
		reqCtx.requestPath,
		requestPath,
		reqCtx.body,
		bodyToSend,
		reqCtx.originalModel,
		actualModel,
		reqCtx.isStreaming,
	)
	if err != nil {
		channelID := cfg.ID
		return &proxyResult{
			status:     http.StatusInternalServerError,
			body:       []byte(err.Error()),
			channelID:  &channelID,
			succeeded:  false,
			nextAction: cooldown.ActionRetryChannel,
		}, cooldown.ActionRetryChannel, nil
	}

	attemptCtx, cancelAttempt := context.WithCancelCause(ctx)
	attemptID := int64(0)
	if reqCtx.activeReqID > 0 && s.activeRequests != nil {
		attemptID, _ = s.activeRequests.BeginAttempt(reqCtx.activeReqID, cancelAttempt)
	}
	defer func() {
		// RequestChannelSkip 与本函数收尾之间可能并发发生。只要管理端已经
		// 成功接受了跳过请求，就必须覆盖尚未返回给外层的失败结果，避免当前
		// 渠道继续尝试下一个 URL/Key。
		if isManualChannelSkip(attemptCtx) {
			result = manualChannelSkipResult(cfg)
			action = cooldown.ActionRetryChannel
			attemptErr = nil
		}
		if attemptID > 0 {
			s.activeRequests.EndAttempt(reqCtx.activeReqID, attemptID)
		}
		cancelAttempt(nil)
	}()

	attemptObserver := reqCtx.observer
	if attemptID > 0 {
		observerCopy := ForwardObserver{}
		if reqCtx.observer != nil {
			observerCopy = *reqCtx.observer
		}
		observerCopy.BeforeResponseCommit = func() error {
			return s.activeRequests.PrepareResponseCommit(reqCtx.activeReqID, attemptID)
		}
		attemptObserver = &observerCopy
	}

	res, duration, err := s.forwardOnceAsync(attemptCtx, cfg, selectedKey, reqCtx.requestMethod,
		plan, reqCtx.header, reqCtx.rawQuery, baseURL, w, attemptObserver)
	if isManualChannelSkip(attemptCtx) {
		return manualChannelSkipResult(cfg), cooldown.ActionRetryChannel, nil
	}

	// 传递 debug 数据到 proxyRequestContext（用于日志记录）
	if res != nil && res.DebugData != nil {
		reqCtx.debugData = res.DebugData
	}

	forceReturnClient := false
	retryStrategies := make([]string, 0, 2)
	for {
		if isManualChannelSkip(attemptCtx) {
			return manualChannelSkipResult(cfg), cooldown.ActionRetryChannel, nil
		}
		retryBody, retryStrategy, ok := codexRetryBodyFor400(upstreamProtocol, cfg, plan, res)
		if !ok || hasRetryStrategy(retryStrategies, retryStrategy) {
			break
		}
		retryStrategies = append(retryStrategies, retryStrategy)
		retryPlan := plan
		retryPlan.TranslatedBody = retryBody
		res, duration, err = s.forwardOnceAsync(attemptCtx, cfg, selectedKey, reqCtx.requestMethod,
			retryPlan, reqCtx.header, reqCtx.rawQuery, baseURL, w, attemptObserver)
		if isManualChannelSkip(attemptCtx) {
			return manualChannelSkipResult(cfg), cooldown.ActionRetryChannel, nil
		}
		if res != nil && res.DebugData != nil {
			reqCtx.debugData = res.DebugData
		}
		if err == nil && res != nil && res.Status >= 200 && res.Status < 300 {
			res.RetryStrategy = strings.Join(retryStrategies, ",")
			break
		}
		forceReturnClient = true
		plan = retryPlan
		if err != nil || res == nil {
			break
		}
	}
	if isManualChannelSkip(attemptCtx) {
		return manualChannelSkipResult(cfg), cooldown.ActionRetryChannel, nil
	}

	// 处理网络错误或异常响应（如空响应）
	// [INFO] 修复：handleResponse可能返回err即使StatusCode=200（例如Content-Length=0）
	// [FIX] 2025-12: 传递 res 和 reqCtx，用于保留 499 场景下已消耗的 token 统计
	if err != nil {
		if isManualChannelSkip(attemptCtx) || errors.Is(err, errManualChannelSkip) {
			return manualChannelSkipResult(cfg), cooldown.ActionRetryChannel, nil
		}
		if errors.Is(err, ErrChannelRPMExceeded) || errors.Is(err, ErrChannelConcurrencyExceeded) {
			return nil, cooldown.ActionRetryChannel, err
		}
		result, action := s.handleNetworkError(
			ctx, cfg, keyIndex, actualModel, selectedKey, reqCtx.tokenID, reqCtx.clientIP,
			duration, err, res, reqCtx, deferChannelCooldown,
		)
		return result, action, nil
	}

	// 处理成功响应（仅当err==nil且状态码2xx时）
	if res.Status >= 200 && res.Status < 300 {
		if result, action, handled := s.handleSuccessfulForwardAnomaly(
			ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx, deferChannelCooldown,
		); handled {
			return result, action, nil
		}

		result, action := s.handleProxySuccess(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx)
		return result, action, nil
	}

	// 处理错误响应
	result, action = s.handleProxyErrorResponse(
		ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx, deferChannelCooldown, forceReturnClient,
	)
	return result, action, nil
}

func shouldRetryCodexInvalidEncryptedContent(upstreamProtocol protocol.Protocol, plan protocol.TransformPlan, res *fwResult) bool {
	return upstreamProtocol == protocol.Codex &&
		!plan.NeedsTransform &&
		res != nil &&
		res.Status == http.StatusBadRequest &&
		isInvalidEncryptedContentError(res.Body)
}

func isInvalidEncryptedContentError(body []byte) bool {
	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		return false
	}

	code := stringMapValue(payload, "code")
	message := firstStringMapValue(payload, "message", "error")
	if errorObject, ok := payload["error"].(map[string]any); ok {
		if nestedCode := stringMapValue(errorObject, "code"); nestedCode != "" {
			code = nestedCode
		}
		if nestedMessage := stringMapValue(errorObject, "message"); nestedMessage != "" {
			message = nestedMessage
		}
	}

	code = strings.ToLower(code)
	if code == "invalid_encrypted_content" {
		return true
	}
	message = strings.ToLower(message)
	if strings.Contains(message, "invalid_encrypted_content") {
		return true
	}
	normalizedMessage := strings.NewReplacer("_", " ", "-", " ").Replace(message)
	return strings.Contains(normalizedMessage, "encrypted content") &&
		(strings.Contains(normalizedMessage, "could not be verified") ||
			strings.Contains(normalizedMessage, "could not be decrypted") ||
			strings.Contains(normalizedMessage, "could not decrypt") ||
			strings.Contains(normalizedMessage, "could not be parsed") ||
			strings.Contains(normalizedMessage, "cannot decrypt") ||
			strings.Contains(normalizedMessage, "failed to decrypt"))
}

func shouldRetryAnyrouterCodexInvalidResponsesRequest(upstreamProtocol protocol.Protocol, cfg *model.Config, res *fwResult) bool {
	return upstreamProtocol == protocol.Codex &&
		cfg != nil &&
		strings.Contains(strings.ToLower(cfg.Name), "anyrouter") &&
		res != nil &&
		res.Status == http.StatusBadRequest &&
		isInvalidResponsesRequestError(res.Body)
}

func isInvalidResponsesRequestError(body []byte) bool {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := sonic.Unmarshal(body, &payload); err != nil {
		return false
	}
	code := strings.ToLower(payload.Error.Code)
	if code == "invalid_responses_request" {
		return true
	}
	return strings.Contains(strings.ToLower(payload.Error.Message), "invalid_responses_request")
}

func codexRetryBodyFor400(
	upstreamProtocol protocol.Protocol,
	cfg *model.Config,
	plan protocol.TransformPlan,
	res *fwResult,
) ([]byte, string, bool) {
	if shouldRetryCodexInvalidEncryptedContent(upstreamProtocol, plan, res) {
		if retryBody, ok := codexBodyWithoutEncryptedInputItems(plan.TranslatedBody); ok {
			return retryBody, "strip_codex_encrypted_input", true
		}
	}
	if shouldRetryAnyrouterCodexInvalidResponsesRequest(upstreamProtocol, cfg, res) {
		if retryBody, ok := codexBodyWithoutEncryptedContent(plan.TranslatedBody); ok {
			return retryBody, "strip_codex_encrypted_content", true
		}
	}
	if shouldRetryCodexUnsupportedThinking(upstreamProtocol, res) {
		if retryBody, ok := codexBodyWithoutThinking(plan.TranslatedBody); ok {
			return retryBody, "strip_codex_thinking", true
		}
	}
	return nil, "", false
}

func hasRetryStrategy(strategies []string, strategy string) bool {
	for _, existing := range strategies {
		if existing == strategy {
			return true
		}
	}
	return false
}

func shouldRetryCodexUnsupportedThinking(upstreamProtocol protocol.Protocol, res *fwResult) bool {
	return upstreamProtocol == protocol.Codex &&
		res != nil &&
		res.Status == http.StatusBadRequest &&
		isUnsupportedThinkingError(res.Body)
}

func isUnsupportedThinkingError(body []byte) bool {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := sonic.Unmarshal(body, &payload); err != nil {
		return false
	}

	code := strings.ToLower(strings.TrimSpace(payload.Error.Code))
	message := strings.ToLower(strings.TrimSpace(payload.Error.Message))
	param := strings.ToLower(strings.TrimSpace(payload.Error.Param))
	typ := strings.ToLower(strings.TrimSpace(payload.Error.Type))

	mentionsThinking := strings.Contains(message, "reasoning") ||
		strings.Contains(message, "thinking") ||
		strings.Contains(param, "reasoning") ||
		strings.Contains(param, "thinking")
	if !mentionsThinking {
		return false
	}

	switch code {
	case "unsupported_parameter", "invalid_request_error", "invalid_responses_request", "unknown_parameter":
		return true
	}
	if typ == "invalid_request_error" {
		return true
	}
	return strings.Contains(message, "unsupported") ||
		strings.Contains(message, "unknown parameter") ||
		strings.Contains(message, "not support") ||
		strings.Contains(message, "does not support") ||
		strings.Contains(message, "invalid")
}

func codexBodyWithoutEncryptedInputItems(body []byte) ([]byte, bool) {
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return nil, false
	}
	input, ok := root["input"].([]any)
	if !ok {
		return nil, false
	}

	filtered := make([]any, 0, len(input))
	removed := false
	for _, item := range input {
		obj, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		typ, _ := obj["type"].(string)
		_, hasEncryptedContent := obj["encrypted_content"]
		if typ == "reasoning" || hasEncryptedContent {
			removed = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !removed {
		return nil, false
	}

	root["input"] = filtered
	retryBody, err := sonic.Marshal(root)
	if err != nil {
		return nil, false
	}
	return retryBody, true
}

func codexBodyWithoutThinking(body []byte) ([]byte, bool) {
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return nil, false
	}

	removed := false
	if _, ok := root["reasoning"]; ok {
		delete(root, "reasoning")
		removed = true
	}
	if filterCodexThinkingIncludes(root) {
		removed = true
	}
	if input, ok := root["input"].([]any); ok {
		filtered := make([]any, 0, len(input))
		for _, item := range input {
			obj, ok := item.(map[string]any)
			if !ok {
				filtered = append(filtered, item)
				continue
			}
			typ, _ := obj["type"].(string)
			if typ == "reasoning" {
				removed = true
				continue
			}
			filtered = append(filtered, item)
		}
		root["input"] = filtered
	}
	if !removed {
		return nil, false
	}

	retryBody, err := sonic.Marshal(root)
	if err != nil {
		return nil, false
	}
	return retryBody, true
}

func filterCodexThinkingIncludes(root map[string]any) bool {
	include, ok := root["include"].([]any)
	if !ok {
		return false
	}
	filtered := make([]any, 0, len(include))
	removed := false
	for _, item := range include {
		value, ok := item.(string)
		if ok && strings.HasPrefix(value, "reasoning.") {
			removed = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !removed {
		return false
	}
	if len(filtered) == 0 {
		delete(root, "include")
		return true
	}
	root["include"] = filtered
	return true
}

func prepareCodexResponsesBodyForUpstream(cfg *model.Config, upstreamProtocol protocol.Protocol, requestPath string, body []byte) []byte {
	if upstreamProtocol != protocol.Codex ||
		protocol.DetectRequestFamily(requestPath) != protocol.RequestFamilyResponses {
		return body
	}
	if normalized, ok := normalizeCodexToolSearchInputItems(body); ok {
		body = normalized
	}
	if isAnyrouterChannel(cfg) {
		if stripped, ok := codexBodyWithoutToolSearchOnlyInputItems(body); ok {
			body = stripped
		}
	}
	return body
}

func applyCodexToOpenAICapabilities(
	cfg *model.Config,
	clientProtocol, upstreamProtocol protocol.Protocol,
	requestPath string,
	body []byte,
) []byte {
	if cfg == nil || clientProtocol != protocol.Codex || upstreamProtocol != protocol.OpenAI ||
		cfg.GetProtocolTransformMode() != model.ProtocolTransformModeLocal ||
		protocol.DetectRequestFamily(requestPath) != protocol.RequestFamilyResponses {
		return body
	}
	if !cfg.ProtocolCapabilityEnabled(string(protocol.Codex), model.ProtocolCapabilityHostedWebSearch) {
		if stripped, ok := codexBodyWithoutHostedWebSearch(body); ok {
			body = stripped
		}
	}
	functionTools := cfg.ProtocolCapabilityEnabled(string(protocol.Codex), model.ProtocolCapabilityFunctionTools)
	toolSearch := cfg.ProtocolCapabilityEnabled(string(protocol.Codex), model.ProtocolCapabilityToolSearch)
	if !functionTools || !toolSearch {
		if stripped, ok := codexBodyWithoutDisabledToolCapabilities(body, functionTools, toolSearch); ok {
			body = stripped
		}
	}
	if !cfg.ProtocolCapabilityEnabled(string(protocol.Codex), model.ProtocolCapabilityReasoning) {
		if stripped, ok := codexBodyWithoutThinking(body); ok {
			body = stripped
		}
	}
	if !cfg.ProtocolCapabilityEnabled(string(protocol.Codex), model.ProtocolCapabilityPromptCache) {
		if stripped, ok := codexBodyWithoutPromptCache(body); ok {
			body = stripped
		}
	}
	return body
}

func codexBodyWithoutDisabledToolCapabilities(body []byte, functionTools, toolSearch bool) ([]byte, bool) {
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return nil, false
	}
	changed := false
	shouldDropType := func(typ string) bool {
		typ = strings.ToLower(strings.TrimSpace(typ))
		if !functionTools {
			switch typ {
			case "function", "custom", "namespace", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
				return true
			}
		}
		if !toolSearch {
			switch typ {
			case "tool_search", "tool_search_call", "tool_search_output", "additional_tools":
				return true
			}
		}
		return false
	}
	for _, field := range []string{"tools", "input"} {
		items, ok := root[field].([]any)
		if !ok {
			continue
		}
		filtered := make([]any, 0, len(items))
		for _, item := range items {
			obj, ok := item.(map[string]any)
			if ok && shouldDropType(stringMapValue(obj, "type")) {
				changed = true
				continue
			}
			filtered = append(filtered, item)
		}
		if len(filtered) == 0 {
			delete(root, field)
		} else {
			root[field] = filtered
		}
	}
	if choice, ok := root["tool_choice"].(map[string]any); ok && shouldDropType(stringMapValue(choice, "type")) {
		delete(root, "tool_choice")
		changed = true
	}
	if !changed {
		return nil, false
	}
	out, err := sonic.Marshal(root)
	return out, err == nil
}

func codexBodyWithoutPromptCache(body []byte) ([]byte, bool) {
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return nil, false
	}
	if _, ok := root["prompt_cache_key"]; !ok {
		return nil, false
	}
	delete(root, "prompt_cache_key")
	out, err := sonic.Marshal(root)
	return out, err == nil
}

func isHostedWebSearchToolType(typ string) bool {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "web_search", "web_search_preview":
		return true
	default:
		return false
	}
}

func codexBodyWithoutHostedWebSearch(body []byte) ([]byte, bool) {
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return nil, false
	}

	changed := false

	if tools, ok := root["tools"].([]any); ok {
		filtered := make([]any, 0, len(tools))
		for _, item := range tools {
			obj, ok := item.(map[string]any)
			if !ok {
				filtered = append(filtered, item)
				continue
			}
			typ, _ := obj["type"].(string)
			if isHostedWebSearchToolType(typ) {
				changed = true
				continue
			}
			filtered = append(filtered, item)
		}
		if changed {
			if len(filtered) == 0 {
				delete(root, "tools")
			} else {
				root["tools"] = filtered
			}
		}
	}

	switch choice := root["tool_choice"].(type) {
	case string:
		if isHostedWebSearchToolType(choice) {
			delete(root, "tool_choice")
			changed = true
		}
	case map[string]any:
		typ, _ := choice["type"].(string)
		if isHostedWebSearchToolType(typ) {
			delete(root, "tool_choice")
			changed = true
		}
	}

	if input, ok := root["input"].([]any); ok {
		filtered := make([]any, 0, len(input))
		removedInput := false
		for _, item := range input {
			obj, ok := item.(map[string]any)
			if !ok {
				filtered = append(filtered, item)
				continue
			}
			typ, _ := obj["type"].(string)
			// Drop empty/fake prior hosted search turns so the model stops relying on them.
			if typ == "web_search_call" || strings.HasPrefix(typ, "web_search_") {
				removedInput = true
				continue
			}
			filtered = append(filtered, item)
		}
		if removedInput {
			root["input"] = filtered
			changed = true
		}
	}

	if !changed {
		return nil, false
	}
	out, err := sonic.Marshal(root)
	if err != nil {
		return nil, false
	}
	return out, true
}

func normalizeCodexToolSearchInputItems(body []byte) ([]byte, bool) {
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return nil, false
	}
	input, ok := root["input"].([]any)
	if !ok {
		return nil, false
	}

	changed := false
	filtered := make([]any, 0, len(input))
	for _, item := range input {
		obj, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		typ, _ := obj["type"].(string)
		if !strings.HasPrefix(typ, "tool_search_") {
			filtered = append(filtered, item)
			continue
		}
		rawArgs, hasArgs := obj["arguments"]
		if !hasArgs {
			filtered = append(filtered, item)
			continue
		}
		if _, ok := rawArgs.(map[string]any); ok {
			filtered = append(filtered, item)
			continue
		}
		argsString, ok := rawArgs.(string)
		if !ok {
			changed = true
			continue
		}

		var decoded any
		if err := sonic.Unmarshal([]byte(argsString), &decoded); err != nil {
			changed = true
			continue
		}
		argsObject, ok := decoded.(map[string]any)
		if !ok {
			changed = true
			continue
		}
		obj["arguments"] = argsObject
		changed = true
		filtered = append(filtered, item)
	}
	if !changed {
		return nil, false
	}

	root["input"] = filtered
	normalized, err := sonic.Marshal(root)
	if err != nil {
		return nil, false
	}
	return normalized, true
}

func codexBodyWithoutToolSearchOnlyInputItems(body []byte) ([]byte, bool) {
	return codexBodyWithoutInputItems(body, func(typ string) bool {
		return strings.HasPrefix(typ, "tool_search_")
	})
}

func codexBodyWithoutInputItems(body []byte, shouldDrop func(string) bool) ([]byte, bool) {
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return nil, false
	}

	input, ok := root["input"].([]any)
	if !ok {
		return nil, false
	}

	filtered := make([]any, 0, len(input))
	removed := false
	for _, item := range input {
		obj, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		typ, _ := obj["type"].(string)
		if shouldDrop(typ) {
			removed = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !removed {
		return nil, false
	}

	root["input"] = filtered
	retryBody, err := sonic.Marshal(root)
	if err != nil {
		return nil, false
	}
	return retryBody, true
}

func codexBodyWithoutEncryptedContent(body []byte) ([]byte, bool) {
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		return nil, false
	}

	removed := removeEncryptedContentFields(root)
	if !removed {
		return nil, false
	}

	retryBody, err := sonic.Marshal(root)
	if err != nil {
		return nil, false
	}
	return retryBody, true
}

func removeEncryptedContentFields(value any) bool {
	removed := false
	switch v := value.(type) {
	case map[string]any:
		if _, ok := v["encrypted_content"]; ok {
			delete(v, "encrypted_content")
			removed = true
		}
		for _, child := range v {
			if removeEncryptedContentFields(child) {
				removed = true
			}
		}
	case []any:
		for _, child := range v {
			if removeEncryptedContentFields(child) {
				removed = true
			}
		}
	}
	return removed
}

// ============================================================================
// 渠道内Key重试
// ============================================================================

// tryChannelWithKeys 在单个渠道内尝试多个Key（Key级重试）
// 从proxy.go提取，遵循SRP原则
// buildCtxDoneResult 构造 ctx 取消/超时时的 proxyResult，统一 fail-fast 路径。
func buildCtxDoneResult(cfg *model.Config, ctxErr error) *proxyResult {
	status := util.StatusClientClosedRequest
	isClientCanceled := errors.Is(ctxErr, context.Canceled)
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	return &proxyResult{
		status:           status,
		body:             []byte(`{"error":"` + ctxErr.Error() + `"}`),
		channelID:        &cfg.ID,
		succeeded:        false,
		isClientCanceled: isClientCanceled,
		nextAction:       cooldown.ActionReturnClient,
	}
}

// selectKeyWithFallback 在 triedKeys 之外选 Key：先 SelectAvailableKey，
// 启用 cooldown fallback 时再 SelectCooldownFallbackKey；全部失败包装 ErrAllKeysUnavailable。
func (s *Server) selectKeyWithFallback(cfg *model.Config, apiKeys []*model.APIKey, triedKeys map[int]bool) (int, string, error) {
	keyIndex, selectedKey, selectErr := s.keySelector.SelectAvailableKey(cfg.ID, apiKeys, triedKeys)
	if selectErr != nil && cfg.CooldownFallback {
		keyIndex, selectedKey, selectErr = s.keySelector.SelectCooldownFallbackKey(cfg.ID, apiKeys, triedKeys)
	}
	if selectErr != nil {
		return 0, "", fmt.Errorf("%w: %v", ErrAllKeysUnavailable, selectErr)
	}
	return keyIndex, selectedKey, nil
}

// recordSuccessTTFBToSelector 在多URL场景的2xx响应里把TTFB回报给URLSelector，
// 单URL/非2xx/无延迟数据直接跳过。优先用 firstByteTime，缺失时回退到 duration。
func recordSuccessTTFBToSelector(selector *URLSelector, channelID int64, urlsCount int, urlStr string, result *proxyResult) {
	if urlsCount <= 1 || selector == nil || result == nil {
		return
	}
	if result.status < 200 || result.status >= 300 {
		return
	}
	ttfb := time.Duration(result.firstByteTime * float64(time.Second))
	if ttfb <= 0 {
		ttfb = time.Duration(result.duration * float64(time.Second))
	}
	if ttfb > 0 {
		selector.RecordLatency(channelID, urlStr, ttfb)
	}
}

// attemptKeyAcrossURLs 在选定 Key 上按 URL 顺序尝试上游：
//   - immediate != nil 表示调用方需立即 `return immediate, nil`（成功 / ActionReturnClient / ctx 取消）
//   - immediate == nil 时 urlLastFailure 给 Key 重试循环用于决定 continue/break
//
// 多URL场景下：失败URL会被 selector 冷却；明确 5xx（除 598 首字节超时）会立即跳出 URL 循环切换渠道，
// 并在该URL处于 deferChannelCooldown 时补做一次渠道级冷却。
func (s *Server) attemptKeyAcrossURLs(
	ctx context.Context,
	cfg *model.Config,
	urls []string,
	selector *URLSelector,
	keyIndex int,
	selectedKey string,
	reqCtx *proxyRequestContext,
	actualModel string,
	bodyToSend []byte,
	requestPath string,
	w http.ResponseWriter,
) (immediate *proxyResult, urlLastFailure *proxyResult, err error) {
	sortedURLs := orderURLsWithSelector(selector, cfg.ID, urls)
	urlsCount := len(urls)
	for urlIdx, urlEntry := range sortedURLs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return buildCtxDoneResult(cfg, ctxErr), nil, nil
		}
		if urlIdx > 0 {
			enabled, err := s.isChannelEnabledForAttempt(ctx, cfg)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return buildCtxDoneResult(cfg, ctxErr), nil, nil
				}
				log.Printf("[WARN] 检查渠道 %s (ID=%d) 启用状态失败，停止后续URL尝试: %v", cfg.Name, cfg.ID, err)
				return channelDisabledResult(cfg, urlLastFailure), nil, nil
			}
			if !enabled {
				return channelDisabledResult(cfg, urlLastFailure), nil, nil
			}
		}

		// 更新活跃请求的当前URL（用于前端显示）
		if reqCtx.activeReqID > 0 && s.activeRequests != nil {
			s.activeRequests.SetBaseURL(reqCtx.activeReqID, urlEntry.url)
		}

		shouldDeferChannelCooldown := urlsCount > 1 && urlIdx < len(sortedURLs)-1
		result, nextAction, attemptErr := s.forwardAttempt(
			ctx, cfg, keyIndex, selectedKey, reqCtx, actualModel, bodyToSend, requestPath, urlEntry.url, w, shouldDeferChannelCooldown)
		if attemptErr != nil {
			return nil, nil, attemptErr
		}
		if result != nil && result.manualChannelSkip {
			return result, nil, nil
		}

		if result != nil && result.succeeded {
			// 成功：记录TTFB到URLSelector（仅多URL场景）
			if !reqCtx.isolatedSubrequest {
				recordSuccessTTFBToSelector(selector, cfg.ID, urlsCount, urlEntry.url, result)
			}
			return result, nil, nil
		}

		if result != nil {
			urlLastFailure = result
		}

		// Key级错误：换URL无意义，跳出URL循环
		if nextAction == cooldown.ActionRetryKey {
			break
		}
		// 容量/限额类错误（413 ActionSkipChannel）：跳过当前渠道，轮到下一个候选渠道（不冷却）
		if nextAction == cooldown.ActionSkipChannel {
			break
		}
		// 客户端错误：直接返回
		if nextAction == cooldown.ActionReturnClient {
			return urlLastFailure, nil, nil
		}
		// 渠道级错误 (ActionRetryChannel) 或网络错误：
		// 在多URL场景下，默认先尝试下一个URL
		if urlsCount > 1 {
			if selector != nil && !reqCtx.isolatedSubrequest {
				selector.CooldownURL(cfg.ID, urlEntry.url)
			}

			// 新策略：上游明确返回 5xx（598 首字节超时除外）时，直接切换下一个渠道。
			// 该分支命中时，当前URL若使用了 deferChannelCooldown，需要补做一次渠道级冷却写入。
			if shouldSwitchChannelImmediatelyOnHTTP5xx(result) {
				if shouldDeferChannelCooldown && result != nil && !reqCtx.isolatedSubrequest {
					input := httpErrorInputFromParts(cfg.ID, keyIndex, result.status, result.body, result.header)
					s.applyCooldownDecision(ctx, cfg, input)
				}
				break
			}
			continue // 下一个URL
		}
		// 单URL：保持原有行为
		break
	}
	return nil, urlLastFailure, nil
}

func (s *Server) tryChannelWithKeys(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext, w http.ResponseWriter) (*proxyResult, error) {
	reqCtx.channelStartTime = time.Now()
	// 每个渠道进入时重置：baseURL 只在 forwardAttempt 内赋值，
	// 不清空会让「未发起转发就失败」的渠道沿用上一个渠道的 URL。
	reqCtx.baseURL = ""

	// Fail-fast：ctx 已结束（客户端断开/请求超时）时不要再做任何 I/O（查库、选Key、发请求）。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return buildCtxDoneResult(cfg, ctxErr), nil
	}
	if reqCtx != nil && reqCtx.isolatedSubrequest {
		enabled, err := s.isChannelEnabledForAttempt(ctx, cfg)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return buildCtxDoneResult(cfg, ctxErr), nil
			}
			log.Printf("[WARN] 检查渠道 %s (ID=%d) 启用状态失败，跳过视觉辅助尝试: %v", cfg.Name, cfg.ID, err)
			return channelDisabledResult(cfg, nil), nil
		}
		if !enabled {
			return channelDisabledResult(cfg, nil), nil
		}
	}

	// 查询渠道的API Keys（缓存优先，缓存不可用自动降级到数据库查询）
	apiKeys, err := s.getAPIKeys(ctx, cfg.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to get API keys: %w", err)
	}

	// 计算实际重试次数
	actualKeyCount := len(apiKeys)
	if actualKeyCount == 0 {
		return nil, fmt.Errorf("no API keys configured for channel %d", cfg.ID)
	}

	maxKeyRetries := min(s.maxKeyRetries, actualKeyCount)

	triedKeys := make(map[int]bool) // 本次请求内已尝试过的Key

	var lastFailure *proxyResult

	// 准备请求体（处理模型重定向）
	// [INFO] 修复：保存重定向后的模型名称，用于日志记录和调试
	actualModel, bodyToSend := s.prepareRequestBody(cfg, reqCtx)

	// [FIX] 2026-01: 模型名变更时同步替换 URL 路径
	// 场景：Gemini API 的模型名在 URL 路径中（如 /v1beta/models/gemini-3-flash:streamGenerateContent）
	// 如果模糊匹配将 gemini-3-flash 改为 gemini-3-flash-preview，URL 路径也需要同步更新
	requestPath := replaceModelInPath(reqCtx.requestPath, reqCtx.originalModel, actualModel)

	// 获取渠道URL列表（单URL时退化为单元素切片）
	urls := cfg.GetURLs()
	if len(urls) == 0 {
		return nil, fmt.Errorf("no valid URLs configured for channel %d", cfg.ID)
	}
	selector := s.urlSelector

	// 多URL场景：异步做TCP连接探测预热
	// 目的：通过TCP连接耗时（纯网络延迟，与模型推理无关）为URLSelector提供初始EWMA种子，
	// 避免首次请求随机选到网络延迟更高的URL。
	if len(urls) > 1 && selector != nil {
		urlsSnapshot := append([]string(nil), urls...)
		go selector.ProbeURLs(s.baseCtx, cfg.ID, urlsSnapshot)
	}

	// Key重试循环
	for attempt := 0; attempt < maxKeyRetries; attempt++ {
		// 检查context是否已取消/超时
		if ctxErr := ctx.Err(); ctxErr != nil {
			return buildCtxDoneResult(cfg, ctxErr), nil
		}
		if attempt > 0 {
			enabled, err := s.isChannelEnabledForAttempt(ctx, cfg)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return buildCtxDoneResult(cfg, ctxErr), nil
				}
				log.Printf("[WARN] 检查渠道 %s (ID=%d) 启用状态失败，停止后续Key尝试: %v", cfg.Name, cfg.ID, err)
				return channelDisabledResult(cfg, lastFailure), nil
			}
			if !enabled {
				return channelDisabledResult(cfg, lastFailure), nil
			}
		}

		// 选择可用的API Key（直接传入apiKeys，避免重复查询）
		keyIndex, selectedKey, selectErr := s.selectKeyWithFallback(cfg, apiKeys, triedKeys)
		if selectErr != nil {
			return nil, selectErr
		}

		// 标记Key为已尝试
		triedKeys[keyIndex] = true

		// 更新活跃请求的渠道信息（用于前端显示）
		if reqCtx.activeReqID > 0 && s.activeRequests != nil {
			s.activeRequests.Update(reqCtx.activeReqID, cfg.ID, cfg.Name, cfg.GetChannelType(), cfg.ResolveUpstreamProtocol(string(reqCtx.clientProtocol)), selectedKey, reqCtx.tokenID, cfg.CostMultiplier)
		}

		// URL循环（单URL时退化为单次迭代）
		immediate, urlLastFailure, attemptErr := s.attemptKeyAcrossURLs(
			ctx, cfg, urls, selector,
			keyIndex, selectedKey, reqCtx, actualModel, bodyToSend, requestPath, w)
		if attemptErr != nil {
			return nil, attemptErr
		}
		if immediate != nil {
			return immediate, nil
		}

		// URL循环结束后的Key级决策
		if urlLastFailure != nil {
			lastFailure = urlLastFailure
			if urlLastFailure.nextAction == cooldown.ActionRetryKey {
				continue // 下一个Key
			}
			break // ActionRetryChannel 或 ActionReturnClient
		}
		break
	}

	// Key重试循环结束：返回最后一次失败结果
	if lastFailure != nil {
		return lastFailure, nil
	}

	// 所有Key都尝试过但都失败（无 lastFailure 说明循环未执行或逻辑异常）
	return nil, ErrAllKeysExhausted
}

func shouldSwitchChannelImmediatelyOnHTTP5xx(result *proxyResult) bool {
	// 仅针对“上游已返回HTTP响应”的5xx生效，避免把网络错误误判为同一策略。
	if result == nil || result.header == nil {
		return false
	}
	if result.status < 500 || result.status > 599 {
		return false
	}
	return result.status != util.StatusFirstByteTimeout
}

func shouldCheckSoftErrorForChannelType(channelType string) bool {
	switch util.NormalizeChannelType(channelType) {
	case util.ChannelTypeAnthropic, util.ChannelTypeCodex:
		return true
	default:
		return false
	}
}

// checkSoftError 检测“200 OK 但实际是错误”的软错误响应
// 原则：宁可漏判也不要误判（避免把正常响应当错误导致重试/冷却）
//
// 规则：
// - JSON：先用 bytes.Contains 短路，仅含可能错误标记时才完整 Unmarshal；只看顶层结构
// - text/plain：只接受“前缀匹配 + 短消息”，禁止 Contains 误判用户内容
// - SSE：若看起来像 SSE（data:/event:），直接跳过
func checkSoftError(data []byte, contentType string) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return false
	}

	// 非 JSON 形态下，先排除 SSE（上游可能用 text/plain 返回 SSE）
	if trimmed[0] != '{' {
		if bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:")) ||
			bytes.Contains(data, []byte("\ndata:")) || bytes.Contains(data, []byte("\nevent:")) {
			return false
		}
	}

	ctLower := strings.ToLower(contentType)
	isJSONCT := strings.Contains(ctLower, "application/json")

	// JSON：仅看顶层结构
	if isJSONCT || trimmed[0] == '{' {
		// 快速短路：99% 成功响应顶层不含错误标记，跳过 sonic.Unmarshal
		// 同时覆盖紧凑/带空格两种格式；"error" 带引号避免误匹配 "api_error" 等子串
		if !maybeContainsTopLevelError(trimmed) {
			if trimmed[0] == '{' {
				return false // 形态确实是 JSON 对象 → 已确认无错误
			}
			// CT=JSON 但内容不像 JSON 对象（如纯文本错误消息）→ 走兜底
		} else {
			var obj map[string]any
			if err := sonic.Unmarshal(trimmed, &obj); err == nil {
				if v, ok := obj["error"]; ok && v != nil {
					return true
				}
				if t, ok := obj["type"].(string); ok && strings.EqualFold(t, "error") {
					return true
				}
				return false
			}
			// 形态像 JSON（以 '{' 开头）但解析失败：不猜，避免误判
			if trimmed[0] == '{' {
				return false
			}
			// Content-Type 标注为 JSON 但内容不是 JSON：允许继续走 text/plain 的“前缀+短消息”兜底
		}
	}

	// text/plain：仅前缀 + 短消息
	const maxPlainLen = 256
	if len(trimmed) > maxPlainLen {
		return false
	}
	if bytes.HasPrefix(trimmed, []byte("当前模型负载过高")) {
		return true
	}
	if bytes.HasPrefix(trimmed, []byte("Current model load too high")) {
		return true
	}

	return false
}

// maybeContainsTopLevelError 字节级扫描快速判断响应体是否可能含顶层 error 标记。
// 假阳性（如 {"errors":[...]} 含 "error" 子串）会进入慢路径精确判定，结果仍正确。
func maybeContainsTopLevelError(data []byte) bool {
	return bytes.Contains(data, []byte(`"error"`)) ||
		bytes.Contains(data, []byte(`"type":"error"`)) ||
		bytes.Contains(data, []byte(`"type": "error"`))
}
