package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
)

const (
	// SSEProbeSize 用于探测 text/plain 内容是否包含 SSE 事件的前缀长度（2KB 足够覆盖小事件）
	SSEProbeSize = 2 * 1024
	// softErrorProbeSize 用于探测 HTTP 200 非流响应里的结构化错误。
	softErrorProbeSize = 512
)

// readerWithCloser 给 Reader 补回底层 Closer，避免 bufio/TeeReader 包装后取消无法打断阻塞 Read。
type readerWithCloser struct {
	io.Reader
	io.Closer
}

// onceCloseReadCloser 确保 Close 只执行一次（用于协调 defer 与 context.AfterFunc 的并发关闭）
type onceCloseReadCloser struct {
	io.ReadCloser
	once sync.Once
}

func (rc *onceCloseReadCloser) Close() error {
	var closeErr error
	rc.once.Do(func() {
		closeErr = rc.ReadCloser.Close()
	})
	return closeErr
}

// upstreamRequestTrace captures the point at which the standard transport has
// successfully written the request to its upstream connection. This is later
// than selecting a channel, but earlier than receiving a response.
type upstreamRequestTrace struct {
	writtenAtUnixNano atomic.Int64
}

func (t *upstreamRequestTrace) withContext(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				t.writtenAtUnixNano.CompareAndSwap(0, time.Now().UnixNano())
			}
		},
	})
}

func (t *upstreamRequestTrace) writtenAt() time.Time {
	if unixNano := t.writtenAtUnixNano.Load(); unixNano > 0 {
		return time.Unix(0, unixNano)
	}
	return time.Time{}
}

// disableResponseWriteTimeout 清除响应写超时（http.Server.WriteTimeout），
// 避免大响应或长流式在写回客户端时被传输层截断。
//
// 流式与非流式都需要：非流式大 body 一次性写回也可能超过 WriteTimeout。
// 代价是慢速客户端可拖长写阻塞，但请求整体已受 nonStreamTimeout 的 context 约束，
// 且最大并发由 concurrencySem 封顶，DoS 面有界——故彻底清零而非另设写 deadline。
func disableResponseWriteTimeout(w http.ResponseWriter, requestKind string) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		log.Printf("[WARN] 无法禁用%s请求的 WriteTimeout: %v", requestKind, err)
	}
}

// prependToBody 将前缀数据合并到resp.Body（用于恢复已探测的数据）
func prependToBody(resp *http.Response, prefix []byte) {
	resp.Body = readerWithCloser{
		Reader: io.MultiReader(bytes.NewReader(prefix), resp.Body),
		Closer: resp.Body,
	}
}

// ============================================================================
// 请求构建和转发
// ============================================================================

// buildProxyRequest 构建上游代理请求（统一处理URL、Header、认证）
// 从proxy.go提取，遵循SRP原则
func (s *Server) buildProxyRequest(
	reqCtx *requestContext,
	cfg *model.Config,
	apiKey string,
	method string,
	body []byte,
	hdr http.Header,
	rawQuery, requestPath string,
	baseURL string,
) (*http.Request, error) {
	// 1. 构建完整 URL
	upstreamURL := buildUpstreamURL(baseURL, requestPath, rawQuery)

	// 1.5 anyrouter Anthropic thinking 兜底归一
	body = normalizeAnyrouterAdaptiveThinking(cfg, requestPath, body)

	// 1.55 messages[].role=developer 降级为 system（多数上游 role 枚举不含 developer）
	body = normalizeDeveloperMessageRole(requestPath, body)

	// 1.6 自定义请求体规则（仅对 JSON body 生效）
	body = applyBodyRules(hdr.Get("Content-Type"), body, cfg.BodyRules())

	// 1.7 Codex Responses 入站历史归一：tool_search_*.arguments 必须是对象；
	// anyrouter/new-api 不接受 tool_search_* 历史项，发出前直接清理。
	body = prepareCodexResponsesBodyForUpstream(cfg, protocol.Protocol(runtimeUpstreamProtocol(reqCtx, cfg)), requestPath, body)

	// 1.8 Codex Responses 缓存提示：向 body 注入 prompt_cache_key
	codexSessionID := resolveCodexSessionHint(reqCtx, body, apiKey, hdr)
	if codexSessionID != "" {
		body = injectCodexPromptCacheKey(body, codexSessionID)
	}

	// 2. 创建带上下文的请求
	req, err := buildUpstreamRequest(reqCtx.ctx, method, upstreamURL, body)
	if err != nil {
		return nil, err
	}

	// 3. 复制请求头
	copyRequestHeaders(req, hdr)

	// 4. 注入认证头
	injectAPIKeyHeaders(req, apiKey, runtimeUpstreamProtocol(reqCtx, cfg))

	// 5. anyrouter渠道：确保anthropic-beta包含context-1m
	if cfg.GetChannelType() == util.ChannelTypeAnthropic &&
		strings.Contains(strings.ToLower(cfg.Name), "anyrouter") {
		injectAnthropicBetaFlag(req, "context-1m-2025-08-07")
	}

	// 5.1 本地协议转换到 Anthropic 上游时，OpenAI/Codex/Gemini 客户端不会携带
	// anthropic-version。缺失该头会让部分 Claude Code 兼容上游按 OpenAI body 解析。
	ensureAnthropicVersionHeader(req, runtimeUpstreamProtocol(reqCtx, cfg))

	// 5.5 Codex Responses 缓存提示：设置 Session_id 头（仅客户端未自带时）
	if codexSessionID != "" && req.Header.Get("Session_id") == "" && req.Header.Get("Session-Id") == "" {
		req.Header.Set("Session_id", codexSessionID)
	}

	// 6. 自定义请求头规则（认证头黑名单保护）
	applyHeaderRules(req.Header, cfg.HeaderRules())

	// 7. 非 Anthropic 上游：移除 Anthropic 协议专属头（anthropic-version/anthropic-beta 等）
	stripAnthropicProtocolHeaders(req, runtimeUpstreamProtocol(reqCtx, cfg))

	if reqCtx != nil {
		reqCtx.translatedBody = body
		reqCtx.transformPlan.TranslatedBody = body
	}

	return req, nil
}

func runtimeUpstreamProtocol(reqCtx *requestContext, cfg *model.Config) string {
	if reqCtx != nil {
		if reqCtx.transformPlan.UpstreamProtocol != "" {
			return string(reqCtx.transformPlan.UpstreamProtocol)
		}
		if reqCtx.upstreamProtocol != "" {
			return string(reqCtx.upstreamProtocol)
		}
	}
	if cfg == nil {
		return ""
	}
	return cfg.GetChannelType()
}

// ============================================================================
// 响应处理
// ============================================================================

