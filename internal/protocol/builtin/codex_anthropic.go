package builtin

import (
	"context"
	"fmt"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
)

type codexToAnthropicStreamState struct {
	started             bool
	blockIndex          int
	model               string
	responseID          string
	openBlock           bool
	lastBlock           string
	hasTextDelta        bool
	thinkingBlockOpen   bool
	thinkingStopPending bool
	thinkingSignature   string
	toolNameMap         map[string]string
	usage               struct {
		inputTokens              int64
		outputTokens             int64
		cachedTokens             int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
}

type anthropicToCodexStreamState struct {
	model      string
	responseID string
	toolRoutes map[string]codexOpenAIToolRoute
	usage      struct {
		inputTokens              int64
		outputTokens             int64
		totalTokens              int64
		cacheReadInputTokens     int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
	toolID             string
	toolName           string
	toolInput          any
	toolJSON           string
	toolActive         bool
	textActive         bool
	textStarted        bool
	textValue          string
	textID             string
	textOutputIndex    int
	textSequence       int
	reasoningActive    bool
	reasoningText      string
	reasoningSignature string
	reasoningData      string
}

func convertCodexRequestToAnthropic(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req codexRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeCodexConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeAnthropicRequest(model, conv, stream)
}

func convertAnthropicRequestToCodex(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req anthropicMessagesRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeAnthropicConversation(req)
	if err != nil {
		return nil, err
	}
	return encodeCodexRequest(model, conv, stream)
}

func convertAnthropicResponseToCodexNonStream(_ context.Context, model string, rawReq, _ []byte, rawJSON []byte) ([]byte, error) {
	var resp anthropicMessagesResponse
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	output, err := codexOutputItemsFromAnthropicBlocks(resp.Content)
	if err != nil {
		return nil, err
	}
	output = restoreAnthropicCodexToolItems(output, codexOpenAIToolRoutes(rawReq))
	out := map[string]any{
		"id":     "resp-proxy",
		"object": "response",
		"status": "completed",
		"model":  coalesceModel(model, resp.Model),
		"output": output,
	}
	if usage := codexUsageFromAnthropicUsage(resp.Usage); usage != nil {
		out["usage"] = codexUsagePayload(usage)
	}
	return sonic.Marshal(out)
}

func convertCodexResponseToAnthropicNonStream(_ context.Context, model string, rawReq, translatedReq, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	aliases := codexToolAliasesFromRequests(protocol.Anthropic, rawReq, translatedReq)
	message, err := openAIMessageFromCodexOutput(resp["output"], aliases.restore)
	if err != nil {
		return nil, err
	}
	blocks, err := anthropicBlocksFromOpenAIMessage(message)
	if err != nil {
		return nil, err
	}
	stopReason := "end_turn"
	if rawToolCalls, ok := message["tool_calls"].([]map[string]any); ok && len(rawToolCalls) > 0 {
		stopReason = "tool_use"
	} else if rawToolCalls, ok := message["tool_calls"].([]any); ok && len(rawToolCalls) > 0 {
		stopReason = "tool_use"
	}
	out := map[string]any{
		"id":          "msg-proxy",
		"type":        "message",
		"role":        "assistant",
		"content":     blocks,
		"model":       coalesceModel(model, resp["model"]),
		"stop_reason": stopReason,
	}
	if usage := codexUsageFromMap(resp["usage"]); usage != nil {
		inputTokens := max(usage.inputTokens-usage.cachedTokens, 0)

		out["usage"] = map[string]any{
			"input_tokens":                inputTokens,
			"output_tokens":               usage.outputTokens,
			"cache_read_input_tokens":     usage.cachedTokens,
			"cache_creation_input_tokens": usage.cacheCreationInputTokens,
			"reasoning_tokens":            usage.reasoningTokens,
		}
	}
	return sonic.Marshal(out)
}

func convertAnthropicResponseToCodexStream(_ context.Context, model string, rawReq, _ []byte, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &anthropicToCodexStreamState{model: model, responseID: "resp-proxy", toolRoutes: codexOpenAIToolRoutes(rawReq)}
	}
	st := (*param).(*anthropicToCodexStreamState)
	if st.model == "" {
		st.model = model
	}

	raw := strings.TrimSpace(string(rawJSON))
	if raw == "" {
		return nil, nil
	}
	eventType, line := parseSSEEventBlock(raw)
	if line == "" {
		return nil, nil
	}
	var payload map[string]any
	if err := sonic.Unmarshal([]byte(line), &payload); err != nil {
		return nil, err
	}
	if eventType == "message_start" {
		if message, _ := payload["message"].(map[string]any); message != nil {
			if messageModel := stringValue(message["model"]); messageModel != "" {
				st.model = messageModel
			}
			if messageID := stringValue(message["id"]); messageID != "" {
				st.responseID = messageID
			}
			if usage, _ := message["usage"].(map[string]any); usage != nil {
				st.usage.inputTokens = int64Value(usage["input_tokens"])
				st.usage.outputTokens = int64Value(usage["output_tokens"])
				st.usage.cacheReadInputTokens = int64Value(usage["cache_read_input_tokens"])
				st.usage.cacheCreationInputTokens = int64Value(usage["cache_creation_input_tokens"])
				st.usage.reasoningTokens = int64Value(usage["reasoning_tokens"])
				st.usage.totalTokens = st.usage.inputTokens + st.usage.cacheReadInputTokens + st.usage.cacheCreationInputTokens + st.usage.outputTokens
				st.usage.seen = st.usage.inputTokens != 0 || st.usage.outputTokens != 0 || st.usage.cacheReadInputTokens != 0 || st.usage.cacheCreationInputTokens != 0 || st.usage.reasoningTokens != 0
			}
		}
		return nil, nil
	}
	if eventType == "message_stop" || func() bool { typ, _ := payload["type"].(string); return typ == "message_stop" }() {
		chunks, err := finishAnthropicCodexText(st)
		if err != nil {
			return nil, err
		}
		done := map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     st.responseID,
				"object": "response",
				"status": "completed",
				"model":  st.model,
			},
		}
		if st.usage.seen {
			done["response"].(map[string]any)["usage"] = codexUsagePayload(&codexUsage{
				inputTokens:              st.usage.inputTokens + st.usage.cacheReadInputTokens + st.usage.cacheCreationInputTokens,
				outputTokens:             st.usage.outputTokens,
				totalTokens:              st.usage.totalTokens,
				cachedTokens:             st.usage.cacheReadInputTokens,
				cacheCreationInputTokens: st.usage.cacheCreationInputTokens,
				reasoningTokens:          st.usage.reasoningTokens,
			})
		}
		body, err := sonic.Marshal(done)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, append([]byte("event: response.completed\ndata: "), append(body, []byte("\n\n")...)...))
		return chunks, nil
	}
	if typ := stringValue(payload["type"]); typ == "content_block_start" {
		if block, _ := payload["content_block"].(map[string]any); block != nil {
			switch stringValue(block["type"]) {
			case "tool_use":
				st.toolID = stringValue(block["id"])
				st.toolName = stringValue(block["name"])
				st.toolInput = block["input"]
				st.toolJSON = ""
				st.toolActive = true
			case "thinking", "redacted_thinking":
				st.reasoningActive = true
				st.reasoningText = ""
				st.reasoningSignature = ""
				st.reasoningData = stringValue(block["data"])
			case "text":
				st.textActive = true
				st.textStarted = false
				st.textValue = ""
				st.textOutputIndex = int(int64Value(payload["index"]))
				st.textSequence++
				st.textID = fmt.Sprintf("msg-proxy-%d", st.textSequence)
				if text := stringValue(block["text"]); text != "" {
					return appendAnthropicCodexTextDelta(st, text)
				}
			}
		}
		return nil, nil
	}
	if typ := stringValue(payload["type"]); typ == "content_block_delta" {
		if delta, ok := payload["delta"].(map[string]any); ok {
			if stringValue(delta["type"]) == "input_json_delta" && st.toolActive {
				st.toolJSON += stringValue(delta["partial_json"])
				return nil, nil
			}
			if stringValue(delta["type"]) == "thinking_delta" && st.reasoningActive {
				st.reasoningText += stringValue(delta["thinking"])
				return nil, nil
			}
			if stringValue(delta["type"]) == "signature_delta" && st.reasoningActive {
				st.reasoningSignature += stringValue(delta["signature"])
				return nil, nil
			}
			if text := stringValue(delta["text"]); text != "" {
				st.textActive = true
				return appendAnthropicCodexTextDelta(st, text)
			}
		}
	}
	if typ := stringValue(payload["type"]); typ == "content_block_stop" && st.textActive {
		return finishAnthropicCodexText(st)
	}
	if typ := stringValue(payload["type"]); typ == "content_block_stop" && st.reasoningActive {
		chunk := map[string]any{
			"type": "response.output_item.done",
			"item": codexReasoningItem(st.reasoningText, firstNonEmptyString(map[string]any{
				"signature": st.reasoningSignature,
				"data":      st.reasoningData,
			}, "signature", "data")),
		}
		body, err := sonic.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		st.reasoningActive = false
		st.reasoningText = ""
		st.reasoningSignature = ""
		st.reasoningData = ""
		return [][]byte{append([]byte("event: response.output_item.done\ndata: "), append(body, []byte("\n\n")...)...)}, nil
	}
	if typ := stringValue(payload["type"]); typ == "content_block_stop" && st.toolActive {
		arguments := strings.TrimSpace(st.toolJSON)
		if arguments == "" {
			if raw, err := sonic.Marshal(st.toolInput); err == nil && len(raw) > 0 {
				arguments = string(raw)
			}
		}
		if arguments == "" {
			arguments = "{}"
		}
		route := st.toolRoutes[st.toolName]
		toolItem := codexToolCallItemFromOpenAI(st.toolID, st.toolName, arguments, route)
		chunk := map[string]any{
			"type": "response.output_item.done",
			"item": toolItem,
		}
		body, err := sonic.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		st.toolID = ""
		st.toolName = ""
		st.toolInput = nil
		st.toolJSON = ""
		st.toolActive = false
		return [][]byte{append([]byte("event: response.output_item.done\ndata: "), append(body, []byte("\n\n")...)...)}, nil
	}
	if typ := stringValue(payload["type"]); typ == "message_delta" {
		if usage, _ := payload["usage"].(map[string]any); usage != nil {
			if val := int64Value(usage["input_tokens"]); val != 0 {
				st.usage.inputTokens = val
			}
			if val := int64Value(usage["output_tokens"]); val != 0 {
				st.usage.outputTokens = val
			}
			if val := int64Value(usage["cache_read_input_tokens"]); val != 0 {
				st.usage.cacheReadInputTokens = val
			}
			if val := int64Value(usage["cache_creation_input_tokens"]); val != 0 {
				st.usage.cacheCreationInputTokens = val
			}
			if val := int64Value(usage["reasoning_tokens"]); val != 0 {
				st.usage.reasoningTokens = val
			}
			st.usage.totalTokens = st.usage.inputTokens + st.usage.cacheReadInputTokens + st.usage.cacheCreationInputTokens + st.usage.outputTokens
			st.usage.seen = true
		}
	}
	return nil, nil
}