// handleRequestError 处理网络请求错误
// 从proxy.go提取，遵循SRP原则
func (s *Server) handleRequestError(
	reqCtx *requestContext,
	cfg *model.Config,
	err error,
) (*fwResult, float64, error) {
	reqCtx.stopFirstByteTimer()
	duration := reqCtx.Duration()
	durationSec := duration.Seconds()

	// 检测超时错误：使用统一的内部状态码+冷却策略
	var statusCode int
	if reqCtx.firstByteTimeoutTriggered() {
		// 流式请求首字节超时（定时器触发）
		statusCode = util.StatusFirstByteTimeout
		timeoutMsg := fmt.Sprintf("upstream first byte timeout after %.2fs", durationSec)
		timeout := reqCtx.firstByteTimeout
		if timeout == 0 {
			timeout = s.firstByteTimeout
		}
		if timeout > 0 {
			timeoutMsg = fmt.Sprintf("%s (threshold=%v)", timeoutMsg, timeout)
		}
		err = fmt.Errorf("%s: %w", timeoutMsg, util.ErrUpstreamFirstByteTimeout)
		log.Printf("[TIMEOUT] [上游首字节超时] 渠道ID=%d, 阈值=%v, 实际耗时=%.2fs", cfg.ID, timeout, durationSec)
	} else if errors.Is(err, context.DeadlineExceeded) {
		if reqCtx.isStreaming {
			// 流式请求超时
			err = fmt.Errorf("upstream timeout after %.2fs (streaming): %w", durationSec, err)
			statusCode = util.StatusFirstByteTimeout
			log.Printf("[TIMEOUT] [流式请求超时] 渠道ID=%d, 耗时=%.2fs", cfg.ID, durationSec)
		} else {
			// 非流式请求超时（context.WithTimeout触发）
			timeout := reqCtx.nonStreamTimeout
			if timeout == 0 {
				timeout = s.nonStreamTimeout
			}
			err = fmt.Errorf("upstream timeout after %.2fs (non-stream, threshold=%v): %w",
				durationSec, timeout, err)
			statusCode = 504 // Gateway Timeout
			log.Printf("[TIMEOUT] [非流式请求超时] 渠道ID=%d, 阈值=%v, 耗时=%.2fs", cfg.ID, timeout, durationSec)
		}
	} else {
		// 其他错误：使用统一分类器
		statusCode, _, _ = util.ClassifyError(err)
	}

	return &fwResult{
		Status:        statusCode,
		Body:          []byte(err.Error()),
		FirstByteTime: 0,
	}, durationSec, err
}

// handleErrorResponse 处理错误响应（读取完整响应体）
// 从proxy.go提取，遵循SRP原则
// 限制错误体大小防止 OOM（与入站 DefaultMaxBodyBytes 限制对称）
func (s *Server) handleErrorResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	readStats *streamReadStats,
) (*fwResult, float64, error) {
	rb, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(config.DefaultMaxBodyBytes)))
	diagMsg := ""
	if readErr != nil {
		// 不要创建“孤儿日志”（StatusCode=0），而是把诊断信息合并到本次请求的日志中（KISS）。
		diagMsg = fmt.Sprintf("error reading upstream body: %v", readErr)
	}

	duration := reqCtx.Duration().Seconds()

	return &fwResult{
		Status:         resp.StatusCode,
		Header:         hdrClone,
		Body:           rb,
		FirstByteTime:  readStats.firstByteSec,
		StreamDiagMsg:  diagMsg,
		ThinkingEffort: extractThinkingEffortFromJSON(rb),
	}, duration, nil
}

// streamAndParseResponse 根据Content-Type选择合适的流式传输策略并解析usage
// 返回: (usageParser, streamErr)
func streamAndParseResponse(
	ctx context.Context,
	body io.ReadCloser,
	w http.ResponseWriter,
	contentType string,
	channelType string,
	isStreaming bool,
	beforeWrite func(usageParser) error,
) (usageParser, error) {
	makeFeed := func(parser usageParser) func([]byte) error {
		return func(data []byte) error {
			if err := parser.Feed(data); err != nil {
				return err
			}
			if beforeWrite != nil {
				return beforeWrite(parser)
			}
			return nil
		}
	}
	copySSE := func(stream io.Reader, parser *sseUsageParser) error {
		feed := makeFeed(parser)
		// Codex 与 Anthropic 都有明确的流终止事件，逐行喂给解析器以便在终止事件后立即停止读取，
		// 避免上游在 message_stop / response.completed 之后继续挂住连接。
		if channelType != util.ChannelTypeCodex && channelType != util.ChannelTypeAnthropic {
			return streamCopySSE(ctx, stream, w, feed)
		}
		return streamCopySSE(ctx, stream, w, func(data []byte) error {
			offset := 0
			for offset < len(data) {
				end := len(data)
				if lineEnd := bytes.IndexByte(data[offset:], '\n'); lineEnd >= 0 {
					end = offset + lineEnd + 1
				}
				if err := feed(data[offset:end]); err != nil {
					return err
				}
				offset = end
				if parser.IsStreamComplete() {
					return &stopStreamAfterWriteError{writeBytes: offset}
				}
			}
			return nil
		})
	}

	// SSE流式响应
	if strings.Contains(contentType, "text/event-stream") {
		parser := newSSEUsageParser(channelType)
		streamErr := copySSE(body, parser)
		return parser, streamErr
	}

	// 非标准SSE场景：上游以text/plain发送SSE事件
	if strings.Contains(contentType, "text/plain") && isStreaming {
		reader := bufio.NewReader(body)
		isSSE := peekUntilSSEOrLimit(reader, SSEProbeSize)
		streamBody := readerWithCloser{Reader: reader, Closer: body}

		if isSSE {
			parser := newSSEUsageParser(channelType)
			sseErr := copySSE(streamBody, parser)
			return parser, sseErr
		}
		parser := newJSONUsageParser(channelType)
		copyErr := streamCopy(ctx, streamBody, w, makeFeed(parser))
		return parser, copyErr
	}

	// 非SSE响应：边转发边缓存
	parser := newJSONUsageParser(channelType)
	copyErr := streamCopy(ctx, body, w, makeFeed(parser))
	return parser, copyErr
}

// isClientDisconnectError 判断是否为客户端主动断开导致的错误
// 只识别明确的客户端取消信号，不包括上游服务器错误
// 注意：http2: response body closed 和 stream error 是上游服务器问题，不是客户端断开！
func isClientDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	// context.Canceled 是明确的客户端取消信号（用户点"停止"）
	if errors.Is(err, context.Canceled) {
		return true
	}
	// "client disconnected" 是 gin/net/http 报告的客户端断开
	// 注意：http2: response body closed 和 stream error 是上游服务器问题，
	// 不应在此判断，否则会导致上游异常被忽略而不触发冷却逻辑
	errStr := err.Error()
	return strings.Contains(errStr, "client disconnected")
}

// buildStreamDiagnostics 生成流诊断消息
// 触发条件：流传输错误且未检测到流完成语义（原始结束标志或已转译终态）
// streamComplete: 是否已确认流完成（比 hasUsage 更可靠，因为不是所有请求都有 usage）
func buildStreamDiagnostics(streamErr error, readStats *streamReadStats, streamComplete bool, channelType string, contentType string) string {
	if readStats == nil {
		return ""
	}

	bytesRead := readStats.totalBytes
	readCount := readStats.readCount

	// 流传输异常中断(排除客户端主动断开)
	// 关键：如果检测到流完成语义，说明流已完整传输
	if streamErr != nil && !isClientDisconnectError(streamErr) {
		// 已检测到流完成语义 = 流完整，http2关闭只是正常结束信号
		if streamComplete {
			return "" // 不触发冷却，数据已完整
		}
		return fmt.Sprintf("[WARN] 流传输中断: 错误=%v | 已读取=%d字节(分%d次) | 流结束标志=%v | 渠道=%s | Content-Type=%s",
			streamErr, bytesRead, readCount, streamComplete, channelType, contentType)
	}

	return ""
}

func translatedStreamChunksComplete(clientProtocol protocol.Protocol, chunks [][]byte) bool {
	for _, chunk := range chunks {
		if translatedStreamChunkCompletes(clientProtocol, chunk) {
			return true
		}
	}
	return false
}

func translatedCodexChunksHaveOutput(chunks [][]byte) bool {
	for _, chunk := range chunks {
		eventType, data := parseSSEEventChunk(chunk)
		payload, ok := decodeSSEPayload(data)
		if !ok {
			continue
		}
		payloadType, _ := payload["type"].(string)
		if payloadType == "" {
			payloadType = eventType
		}
		switch payloadType {
		case "response.output_text.delta":
			if visibleTranslatedText(translatedString(payload["delta"])) {
				return true
			}
		case "response.output_item.added", "response.output_item.done":
			item, _ := payload["item"].(map[string]any)
			switch strings.TrimSpace(translatedString(item["type"])) {
			case "function_call", "custom_tool_call", "tool_search_call", "reasoning":
				return true
			case "message":
				if content, ok := item["content"].([]any); ok && len(content) > 0 {
					return true
				}
			}
		}
	}
	return false
}

func translatedString(value any) string {
	text, _ := value.(string)
	return text
}

func visibleTranslatedText(text string) bool {
	text = strings.TrimSpace(text)
	text = strings.Trim(text, "\u200b\ufeff")
	return strings.TrimSpace(text) != ""
}

var sseDoneMarker = []byte("[DONE]")

func translatedStreamChunkCompletes(clientProtocol protocol.Protocol, chunk []byte) bool {
	eventType, data := parseSSEEventChunk(chunk)
	if len(data) == 0 && eventType == "" {
		return false
	}

	switch clientProtocol {
	case protocol.Anthropic:
		return eventType == "message_stop" || ssePayloadType(data) == "message_stop"
	case protocol.Codex:
		return eventType == "response.completed" || ssePayloadType(data) == "response.completed"
	case protocol.OpenAI:
		if bytes.Equal(data, sseDoneMarker) {
			return true
		}
		payload, ok := decodeSSEPayload(data)
		if !ok {
			return false
		}
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			return false
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			return false
		}
		finishReason, hasFinishReason := choice["finish_reason"]
		return hasFinishReason && finishReason != nil
	case protocol.Gemini:
		payload, ok := decodeSSEPayload(data)
		if !ok {
			return false
		}
		candidates, _ := payload["candidates"].([]any)
		if len(candidates) == 0 {
			return false
		}
		candidate, _ := candidates[0].(map[string]any)
		if candidate == nil {
			return false
		}
		finishReason, _ := candidate["finishReason"].(string)
		return strings.TrimSpace(finishReason) != ""
	default:
		return false
	}
}

// parseSSEEventChunk 在 []byte 视图上解析 SSE 事件块，避免 string(chunk) 与 []byte(data) 来回拷贝。
// 返回的 data 是 chunk 的字节副本（拼接多行时已分配新切片），调用方可安全持有。
func parseSSEEventChunk(chunk []byte) (eventType string, data []byte) {
	chunk = bytes.TrimSpace(chunk)
	if len(chunk) == 0 {
		return "", nil
	}
	lines := bytes.Split(chunk, []byte{'\n'})
	dataLines := make([][]byte, 0, 1)
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		if after, ok := bytes.CutPrefix(line, []byte("event:")); ok {
			eventType = string(bytes.TrimSpace(after))
			continue
		}
		if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			dataLines = append(dataLines, bytes.TrimSpace(after))
		}
	}
	if len(dataLines) == 0 {
		return eventType, nil
	}
	return eventType, bytes.Join(dataLines, []byte{'\n'})
}

func ssePayloadType(data []byte) string {
	payload, ok := decodeSSEPayload(data)
	if !ok {
		return ""
	}
	typ, _ := payload["type"].(string)
	return typ
}

func decodeSSEPayload(data []byte) (map[string]any, bool) {
	if len(data) == 0 || bytes.Equal(data, sseDoneMarker) {
		return nil, false
	}

	var payload map[string]any
	if err := sonic.Unmarshal(data, &payload); err != nil {
		return nil, false
	}
	return payload, true
}

func maybePrepareDynamicStreamTransform(reqCtx *requestContext, resp *http.Response) (protocol.Protocol, bool, error) {
	if reqCtx == nil || resp == nil || resp.Body == nil {
		return "", false, nil
	}
	if !reqCtx.isStreaming {
		return "", false, nil
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return "", false, nil
	}

	prefix, err := readSSEPrefixThroughFirstEvent(resp.Body)
	if len(prefix) > 0 {
		prependToBody(resp, prefix)
	}
	if err != nil {
		return "", false, err
	}

	return applyDetectedResponseProtocol(reqCtx, detectProtocolFromSSEPrefix(prefix))
}

func maybePrepareDynamicNonStreamTransform(reqCtx *requestContext, resp *http.Response) (protocol.Protocol, bool, error) {
	if reqCtx == nil || resp == nil || resp.Body == nil || reqCtx.isStreaming {
		return "", false, nil
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "application/json") && !strings.Contains(contentType, "text/plain") {
		return "", false, nil
	}

	rawBody, err := io.ReadAll(resp.Body)
	if len(rawBody) > 0 {
		prependToBody(resp, rawBody)
	}
	if err != nil {
		return "", false, err
	}

	detected := detectProtocolFromJSONBody(rawBody)
	return applyDetectedResponseProtocol(reqCtx, detected)
}

func applyDetectedResponseProtocol(reqCtx *requestContext, detected protocol.Protocol) (protocol.Protocol, bool, error) {
	if detected == "" {
		return "", false, nil
	}
	clientProtocol := reqCtx.transformPlan.ClientProtocol
	if clientProtocol == "" {
		clientProtocol = reqCtx.clientProtocol
	}
	if clientProtocol == "" {
		return detected, false, nil
	}
	if detected == clientProtocol {
		plan := reqCtx.transformPlan
		plan.ClientProtocol = clientProtocol
		plan.UpstreamProtocol = detected
		plan.NeedsTransform = false
		reqCtx.transformPlan = plan
		reqCtx.clientProtocol = clientProtocol
		reqCtx.upstreamProtocol = detected
		return detected, false, nil
	}
	if !protocol.SupportsTransform(detected, clientProtocol) {
		return detected, false, fmt.Errorf("no response transform for detected protocol mismatch: %s -> %s", detected, clientProtocol)
	}

	plan := reqCtx.transformPlan
	plan.ClientProtocol = clientProtocol
	plan.UpstreamProtocol = detected
	plan.NeedsTransform = true
	reqCtx.transformPlan = plan
	reqCtx.clientProtocol = clientProtocol
	reqCtx.upstreamProtocol = detected

	return detected, true, nil
}

func readSSEPrefixThroughFirstEvent(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, SSEBufferSize)
	for buf.Len() < maxSSEEventSize {
		remaining := maxSSEEventSize - buf.Len()
		if remaining < len(tmp) {
			tmp = tmp[:remaining]
		}
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			if firstSSEEventEnd(buf.Bytes()) >= 0 {
				return append([]byte(nil), buf.Bytes()...), nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return append([]byte(nil), buf.Bytes()...), nil
			}
			return append([]byte(nil), buf.Bytes()...), err
		}
	}
	return append([]byte(nil), buf.Bytes()...), fmt.Errorf("SSE first event exceeds max size (%d bytes)", maxSSEEventSize)
}

func detectProtocolFromSSEPrefix(prefix []byte) protocol.Protocol {
	for len(prefix) > 0 {
		eventEnd := firstSSEEventEnd(prefix)
		if eventEnd < 0 {
			eventEnd = len(prefix)
		}
		if detected := detectProtocolFromSSEEvent(prefix[:eventEnd]); detected != "" {
			return detected
		}
		if eventEnd >= len(prefix) {
			break
		}
		prefix = prefix[eventEnd:]
	}
	return ""
}