func finishAnthropicCodexText(st *anthropicToCodexStreamState) ([][]byte, error) {
	if st == nil || !st.textActive || !st.textStarted {
		return nil, nil
	}
	text := st.textValue
	st.textActive = false
	st.textStarted = false
	st.textValue = ""
	textID := st.textID
	st.textID = ""
	outputIndex := st.textOutputIndex
	visible := strings.TrimSpace(text)
	visible = strings.Trim(visible, "\u200b\ufeff")
	if strings.TrimSpace(visible) == "" {
		return nil, nil
	}
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	textDone := map[string]any{
		"type": "response.output_text.done", "item_id": textID, "output_index": outputIndex, "content_index": 0, "text": text,
	}
	partDone := map[string]any{
		"type": "response.content_part.done", "item_id": textID, "output_index": outputIndex, "content_index": 0, "part": part,
	}
	itemDone := map[string]any{
		"type":         "response.output_item.done",
		"output_index": outputIndex,
		"item": map[string]any{
			"id": textID, "type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]any{part},
		},
	}
	textDoneChunk, err := appendCodexSSEEvent(nil, "response.output_text.done", textDone)
	if err != nil {
		return nil, err
	}
	partDoneChunk, err := appendCodexSSEEvent(nil, "response.content_part.done", partDone)
	if err != nil {
		return nil, err
	}
	itemDoneChunk, err := appendCodexSSEEvent(nil, "response.output_item.done", itemDone)
	if err != nil {
		return nil, err
	}
	return [][]byte{textDoneChunk, partDoneChunk, itemDoneChunk}, nil
}

func appendAnthropicCodexTextDelta(st *anthropicToCodexStreamState, text string) ([][]byte, error) {
	if st == nil || text == "" {
		return nil, nil
	}
	st.textValue += text
	chunks := make([][]byte, 0, 3)
	if !st.textStarted {
		st.textStarted = true
		added := map[string]any{
			"type": "response.output_item.added", "output_index": st.textOutputIndex,
			"item": map[string]any{"id": st.textID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
		}
		partAdded := map[string]any{
			"type": "response.content_part.added", "item_id": st.textID, "output_index": st.textOutputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}
		addedChunk, err := appendCodexSSEEvent(nil, "response.output_item.added", added)
		if err != nil {
			return nil, err
		}
		partChunk, err := appendCodexSSEEvent(nil, "response.content_part.added", partAdded)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, addedChunk, partChunk)
	}
	delta := map[string]any{
		"type": "response.output_text.delta", "item_id": st.textID, "output_index": st.textOutputIndex, "content_index": 0, "delta": text,
	}
	deltaChunk, err := appendCodexSSEEvent(nil, "response.output_text.delta", delta)
	if err != nil {
		return nil, err
	}
	return append(chunks, deltaChunk), nil
}

func restoreAnthropicCodexToolItems(items []map[string]any, routes map[string]codexOpenAIToolRoute) []map[string]any {
	for i, item := range items {
		if normalizeRole(stringValue(item["type"])) != "function_call" {
			continue
		}
		name := stringValue(item["name"])
		arguments := stringValue(item["arguments"])
		items[i] = codexToolCallItemFromOpenAI(firstNonEmptyString(item, "call_id", "id"), name, arguments, routes[name])
	}
	return items
}