func detectProtocolFromSSEEvent(event []byte) protocol.Protocol {
	eventType, data := parseSSEEventChunk(event)
	if isAnthropicSSEEventType(eventType) {
		return protocol.Anthropic
	}
	if isCodexSSEEventType(eventType) {
		return protocol.Codex
	}
	payload, ok := decodeSSEPayload(data)
	if !ok {
		return ""
	}
	return detectProtocolFromJSONPayload(payload)
}

func detectProtocolFromJSONBody(raw []byte) protocol.Protocol {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	var payload map[string]any
	if err := sonic.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return detectProtocolFromJSONPayload(payload)
}

func detectProtocolFromJSONPayload(payload map[string]any) protocol.Protocol {
	payloadType, _ := payload["type"].(string)
	if isCodexSSEEventType(payloadType) {
		return protocol.Codex
	}
	if object, _ := payload["object"].(string); object == "response" {
		return protocol.Codex
	}
	if _, ok := payload["choices"].([]any); ok {
		return protocol.OpenAI
	}
	if object, _ := payload["object"].(string); strings.HasPrefix(object, "chat.completion") {
		return protocol.OpenAI
	}
	if _, ok := payload["candidates"].([]any); ok {
		return protocol.Gemini
	}
	if _, ok := payload["usageMetadata"].(map[string]any); ok {
		return protocol.Gemini
	}
	if isAnthropicSSEEventType(payloadType) || (payloadType == "message" && payload["role"] != nil && payload["content"] != nil) {
		return protocol.Anthropic
	}
	return ""
}

func firstSSEEventEnd(data []byte) int {
	pos := 0
	for pos < len(data) {
		idx := bytes.IndexByte(data[pos:], '\n')
		if idx < 0 {
			return -1
		}
		lineEnd := pos + idx
		if len(bytes.TrimRight(data[pos:lineEnd], "\r")) == 0 {
			return lineEnd + 1
		}
		pos = lineEnd + 1
	}
	return -1
}

func isAnthropicSSEEventType(value string) bool {
	switch value {
	case "message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
		"ping":
		return true
	default:
		return false
	}
}

func isCodexSSEEventType(value string) bool {
	return strings.HasPrefix(value, "response.")
}

// handleSuccessResponse 处理成功响应（流式传输）
func (s *Server) handleSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	w http.ResponseWriter,
	channelType string,
	readStats *streamReadStats,
	observer *ForwardObserver,
) (*fwResult, float64, error) {
	if reqCtx.isStreaming && s.protocolRegistry != nil {
		detectedProtocol, transform, err := maybePrepareDynamicStreamTransform(reqCtx, resp)
		if detectedProtocol != "" {
			channelType = string(detectedProtocol)
		}
		if err != nil {
			return &fwResult{
				Status:        resp.StatusCode,
				Header:        hdrClone,
				FirstByteTime: readStats.firstByteSec,
				BytesReceived: readStats.totalBytes,
			}, reqCtx.Duration().Seconds(), err
		}
		if transform {
			return s.handleTranslatedStreamSuccessResponse(reqCtx, resp, hdrClone, w, string(detectedProtocol), readStats, observer)
		}
	}

	if !reqCtx.isStreaming && s.protocolRegistry != nil {
		detectedProtocol, transform, err := maybePrepareDynamicNonStreamTransform(reqCtx, resp)
		if detectedProtocol != "" {
			channelType = string(detectedProtocol)
		}
		if err != nil {
			return &fwResult{
				Status:        resp.StatusCode,
				Header:        hdrClone,
				FirstByteTime: readStats.firstByteSec,
				BytesReceived: readStats.totalBytes,
			}, reqCtx.Duration().Seconds(), err
		}
		if transform {
			return s.handleTranslatedNonStreamSuccessResponse(reqCtx, resp, hdrClone, w, string(detectedProtocol), readStats, observer)
		}
	}

	if reqCtx.isStreaming &&
		s.protocolRegistry != nil &&
		reqCtx.transformPlan.NeedsTransform &&
		(strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") ||
			strings.Contains(resp.Header.Get("Content-Type"), "text/plain")) {
		return s.handleTranslatedStreamSuccessResponse(reqCtx, resp, hdrClone, w, channelType, readStats, observer)
	}

	if !reqCtx.isStreaming &&
		s.protocolRegistry != nil &&
		reqCtx.transformPlan.NeedsTransform {
		return s.handleTranslatedNonStreamSuccessResponse(reqCtx, resp, hdrClone, w, channelType, readStats, observer)
	}

	// [FIX] 流式请求：禁用 WriteTimeout，避免长时间流被服务器自己切断
	// Go 1.20+ http.ResponseController 支持动态调整 WriteDeadline
	if reqCtx.isStreaming {
		disableResponseWriteTimeout(w, "流式")
	} else {
		disableResponseWriteTimeout(w, "非流式")
	}

	streamWriter := w
	var deferredWriter *deferredResponseWriter
	if reqCtx.isStreaming {
		deferredWriter = newDeferredResponseWriter(w, responseCommitHook(observer))
		streamWriter = deferredWriter
	}

	if deferredWriter == nil && observer != nil && observer.BeforeResponseCommit != nil {
		if err := observer.BeforeResponseCommit(); err != nil {
			return &fwResult{Status: resp.StatusCode, Header: hdrClone}, reqCtx.Duration().Seconds(), err
		}
	}
	// 写入响应头
	filterAndWriteResponseHeaders(streamWriter, resp.Header)
	streamWriter.WriteHeader(resp.StatusCode)

	// 流式传输并解析usage
	contentType := resp.Header.Get("Content-Type")
	parser, streamErr := streamAndParseResponse(
		reqCtx.ctx, resp.Body, streamWriter, contentType, channelType, reqCtx.isStreaming,
		func(parser usageParser) error {
			if deferredWriter == nil || deferredWriter.Committed() {
				return nil
			}
			if parser.GetLastError() != nil || parser.HasStreamOutput() || parser.IsStreamComplete() {
				markFirstStreamResponse(reqCtx, readStats, observer)
			}
			if parser.GetLastError() != nil {
				return errAbortStreamBeforeWrite
			}
			if parser.HasStreamOutput() {
				return deferredWriter.Commit()
			}
			return nil
		},
	)
	// 手动跳过会先取消 attemptCtx；底层读取通常只返回 context.Canceled，
	// 因此还要查看取消原因，确保丢弃 deferred writer 中尚未提交的缓冲内容。
	abortedBeforeCommit := shouldAbortStreamBeforeWrite(streamErr) || isManualChannelSkip(reqCtx.ctx)
	if abortedBeforeCommit && deferredWriter != nil {
		if errors.Is(streamErr, errAbortStreamBeforeWrite) {
			streamErr = nil
		}
		deferredWriter.AbortBeforeCommit()
	} else if deferredWriter != nil && !deferredWriter.Committed() && isEmptyStreamOutput(parser, readStats) {
		if streamErr == nil {
			return emptyOKResponseResult(reqCtx, resp, hdrClone, readStats, emptyStreamDetail(readStats))
		}
	} else if deferredWriter != nil && !deferredWriter.Committed() {
		if commitErr := deferredWriter.Commit(); commitErr != nil && streamErr == nil {
			streamErr = commitErr
		}
	}

	// 构建结果
	result := &fwResult{
		Status:            resp.StatusCode,
		Header:            hdrClone,
		FirstByteTime:     readStats.firstByteSec,
		BytesReceived:     readStats.totalBytes, // 记录已接收字节数，用于499诊断
		ResponseCommitted: deferredWriter == nil || deferredWriter.Committed(),
	}

	// 提取usage数据和错误事件
	var streamComplete bool
	if parser != nil {
		result.InputTokens, result.OutputTokens, result.CacheReadInputTokens, result.CacheCreationInputTokens = parser.GetUsage()
		result.ReasoningTokens = parser.GetReasoningTokens()
		result.Cache5mInputTokens, result.Cache1hInputTokens, result.ServiceTier = parser.GetCacheBreakdown()
		result.ToolCostUSD = parser.GetToolCostUSD()
		result.ThinkingEffort = parser.GetThinkingEffort()

		if errorEvent := parser.GetLastError(); errorEvent != nil {
			result.SSEErrorEvent = errorEvent
		}
		streamComplete = parser.IsStreamComplete()
	}

	// 生成流诊断消息（仅流请求）
	if reqCtx.isStreaming {
		// [VALIDATE] 诊断增强: 传递contentType帮助定位问题(区分SSE/JSON/其他)
		// 使用 streamComplete 而非 hasUsage，因为不是所有请求都有 usage 信息
		if diagMsg := buildStreamDiagnostics(streamErr, readStats, streamComplete, channelType, contentType); diagMsg != "" {
			result.StreamDiagMsg = diagMsg
			log.Print(diagMsg)
		} else if streamComplete && streamErr != nil {
			// [FIX] 流式请求：检测到流结束标志（[DONE]/message_stop）说明数据完整
			// 所有收尾阶段的错误都应忽略，包括：
			// - http2 流关闭（正常结束信号）
			// - context.Canceled（客户端在传输完成后取消，不应标记为499）
			streamErr = nil
		}
	} else {
		// [FIX] 非流式请求：如果有数据被传输，且错误是 HTTP/2 流关闭相关的，视为成功
		// 原因：streamCopy 已将数据写入 ResponseWriter，客户端已收到完整响应
		// http2 流关闭只是 "确认结束" 阶段的错误，不影响已传输的数据
		if readStats.totalBytes > 0 && streamErr != nil && isHTTP2StreamCloseError(streamErr) {
			streamErr = nil
		}
	}

	return result, reqCtx.Duration().Seconds(), streamErr
}

func (s *Server) handleTranslatedNonStreamSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	w http.ResponseWriter,
	channelType string,
	readStats *streamReadStats,
	observer *ForwardObserver,
) (*fwResult, float64, error) {
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return &fwResult{
			Status:        resp.StatusCode,
			Header:        hdrClone,
			Body:          []byte(err.Error()),
			FirstByteTime: readStats.firstByteSec,
		}, reqCtx.Duration().Seconds(), err
	}

	readStats.totalBytes = int64(len(rawBody))
	if len(rawBody) > 0 {
		readStats.readCount = 1
	}

	parser := newJSONUsageParser(channelType)
	if err := parser.Feed(rawBody); err != nil {
		return &fwResult{
			Status:        resp.StatusCode,
			Header:        hdrClone,
			Body:          rawBody,
			FirstByteTime: readStats.firstByteSec,
		}, reqCtx.Duration().Seconds(), err
	}

	translatedBody, err := s.protocolRegistry.TranslateResponseNonStream(
		reqCtx.ctx,
		reqCtx.transformPlan.UpstreamProtocol,
		reqCtx.transformPlan.ClientProtocol,
		reqCtx.transformPlan.ResponseModel(),
		reqCtx.transformPlan.OriginalBody,
		reqCtx.transformPlan.TranslatedBody,
		rawBody,
	)
	if err != nil {
		return &fwResult{
			Status:        resp.StatusCode,
			Header:        hdrClone,
			Body:          rawBody,
			FirstByteTime: readStats.firstByteSec,
		}, reqCtx.Duration().Seconds(), err
	}

	translatedHeader := resp.Header.Clone()
	translatedHeader.Set("Content-Type", "application/json")
	translatedHeader.Del("Content-Encoding")

	disableResponseWriteTimeout(w, "非流式")

	if beforeCommit := responseCommitHook(observer); beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			return &fwResult{Status: resp.StatusCode, Header: hdrClone}, reqCtx.Duration().Seconds(), err
		}
	}
	filterAndWriteResponseHeaders(w, translatedHeader)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(translatedBody)

	result := &fwResult{
		Status:            resp.StatusCode,
		Header:            hdrClone,
		FirstByteTime:     readStats.firstByteSec,
		BytesReceived:     readStats.totalBytes,
		ResponseCommitted: true,
	}
	result.InputTokens, result.OutputTokens, result.CacheReadInputTokens, result.CacheCreationInputTokens = parser.GetUsage()
	result.ReasoningTokens = parser.GetReasoningTokens()
	result.Cache5mInputTokens = parser.Cache5mInputTokens
	result.Cache1hInputTokens = parser.Cache1hInputTokens
	result.ServiceTier = parser.ServiceTier
	result.ToolCostUSD = parser.GetToolCostUSD()
	result.ThinkingEffort = parser.GetThinkingEffort()

	return result, reqCtx.Duration().Seconds(), nil
}