func convertCodexResponseToAnthropicStream(_ context.Context, model string, rawReq, translatedReq, rawJSON []byte, param *any) ([][]byte, error) {
	st := initCodexToAnthropicStreamState(param, model)

	raw := strings.TrimSpace(string(rawJSON))
	if raw == "" {
		return nil, nil
	}
	eventType, line := parseSSEEventBlock(raw)
	if line == "" {
		return nil, nil
	}

	var payload map[string]any
	if err := sonic.Unmarshal([]byte(line), &payload); err != nil {
		return nil, err
	}
	applyCodexResponsePayload(st, payload)

	eventName := eventType
	if eventName == "" {
		eventName = stringValue(payload["type"])
	}
	switch eventName {
	case "response.output_item.added":
		return handleCodexOutputItemAdded(st, payload)
	case "response.reasoning_summary_part.added":
		return codexAnthropicStartThinkingBlock(st)
	case "response.reasoning_summary_text.delta":
		return handleCodexReasoningSummaryDelta(st, payload)
	case "response.reasoning_summary_part.done":
		st.thinkingStopPending = true
		return nil, nil
	case "response.output_item.done":
		return handleCodexOutputItemDone(st, payload, rawReq, translatedReq)
	case "response.output_text.delta":
		return handleCodexOutputTextDelta(st, payload)
	case "response.completed":
		return handleCodexResponseCompleted(st)
	}
	return nil, nil
}

func initCodexToAnthropicStreamState(param *any, model string) *codexToAnthropicStreamState {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &codexToAnthropicStreamState{model: model}
	}
	st := (*param).(*codexToAnthropicStreamState)
	if st.model == "" {
		st.model = model
	}
	return st
}

func applyCodexResponsePayload(st *codexToAnthropicStreamState, payload map[string]any) {
	response, _ := payload["response"].(map[string]any)
	if response == nil {
		return
	}
	if responseModel := stringValue(response["model"]); responseModel != "" {
		st.model = responseModel
	}
	if id := stringValue(response["id"]); id != "" && st.responseID == "" {
		st.responseID = id
	}
	if usage := codexUsageFromMap(response["usage"]); usage != nil {
		st.usage.inputTokens = usage.inputTokens
		st.usage.outputTokens = usage.outputTokens
		st.usage.cachedTokens = usage.cachedTokens
		st.usage.cacheCreationInputTokens = usage.cacheCreationInputTokens
		st.usage.reasoningTokens = usage.reasoningTokens
		st.usage.seen = true
	}
}

func handleCodexOutputItemAdded(st *codexToAnthropicStreamState, payload map[string]any) ([][]byte, error) {
	if item, _ := payload["item"].(map[string]any); item != nil && stringValue(item["type"]) == "reasoning" {
		if signature := firstNonEmptyString(item, "encrypted_content", "signature"); signature != "" {
			st.thinkingSignature = signature
		}
	}
	return nil, nil
}

func handleCodexReasoningSummaryDelta(st *codexToAnthropicStreamState, payload map[string]any) ([][]byte, error) {
	deltaText := stringValue(payload["delta"])
	if deltaText == "" {
		return nil, nil
	}
	outputs, err := codexAnthropicStartThinkingBlock(st)
	if err != nil {
		return nil, err
	}
	thinkingDelta, err := marshalEventSSE("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": st.blockIndex,
		"delta": map[string]any{
			"type":     "thinking_delta",
			"thinking": deltaText,
		},
	})
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, thinkingDelta)
	st.lastBlock = "thinking"
	return outputs, nil
}

func handleCodexOutputItemDone(st *codexToAnthropicStreamState, payload map[string]any, rawReq, translatedReq []byte) ([][]byte, error) {
	item, _ := payload["item"].(map[string]any)
	if item == nil {
		return nil, nil
	}
	switch stringValue(item["type"]) {
	case "message":
		return handleCodexMessageItem(st, item)
	case "reasoning":
		return handleCodexReasoningItem(st, item)
	case "function_call":
		return handleCodexFunctionCallItem(st, item, rawReq, translatedReq)
	default:
		return nil, nil
	}
}

func handleCodexMessageItem(st *codexToAnthropicStreamState, item map[string]any) ([][]byte, error) {
	if st.hasTextDelta {
		return nil, nil
	}
	parts, err := extractCodexContentParts(item["content"])
	if err != nil {
		return nil, err
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Kind == partKindText && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	if len(texts) == 0 {
		return nil, nil
	}
	outputs, err := codexAnthropicEnsureTextBlockOpen(st)
	if err != nil {
		return nil, err
	}
	for _, text := range texts {
		deltaChunk, err := marshalEventSSE("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": st.blockIndex,
			"delta": map[string]any{
				"type": "text_delta",
				"text": text,
			},
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, deltaChunk)
	}
	st.hasTextDelta = true
	st.lastBlock = "text"
	return outputs, nil
}

func handleCodexReasoningItem(st *codexToAnthropicStreamState, item map[string]any) ([][]byte, error) {
	if st.thinkingBlockOpen || st.thinkingStopPending {
		return codexAnthropicFinalizeThinking(st)
	}
	outputs := make([][]byte, 0, 6)
	if !st.started {
		msgStart, err := codexAnthropicMessageStartChunk(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, msgStart)
		st.started = true
	}
	if st.openBlock {
		blockStop, err := codexAnthropicCloseOpenBlock(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, blockStop)
	}
	if encContent := stringValue(item["encrypted_content"]); encContent != "" {
		// redacted_thinking 块只发 start+stop，无 delta
		blockStart, err := marshalEventSSE("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": st.blockIndex,
			"content_block": map[string]any{
				"type": "redacted_thinking",
				"data": "",
			},
		})
		if err != nil {
			return nil, err
		}
		blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": st.blockIndex,
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, blockStart, blockStop)
		st.lastBlock = "redacted_thinking"
		st.blockIndex++
		return outputs, nil
	}
	summaryText := codexExtractSummaryText(item["summary"])
	if summaryText == "" {
		return outputs, nil
	}
	blockStart, err := marshalEventSSE("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": st.blockIndex,
		"content_block": map[string]any{
			"type":     "thinking",
			"thinking": "",
		},
	})
	if err != nil {
		return nil, err
	}
	thinkingDelta, err := marshalEventSSE("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": st.blockIndex,
		"delta": map[string]any{
			"type":     "thinking_delta",
			"thinking": summaryText,
		},
	})
	if err != nil {
		return nil, err
	}
	blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": st.blockIndex,
	})
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, blockStart, thinkingDelta, blockStop)
	st.lastBlock = "thinking"
	st.blockIndex++
	return outputs, nil
}

func codexExtractSummaryText(raw any) string {
	summaries, ok := raw.([]any)
	if !ok {
		return ""
	}
	out := ""
	for _, s := range summaries {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if stringValue(sm["type"]) == "summary_text" {
			out += stringValue(sm["text"])
		}
	}
	return out
}