func (s *Server) handleTranslatedStreamSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	w http.ResponseWriter,
	channelType string,
	readStats *streamReadStats,
	observer *ForwardObserver,
) (*fwResult, float64, error) {
	disableResponseWriteTimeout(w, "流式")

	deferredWriter := newDeferredResponseWriter(w, responseCommitHook(observer))
	filterAndWriteResponseHeaders(deferredWriter, resp.Header)
	deferredWriter.WriteHeader(resp.StatusCode)

	parser := newSSEUsageParser(channelType)
	var translatedComplete bool
	var translatedHasOutput bool
	gateOnTranslatedCodexOutput := reqCtx.transformPlan.ClientProtocol == protocol.Codex
	var state any
	streamErr := streamTransformSSEEventsUntil(
		reqCtx.ctx,
		resp.Body,
		deferredWriter,
		func(rawEvent []byte) error {
			if err := parser.Feed(rawEvent); err != nil {
				return err
			}
			if parser.GetLastError() != nil || (!gateOnTranslatedCodexOutput && parser.HasStreamOutput()) || parser.IsStreamComplete() {
				markFirstStreamResponse(reqCtx, readStats, observer)
			}
			if !deferredWriter.Committed() && parser.GetLastError() != nil {
				return errAbortStreamBeforeWrite
			}
			if !gateOnTranslatedCodexOutput && !deferredWriter.Committed() && parser.HasStreamOutput() {
				return deferredWriter.Commit()
			}
			return nil
		},
		func(rawEvent []byte) ([][]byte, error) {
			chunks, err := s.protocolRegistry.TranslateResponseStream(
				reqCtx.ctx,
				reqCtx.transformPlan.UpstreamProtocol,
				reqCtx.transformPlan.ClientProtocol,
				reqCtx.transformPlan.ResponseModel(),
				reqCtx.transformPlan.OriginalBody,
				reqCtx.transformPlan.TranslatedBody,
				rawEvent,
				&state,
			)
			if err != nil {
				return nil, err
			}
			if !translatedComplete && translatedStreamChunksComplete(reqCtx.transformPlan.ClientProtocol, chunks) {
				translatedComplete = true
			}
			if gateOnTranslatedCodexOutput && translatedCodexChunksHaveOutput(chunks) {
				translatedHasOutput = true
				markFirstStreamResponse(reqCtx, readStats, observer)
				if !deferredWriter.Committed() {
					if err := deferredWriter.Commit(); err != nil {
						return nil, err
					}
				}
			}
			return chunks, nil
		},
		func() bool {
			// Codex 与 Anthropic 的上游流都有确定的终止事件，可在其到达后停止读取。
			terminalProtocol := reqCtx.transformPlan.UpstreamProtocol == protocol.Codex ||
				reqCtx.transformPlan.UpstreamProtocol == protocol.Anthropic
			return terminalProtocol && parser.IsStreamComplete() && translatedComplete
		},
	)

	// 同普通流式透传：手动跳过的底层读取可能只暴露 context.Canceled，
	// 必须按取消原因清理尚未提交的转换结果。
	abortedBeforeCommit := shouldAbortStreamBeforeWrite(streamErr) || isManualChannelSkip(reqCtx.ctx)
	if abortedBeforeCommit && deferredWriter != nil {
		if errors.Is(streamErr, errAbortStreamBeforeWrite) {
			streamErr = nil
		}
		deferredWriter.AbortBeforeCommit()
	} else if !deferredWriter.Committed() && ((gateOnTranslatedCodexOutput && !translatedHasOutput) || (!gateOnTranslatedCodexOutput && isEmptyStreamOutput(parser, readStats))) {
		if streamErr == nil {
			return emptyOKResponseResult(reqCtx, resp, hdrClone, readStats, emptyStreamDetail(readStats))
		}
	} else if !deferredWriter.Committed() {
		if commitErr := deferredWriter.Commit(); commitErr != nil && streamErr == nil {
			streamErr = commitErr
		}
	}

	result := &fwResult{
		Status:            resp.StatusCode,
		Header:            hdrClone,
		FirstByteTime:     readStats.firstByteSec,
		BytesReceived:     readStats.totalBytes,
		ResponseCommitted: deferredWriter.Committed(),
	}
	result.InputTokens, result.OutputTokens, result.CacheReadInputTokens, result.CacheCreationInputTokens = parser.GetUsage()
	result.ReasoningTokens = parser.GetReasoningTokens()
	result.Cache5mInputTokens = parser.Cache5mInputTokens
	result.Cache1hInputTokens = parser.Cache1hInputTokens
	result.ServiceTier = parser.ServiceTier
	result.ToolCostUSD = parser.GetToolCostUSD()
	result.ThinkingEffort = parser.GetThinkingEffort()
	result.SSEErrorEvent = parser.GetLastError()
	streamComplete := parser.IsStreamComplete() || translatedComplete

	if diagMsg := buildStreamDiagnostics(streamErr, readStats, streamComplete, channelType, resp.Header.Get("Content-Type")); diagMsg != "" {
		result.StreamDiagMsg = diagMsg
		log.Print(diagMsg)
	} else if streamComplete && streamErr != nil {
		streamErr = nil
	}

	return result, reqCtx.Duration().Seconds(), streamErr
}

// isHTTP2StreamCloseError 判断是否是 HTTP/2 流关闭相关的错误
// 这类错误发生在数据传输完成后，不影响已传输的数据完整性
func isHTTP2StreamCloseError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "http2: response body closed") ||
		strings.Contains(errStr, "stream error:")
}

// peekUntilSSEOrLimit 增量探测 text/plain SSE，避免短流在上游不 EOF 时等待满 2KB。
func peekUntilSSEOrLimit(reader *bufio.Reader, limit int) bool {
	for n := 1; n <= limit; n++ {
		current, err := reader.Peek(n)
		if looksLikeSSE(current) {
			return true
		}
		if err != nil {
			return false
		}
	}
	return false
}

// looksLikeSSE 粗略判断文本内容是否包含 SSE 事件结构
func looksLikeSSE(data []byte) bool {
	// 同时包含 event: 与 data: 行。必须是行前缀，避免普通JSON字符串里的
	// "event:" 文本把非流响应误判成SSE。
	hasEvent := false
	hasData := false
	for len(data) > 0 {
		line := data
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			line = data[:idx]
			data = data[idx+1:]
		} else {
			data = nil
		}

		line = bytes.TrimLeft(line, " \t\r")
		if bytes.HasPrefix(line, []byte("event:")) {
			hasEvent = true
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			hasData = true
		}
		if hasEvent && hasData {
			return true
		}
	}
	return false
}

func attachFirstByteDetector(
	reqCtx *requestContext,
	resp *http.Response,
	readStats *streamReadStats,
	observer *ForwardObserver,
) {
	resp.Body = &firstByteDetector{
		ReadCloser: resp.Body,
		stats:      readStats,
		onFirstRead: func() {
			if reqCtx.isStreaming && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
			if reqCtx.isStreaming {
				reqCtx.stopFirstByteTimer()
			}
			if readStats.firstByteSec == 0 {
				readStats.firstByteSec = reqCtx.Duration().Seconds()
				if readStats.firstByteSec == 0 {
					readStats.firstByteSec = time.Nanosecond.Seconds()
				}
			}
			if reqCtx.isStreaming && observer != nil && observer.OnFirstByteRead != nil {
				observer.OnFirstByteRead()
			}
		},
		onBytesRead: func(n int64) {
			if observer != nil && observer.OnBytesRead != nil {
				observer.OnBytesRead(n)
			}
		},
		onResponseBytes: func(data []byte) {
			if observer != nil && observer.OnResponseBytes != nil {
				observer.OnResponseBytes(data)
			}
		},
	}
}

func markFirstStreamResponse(reqCtx *requestContext, readStats *streamReadStats, observer *ForwardObserver) {
	if !reqCtx.isStreaming || readStats.firstByteSec > 0 {
		return
	}

	reqCtx.stopFirstByteTimer()
	readStats.firstByteSec = reqCtx.Duration().Seconds()
	if readStats.firstByteSec == 0 {
		readStats.firstByteSec = time.Nanosecond.Seconds()
	}
	if observer != nil && observer.OnFirstByteRead != nil {
		observer.OnFirstByteRead()
	}
}

func shouldProbeSoftError(reqCtx *requestContext, resp *http.Response, channelType string) bool {
	if resp.StatusCode != http.StatusOK || reqCtx.isStreaming {
		return false
	}
	if !shouldCheckSoftErrorForChannelType(channelType) {
		return false
	}
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(ct, "text/plain") || strings.Contains(ct, "application/json")
}

// classifySSEErrorStatus 根据响应体内容判定 SSE 错误的内部状态码：
// 1308 配额超限 → 596；明确限流 → 429；其他 → 597。
func classifySSEErrorStatus(body []byte) int {
	if _, is1308 := util.ParseResetTimeFrom1308Error(body); is1308 {
		return util.StatusQuotaExceeded
	}
	if isSSERateLimitError(body) {
		return http.StatusTooManyRequests
	}
	return util.StatusSSEError
}

func isSSERateLimitError(body []byte) bool {
	var payload struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := sonic.Unmarshal(body, &payload); err != nil {
		return false
	}
	return isRateLimitErrorType(payload.Error.Type) || isRateLimitErrorType(payload.Error.Code)
}

func isRateLimitErrorType(value string) bool {
	switch strings.ToLower(value) {
	case "rate_limit_error", "rate_limit_exceeded", "too_many_requests":
		return true
	default:
		return false
	}
}