func handleCodexFunctionCallItem(st *codexToAnthropicStreamState, item map[string]any, rawReq, translatedReq []byte) ([][]byte, error) {
	outputs := make([][]byte, 0, 6)
	if !st.started {
		msgStart, err := codexAnthropicMessageStartChunk(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, msgStart)
		st.started = true
	}
	if thinkingOutputs, err := codexAnthropicFinalizeThinking(st); err != nil {
		return nil, err
	} else if len(thinkingOutputs) > 0 {
		outputs = append(outputs, thinkingOutputs...)
	}
	if st.openBlock {
		blockStop, err := codexAnthropicCloseOpenBlock(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, blockStop)
	}
	call, err := decodeCodexToolCall(item)
	if err != nil {
		return nil, err
	}
	call.Name = st.restoreToolName(rawReq, translatedReq, call.Name)
	partialJSON := "{}"
	if len(call.Arguments) > 0 {
		partialJSON = string(call.Arguments)
	}
	blockStart, err := marshalEventSSE("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": st.blockIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Name,
			"input": map[string]any{},
		},
	})
	if err != nil {
		return nil, err
	}
	inputDelta, err := marshalEventSSE("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": st.blockIndex,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": partialJSON,
		},
	})
	if err != nil {
		return nil, err
	}
	blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": st.blockIndex,
	})
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, blockStart, inputDelta, blockStop)
	st.lastBlock = "tool_use"
	st.blockIndex++
	return outputs, nil
}

func handleCodexOutputTextDelta(st *codexToAnthropicStreamState, payload map[string]any) ([][]byte, error) {
	content := stringValue(payload["delta"])
	if content == "" {
		return nil, nil
	}
	outputs, err := codexAnthropicEnsureTextBlockOpen(st)
	if err != nil {
		return nil, err
	}
	deltaChunk, err := marshalEventSSE("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": st.blockIndex,
		"delta": map[string]any{
			"type": "text_delta",
			"text": content,
		},
	})
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, deltaChunk)
	st.hasTextDelta = true
	st.lastBlock = "text"
	return outputs, nil
}

func handleCodexResponseCompleted(st *codexToAnthropicStreamState) ([][]byte, error) {
	outputs := make([][]byte, 0, 6)
	if thinkingOutputs, err := codexAnthropicFinalizeThinking(st); err != nil {
		return nil, err
	} else if len(thinkingOutputs) > 0 {
		outputs = append(outputs, thinkingOutputs...)
	}
	stopOutputs, err := codexAnthropicStopChunks(st)
	if err != nil {
		return nil, err
	}
	return append(outputs, stopOutputs...), nil
}

// codexAnthropicMessageStartChunk emits only the message_start SSE frame.
// Callers are responsible for emitting the appropriate content_block_start
// depending on whether the first block is text, thinking, or tool_use.
func codexAnthropicMessageStartChunk(st *codexToAnthropicStreamState) ([]byte, error) {
	return anthropicMessageStartSSE(st.model, st.responseID, codexAnthropicStartUsage(st))
}

func codexAnthropicStartUsage(st *codexToAnthropicStreamState) anthropicStreamUsage {
	if st == nil {
		return anthropicStreamUsage{}
	}
	return anthropicStreamUsageFromCounts(st.usage.inputTokens, 0, st.usage.cachedTokens, st.usage.cacheCreationInputTokens, 0, st.usage.seen)
}

func codexAnthropicDeltaUsage(st *codexToAnthropicStreamState) anthropicStreamUsage {
	if st == nil {
		return anthropicStreamUsage{}
	}
	return anthropicStreamUsageFromCounts(
		st.usage.inputTokens,
		st.usage.outputTokens,
		st.usage.cachedTokens,
		st.usage.cacheCreationInputTokens,
		st.usage.reasoningTokens,
		st.usage.seen,
	)
}

// codexAnthropicStartChunks emits message_start + text content_block_start.
// Used when the response begins directly with text (no leading reasoning/tool blocks).
func codexAnthropicStartChunks(st *codexToAnthropicStreamState) ([][]byte, error) {
	msgStart, err := codexAnthropicMessageStartChunk(st)
	if err != nil {
		return nil, err
	}
	blockStart, err := anthropicTextBlockStartSSE(st.blockIndex)
	if err != nil {
		return nil, err
	}
	return [][]byte{msgStart, blockStart}, nil
}