func (s *Server) probeSoftErrorResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	cfg *model.Config,
	channelType string,
	readStats *streamReadStats,
) (handled bool, res *fwResult, duration float64, err error) {
	if !shouldProbeSoftError(reqCtx, resp, channelType) {
		return false, nil, 0, nil
	}

	ct := resp.Header.Get("Content-Type")
	buf := make([]byte, softErrorProbeSize)
	n, readErr := resp.Body.Read(buf)
	if readErr != nil && readErr != io.EOF {
		log.Printf("[WARN] 软错误检测读取失败: %v", readErr)
	}

	validData := buf[:n]
	if n > 0 && checkSoftError(validData, ct) {
		log.Printf("[WARN] [软错误检测] 渠道ID=%d, 响应200但疑似错误响应: %s", cfg.ID, truncateErr(safeBodyToString(validData)))
		resp.StatusCode = classifySSEErrorStatus(validData)
		prependToBody(resp, validData)
		res, duration, err = s.handleErrorResponse(reqCtx, resp, hdrClone, readStats)
		return true, res, duration, err
	}

	if n > 0 {
		prependToBody(resp, validData)
	}
	return false, nil, 0, nil
}

func emptyOKResponseResult(reqCtx *requestContext, resp *http.Response, hdrClone http.Header, readStats *streamReadStats, detail string) (*fwResult, float64, error) {
	duration := reqCtx.Duration().Seconds()
	err := fmt.Errorf("%w (200 OK %s)", util.ErrUpstreamEmptyResponse, detail)
	return &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		Body:          []byte(err.Error()),
		FirstByteTime: readStats.firstByteSec,
	}, duration, err
}

func isEmptyStreamOutput(parser usageParser, readStats *streamReadStats) bool {
	if readStats == nil || readStats.totalBytes == 0 {
		return true
	}
	return parser != nil && !parser.HasStreamOutput()
}

func emptyStreamDetail(readStats *streamReadStats) string {
	if readStats == nil || readStats.totalBytes == 0 {
		return "without response body"
	}
	return "without response content"
}

func probeEmptyOKResponse(reqCtx *requestContext, resp *http.Response, hdrClone http.Header, readStats *streamReadStats) (bool, *fwResult, float64, error) {
	if reqCtx.isStreaming || resp.StatusCode != http.StatusOK {
		return false, nil, 0, nil
	}

	if resp.Body == nil {
		res, duration, err := emptyOKResponseResult(reqCtx, resp, hdrClone, readStats, "with nil body")
		return true, res, duration, err
	}

	if resp.Header.Get("Content-Length") == "0" {
		res, duration, err := emptyOKResponseResult(reqCtx, resp, hdrClone, readStats, "with Content-Length: 0")
		return true, res, duration, err
	}

	var firstByte [1]byte
	n, readErr := resp.Body.Read(firstByte[:])
	if n > 0 {
		prependToBody(resp, firstByte[:n])
		return false, nil, 0, nil
	}
	if readErr == io.EOF {
		res, duration, err := emptyOKResponseResult(reqCtx, resp, hdrClone, readStats, "without response body")
		return true, res, duration, err
	}
	return false, nil, 0, nil
}

// handleResponse 处理 HTTP 响应（错误或成功）
// 从proxy.go提取，遵循SRP原则
// channelType: 渠道类型,用于精确识别usage格式
// cfg: 渠道配置,用于提取渠道ID
// apiKey: 使用的API Key,用于日志记录
func (s *Server) handleResponse(
	reqCtx *requestContext,
	resp *http.Response,
	w http.ResponseWriter,
	channelType string,
	cfg *model.Config,
	_ string,
	observer *ForwardObserver,
) (*fwResult, float64, error) {
	hdrClone := resp.Header.Clone()
	readStats := &streamReadStats{}

	attachFirstByteDetector(reqCtx, resp, readStats, observer)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.handleErrorResponse(reqCtx, resp, hdrClone, readStats)
	}

	if handled, res, duration, err := probeEmptyOKResponse(reqCtx, resp, hdrClone, readStats); handled {
		return res, duration, err
	}

	if handled, res, duration, err := s.probeSoftErrorResponse(reqCtx, resp, hdrClone, cfg, channelType, readStats); handled {
		return res, duration, err
	}

	return s.handleSuccessResponse(reqCtx, resp, hdrClone, w, channelType, readStats, observer)
}

// ============================================================================
// 核心转发函数
// ============================================================================

// forwardOnceAsync 异步流式转发，透明转发客户端原始请求
// 从proxy.go提取，遵循SRP原则
// 参数新增 apiKey 用于直接传递已选中的API Key（从KeySelector获取）
// 参数新增 method 用于支持任意HTTP方法（GET、POST、PUT、DELETE等）
func (s *Server) forwardOnceAsync(ctx context.Context, cfg *model.Config, apiKey string, method string, plan protocol.TransformPlan, hdr http.Header, rawQuery string, baseURL string, w http.ResponseWriter, observer *ForwardObserver) (*fwResult, float64, error) {
	// 1. 创建请求上下文（处理超时）
	reqCtx := s.newRequestContextWithTimeouts(ctx, plan.UpstreamPath, plan.TranslatedBody, s.resolveProtocolTimeouts(cfg, plan))
	reqCtx.transformPlan = plan
	reqCtx.clientProtocol = plan.ClientProtocol
	reqCtx.upstreamProtocol = plan.UpstreamProtocol
	reqCtx.originalBody = plan.OriginalBody
	reqCtx.translatedBody = plan.TranslatedBody
	reqCtx.originalModel = plan.ResponseModel()
	defer reqCtx.cleanup() // [INFO] 统一清理：定时器 + context（总是安全）

	var riskCapture *channelRiskResponseCapture
	attemptObserver := observer

	if s.protocolRegistry != nil && plan.NeedsTransform {
		translatedBody, err := s.protocolRegistry.TranslateRequest(plan.ClientProtocol, plan.UpstreamProtocol, plan.RequestModel(), plan.TranslatedBody, plan.Streaming)
		if err != nil {
			return nil, 0, fmt.Errorf("translate request for channel %d: %w", cfg.ID, err)
		}
		plan.TranslatedBody = translatedBody
		switch plan.UpstreamProtocol {
		case protocol.Gemini:
			plan.UpstreamPath = buildGeminiGeneratePath(plan.RequestModel(), plan.Streaming)
		case protocol.Anthropic:
			plan.UpstreamPath = buildAnthropicMessagesPath()
		case protocol.OpenAI:
			plan.UpstreamPath = buildOpenAIChatPath()
		case protocol.Codex:
			plan.UpstreamPath = buildCodexResponsesPath()
		}
		reqCtx.transformPlan = plan
		reqCtx.translatedBody = translatedBody
	}

	// 2. 构建上游请求
	req, err := s.buildProxyRequest(reqCtx, cfg, apiKey, method, reqCtx.transformPlan.TranslatedBody, hdr, rawQuery, reqCtx.transformPlan.UpstreamPath, baseURL)
	if err != nil {
		return nil, 0, err
	}
	if s.channelRisk != nil {
		riskCapture = &channelRiskResponseCapture{}
		observerCopy := ForwardObserver{}
		if observer != nil {
			observerCopy = *observer
		}
		previousOnResponseBytes := observerCopy.OnResponseBytes
		observerCopy.OnResponseBytes = func(data []byte) {
			if previousOnResponseBytes != nil {
				previousOnResponseBytes(data)
			}
			riskCapture.append(data)
		}
		attemptObserver = &observerCopy
	}

	// 2.5 Debug捕获：记录发送前的请求信息
	dc := s.captureDebugRequest(req, reqCtx.transformPlan.TranslatedBody)
	if attemptObserver != nil && attemptObserver.OnDebugCapture != nil {
		attemptObserver.OnDebugCapture(dc)
	}

	// 3. 发送请求。日志时间使用请求实际写入上游连接的时刻，避免把本地
	// 连接池等待或协议转换时间误显示成渠道已经收到请求的时间。
	requestTrace := &upstreamRequestTrace{}
	req = req.WithContext(requestTrace.withContext(req.Context()))
	resp, err := s.doUpstreamRequest(cfg, req)
	requestSentAt := requestTrace.writtenAt()
	if err != nil && (errors.Is(err, ErrChannelRPMExceeded) || errors.Is(err, ErrChannelConcurrencyExceeded)) {
		return nil, reqCtx.Duration().Seconds(), err
	}

	// [INFO] 修复（2025-12）：客户端取消时主动关闭 response body，立即中断上游传输
	// 问题：streamCopy 中的 Read 阻塞时，无法立即响应 context 取消，上游继续生成完整响应
	// 解决：使用 Go 1.21+ context.AfterFunc 替代手动 goroutine（零泄漏风险）
	//   - HTTP/1.1: 关闭 TCP 连接 → 上游收到 RST，立即停止发送
	//   - HTTP/2: 发送 RST_STREAM 帧 → 取消当前 stream（不影响同连接的其他请求）
	// 效果：避免 AI 流式生成场景下，用户点"停止"后上游仍生成数千 tokens 的浪费
	if resp != nil {
		// Debug捕获：在 resp.Body 被其他层包装前，用 TeeReader 旁路捕获响应体
		dc.wrapResponseBody(resp)

		// 注意：resp.Body 后续会被包装（例如 firstByteDetector）。
		// 因此需要先把 body 封装成“稳定引用”，避免取消 goroutine 与包装赋值发生 data race。
		body := &onceCloseReadCloser{ReadCloser: resp.Body}
		resp.Body = body

		// 正常返回时关闭（Close 幂等，允许与 AfterFunc 并发触发）
		defer func() { _ = resp.Body.Close() }()

		// [INFO] 使用 context.AfterFunc 监听请求取消/超时（Go 1.21+，标准库保证无泄漏）
		// 必须监听 reqCtx.ctx（而非父 ctx），否则 nonStreamTimeout/firstByteTimeout 触发时无法强制打断阻塞 Read。
		stop := context.AfterFunc(reqCtx.ctx, func() { _ = body.Close() })
		defer stop() // 取消注册（请求正常结束时避免内存泄漏）
	}

	if err != nil {
		errRes, errDur, errErr := s.handleRequestError(reqCtx, cfg, err)
		if errRes != nil {
			errRes.RequestSentAt = requestSentAt
			errRes.DebugData = dc.buildEntry(resp)
		}
		return errRes, errDur, errErr
	}

	// 4. 处理响应(传递channelType用于精确识别usage格式,传递渠道信息用于日志记录,传递观测回调)
	var res *fwResult
	var duration float64
	res, duration, err = s.handleResponse(reqCtx, resp, w, string(reqCtx.upstreamProtocol), cfg, apiKey, attemptObserver)
	if isManualChannelSkip(ctx) {
		err = errManualChannelSkip
	}

	// [FIX] 2025-12: 流式传输过程中首字节超时的错误修正
	// 场景：响应头已收到(200 OK)，但在读取响应体时超时定时器触发
	// 此时 streamCopy 返回 context.Canceled，但实际原因是首字节超时
	// 需要将错误包装为 ErrUpstreamFirstByteTimeout，确保正确分类和日志记录
	if err != nil && reqCtx.firstByteTimeoutTriggered() {
		timeoutMsg := fmt.Sprintf("upstream first byte timeout after %.2fs", duration)
		timeout := reqCtx.firstByteTimeout
		if timeout == 0 {
			timeout = s.firstByteTimeout
		}
		if timeout > 0 {
			timeoutMsg = fmt.Sprintf("%s (threshold=%v)", timeoutMsg, timeout)
		}
		err = fmt.Errorf("%s: %w", timeoutMsg, util.ErrUpstreamFirstByteTimeout)
		res.Status = util.StatusFirstByteTimeout
		log.Printf("[TIMEOUT] [上游首字节超时-流传输中断] 渠道ID=%d, 阈值=%v, 实际耗时=%.2fs", cfg.ID, timeout, duration)
	}

	// 5. Debug捕获：构建完整的 debug 日志条目（响应体已通过 TeeReader 收集完毕）
	if res != nil {
		res.RequestSentAt = requestSentAt
		res.DebugData = dc.buildEntry(resp)
	}
	if err == nil {
		s.recordChannelRiskObservation(cfg.ID, res, riskCapture)
	}

	return res, duration, err
}

// ============================================================================
// 单次转发尝试
// ============================================================================

func markSSEErrorForwardResult(res *fwResult) {
	res.Body = res.SSEErrorEvent
	res.Status = classifySSEErrorStatus(res.SSEErrorEvent)
	if res.Status == util.StatusQuotaExceeded {
		res.StreamDiagMsg = fmt.Sprintf("Quota Exceeded (1308): %s", safeBodyToString(res.SSEErrorEvent))
		return
	}
	res.StreamDiagMsg = fmt.Sprintf("SSE error event: %s", safeBodyToString(res.SSEErrorEvent))
}

func markIncompleteStreamForwardResult(res *fwResult) {
	res.Body = []byte(res.StreamDiagMsg)
	res.Status = util.StatusStreamIncomplete
}

func (s *Server) handleCommittedAwareProxyError(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string,
	selectedKey string,
	res *fwResult,
	duration float64,
	reqCtx *proxyRequestContext,
	deferChannelCooldown bool,
) (*proxyResult, cooldown.Action) {
	if !res.ResponseCommitted {
		return s.handleProxyErrorResponse(
			ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx, deferChannelCooldown, false,
		)
	}
	return s.handleStreamingErrorNoRetry(ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx)
}

func (s *Server) handleSuccessfulForwardAnomaly(
	ctx context.Context,
	cfg *model.Config,
	keyIndex int,
	actualModel string,
	selectedKey string,
	res *fwResult,
	duration float64,
	reqCtx *proxyRequestContext,
	deferChannelCooldown bool,
) (*proxyResult, cooldown.Action, bool) {
	if res.SSEErrorEvent != nil {
		log.Printf("[WARN]  [SSE错误处理] HTTP状态码200但检测到SSE error事件，触发冷却逻辑")
		markSSEErrorForwardResult(res)
		result, action := s.handleCommittedAwareProxyError(
			ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx, deferChannelCooldown,
		)
		return result, action, true
	}

	if res.StreamDiagMsg != "" {
		log.Printf("[WARN]  [流响应不完整] HTTP状态码200但检测到流响应不完整，触发冷却逻辑: %s", res.StreamDiagMsg)
		markIncompleteStreamForwardResult(res)
		result, action := s.handleCommittedAwareProxyError(
			ctx, cfg, keyIndex, actualModel, selectedKey, res, duration, reqCtx, deferChannelCooldown,
		)
		return result, action, true
	}

	return nil, cooldown.ActionReturnClient, false
}

// forwardAttempt 单次转发尝试（包含错误处理和日志记录）
// 从proxy.go提取，遵循SRP原则
// 返回：(proxyResult, nextAction)