func codexAnthropicStopChunks(st *codexToAnthropicStreamState) ([][]byte, error) {
	outputs := make([][]byte, 0, 5)
	if st != nil && !st.started {
		// No content was emitted at all: synthesize message_start + empty text block.
		start, err := codexAnthropicStartChunks(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, start...)
		st.started = true
		st.openBlock = true
		st.lastBlock = "text"
	}
	if st != nil && st.openBlock {
		blockStop, err := codexAnthropicCloseOpenBlock(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, blockStop)
	}
	messageDelta, err := marshalEventSSE("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   codexAnthropicStopReason(st),
			"stop_sequence": nil,
		},
		"usage": anthropicStreamUsagePayload(codexAnthropicDeltaUsage(st)),
	})
	if err != nil {
		return nil, err
	}
	messageStop, err := marshalEventSSE("message_stop", map[string]any{"type": "message_stop"})
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, messageDelta, messageStop)
	if st != nil {
		st.started = false
		st.openBlock = false
		st.lastBlock = ""
		st.hasTextDelta = false
		st.thinkingBlockOpen = false
		st.thinkingStopPending = false
		st.thinkingSignature = ""
	}
	return outputs, nil
}

func codexAnthropicCloseOpenBlock(st *codexToAnthropicStreamState) ([]byte, error) {
	blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": st.blockIndex,
	})
	if err != nil {
		return nil, err
	}
	st.openBlock = false
	st.blockIndex++
	return blockStop, nil
}

func codexAnthropicStopReason(st *codexToAnthropicStreamState) string {
	if st != nil && st.lastBlock == "tool_use" {
		return "tool_use"
	}
	return "end_turn"
}

func (st *codexToAnthropicStreamState) restoreToolName(rawReq, translatedReq []byte, name string) string {
	if st.toolNameMap == nil {
		st.toolNameMap = codexToolAliasesFromRequests(protocol.Anthropic, rawReq, translatedReq).ShortToOriginal
	}
	if original := st.toolNameMap[name]; original != "" {
		return original
	}
	return name
}

func codexAnthropicEnsureTextBlockOpen(st *codexToAnthropicStreamState) ([][]byte, error) {
	outputs := make([][]byte, 0, 3)
	if thinkingOutputs, err := codexAnthropicFinalizeThinking(st); err != nil {
		return nil, err
	} else if len(thinkingOutputs) > 0 {
		outputs = append(outputs, thinkingOutputs...)
	}
	if st != nil && !st.started {
		msgStart, err := codexAnthropicMessageStartChunk(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, msgStart)
		st.started = true
	}
	if st != nil && !st.openBlock {
		textBlockStart, err := marshalEventSSE("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": st.blockIndex,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, textBlockStart)
		st.openBlock = true
		st.lastBlock = "text"
	}
	return outputs, nil
}

func codexAnthropicStartThinkingBlock(st *codexToAnthropicStreamState) ([][]byte, error) {
	outputs := make([][]byte, 0, 4)
	if thinkingOutputs, err := codexAnthropicFinalizeThinking(st); err != nil {
		return nil, err
	} else if len(thinkingOutputs) > 0 {
		outputs = append(outputs, thinkingOutputs...)
	}
	if st != nil && !st.started {
		msgStart, err := codexAnthropicMessageStartChunk(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, msgStart)
		st.started = true
	}
	if st != nil && st.openBlock {
		blockStop, err := codexAnthropicCloseOpenBlock(st)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, blockStop)
	}
	if st != nil && !st.thinkingBlockOpen {
		blockStart, err := marshalEventSSE("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": st.blockIndex,
			"content_block": map[string]any{
				"type":     "thinking",
				"thinking": "",
			},
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, blockStart)
		st.thinkingBlockOpen = true
		st.thinkingStopPending = false
		st.lastBlock = "thinking"
	}
	return outputs, nil
}

func codexAnthropicFinalizeThinking(st *codexToAnthropicStreamState) ([][]byte, error) {
	if st == nil || !st.thinkingBlockOpen || !st.thinkingStopPending {
		return nil, nil
	}
	outputs := make([][]byte, 0, 2)
	if st.thinkingSignature != "" {
		signatureDelta, err := marshalEventSSE("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": st.blockIndex,
			"delta": map[string]any{
				"type":      "signature_delta",
				"signature": st.thinkingSignature,
			},
		})
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, signatureDelta)
	}
	blockStop, err := marshalEventSSE("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": st.blockIndex,
	})
	if err != nil {
		return nil, err
	}
	outputs = append(outputs, blockStop)
	st.thinkingBlockOpen = false
	st.thinkingStopPending = false
	st.thinkingSignature = ""
	st.lastBlock = "thinking"
	st.blockIndex++
	return outputs, nil
}
