package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
)

type pendingToolCall struct {
	id          string
	name        string
	arguments   string
	outputIndex int
	sent        int
	started     bool
}

type openAIToCodexStreamState struct {
	created      int64
	sequence     int
	started      bool
	serviceTier  any
	model        string
	responseID   string
	finishReason string
	finished     bool
	output       map[int]map[string]any
	toolRoutes   map[string]codexOpenAIToolRoute
	usage        struct {
		promptTokens             int64
		completionTokens         int64
		totalTokens              int64
		cachedTokens             int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
	reasoningStarted   bool
	reasoningIndex     int
	refusal            string
	refusalIndex       int
	refusalStarted     bool
	reasoningText      string
	reasoningEncrypted string
	textValue          string
	textStarted        bool
	textOutputIndex    int
	nextOutputIndex    int
	toolCalls          map[int]*pendingToolCall
}

type codexToOpenAIStreamState struct {
	model         string
	responseID    string
	finished      bool
	tools         map[string]*responsesChatToolState
	reasoningSent map[string]bool
	usage         struct {
		inputTokens              int64
		outputTokens             int64
		totalTokens              int64
		cachedTokens             int64
		cacheCreationInputTokens int64
		reasoningTokens          int64
		seen                     bool
	}
	toolCallIndex int
	sawToolCall   bool
	toolNameMap   map[string]string
}

func convertOpenAIRequestToCodex(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req openAIChatRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeOpenAIConversation(req)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeCodexRequest(model, conv, stream)
	if err != nil {
		return nil, err
	}
	return preserveOpenAIResponseRequestFields(rawJSON, encoded, true, stream)
}

func convertCodexRequestToOpenAI(model string, rawJSON []byte, stream bool) ([]byte, error) {
	var req codexRequest
	if err := sonic.Unmarshal(rawJSON, &req); err != nil {
		return nil, err
	}
	conv, err := normalizeCodexConversation(req)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeOpenAIRequest(model, conv, stream)
	if err != nil {
		return nil, err
	}
	return preserveOpenAIResponseRequestFields(rawJSON, encoded, false, stream)
}

func convertOpenAIResponseToCodexNonStream(_ context.Context, model string, rawReq, _ []byte, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	output, err := codexOutputItemsFromOpenAIResponse(resp, codexOpenAIToolRoutes(rawReq))
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"id":     "resp_" + uuid.NewString(),
		"object": "response",
		"status": "completed",
		"model":  coalesceModel(model, resp["model"]),
		"output": output,
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		setResponsesCompletionStatus(out, stringValue(choice["finish_reason"]))
	}
	copyServiceTier(resp, out)
	if usage := openAIUsageFromMap(resp["usage"]); usage != nil {
		out["usage"] = codexUsagePayload(&codexUsage{
			inputTokens:              usage.promptTokens,
			outputTokens:             usage.completionTokens,
			totalTokens:              usage.totalTokens,
			cachedTokens:             usage.cachedTokens,
			cacheCreationInputTokens: usage.cacheCreationInputTokens,
			reasoningTokens:          usage.reasoningTokens,
		})
	}
	return sonic.Marshal(out)
}

func convertCodexResponseToOpenAINonStream(_ context.Context, model string, rawReq, translatedReq, rawJSON []byte) ([]byte, error) {
	var resp map[string]any
	if err := sonic.Unmarshal(rawJSON, &resp); err != nil {
		return nil, err
	}
	aliases := codexToolAliasesFromRequests(protocol.OpenAI, rawReq, translatedReq)
	message, err := openAIMessageFromCodexOutput(resp["output"], aliases.restore)
	if err != nil {
		return nil, err
	}
	finishReason := "stop"
	if rawToolCalls, ok := message["tool_calls"].([]map[string]any); ok && len(rawToolCalls) > 0 {
		finishReason = "tool_calls"
	} else if rawToolCalls, ok := message["tool_calls"].([]any); ok && len(rawToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	if reason := responsesFinishReason(resp); reason != "" {
		finishReason = reason
	}
	if stringValue(resp["status"]) == "failed" || resp["error"] != nil {
		return sonic.Marshal(map[string]any{"error": resp["error"]})
	}
	out := map[string]any{
		"id":      "chatcmpl_" + uuid.NewString(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   coalesceModel(model, resp["model"]),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	copyServiceTier(resp, out)
	if usage := codexUsageFromMap(resp["usage"]); usage != nil {
		out["usage"] = openAIUsagePayload(&openAIUsage{
			promptTokens:             usage.inputTokens,
			completionTokens:         usage.outputTokens,
			totalTokens:              usage.totalTokens,
			cachedTokens:             usage.cachedTokens,
			cacheCreationInputTokens: usage.cacheCreationInputTokens,
			reasoningTokens:          usage.reasoningTokens,
		})
	}
	return sonic.Marshal(out)
}

func convertOpenAIResponseToCodexStreamEvent(_ context.Context, model string, rawReq, _ []byte, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &openAIToCodexStreamState{model: model, responseID: "resp_" + uuid.NewString(), created: time.Now().Unix(), output: make(map[int]map[string]any), toolRoutes: codexOpenAIToolRoutes(rawReq)}
	}
	st := (*param).(*openAIToCodexStreamState)
	if st.finished {
		return nil, nil
	}
	if st.model == "" {
		st.model = model
	}

	eventType, line := parseSSEEventBlockOrRaw(string(rawJSON))
	if line == "" {
		return nil, nil
	}
	if isCodexResponseEventType(eventType) {
		return [][]byte{rawJSON}, nil
	}
	if line == "[DONE]" {
		chunks, err := finishOpenAICodexReasoning(st)
		if err != nil {
			return nil, err
		}
		textChunks, err := finishOpenAICodexText(st)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, textChunks...)
		indices := make([]int, 0, len(st.toolCalls))
		for idx := range st.toolCalls {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		for _, idx := range indices {
			tc := st.toolCalls[idx]
			toolItem := codexToolCallItemFromOpenAI(tc.id, tc.name, tc.arguments, st.toolRoute(tc.name))
			toolItem["id"] = st.toolItemID(tc)
			toolItem["status"] = "completed"
			if tc.started {
				done, err := appendCodexSSEEvent(nil, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": st.toolItemID(tc), "output_index": tc.outputIndex, "arguments": tc.arguments})
				if err != nil {
					return nil, err
				}
				chunks = append(chunks, done)
			}
			st.output[tc.outputIndex] = toolItem
			body, err := appendCodexSSEEvent(nil, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": tc.outputIndex, "item": toolItem})
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, body)
		}
		response := map[string]any{
			"id":     st.responseID,
			"object": "response",
			"status": "completed",
			"model":  st.model,
		}
		if st.usage.seen {
			response["usage"] = codexUsagePayload(&codexUsage{
				inputTokens:              st.usage.promptTokens,
				outputTokens:             st.usage.completionTokens,
				totalTokens:              st.usage.totalTokens,
				cachedTokens:             st.usage.cachedTokens,
				cacheCreationInputTokens: st.usage.cacheCreationInputTokens,
				reasoningTokens:          st.usage.reasoningTokens,
			})
		}
		if st.refusalStarted {
			events, err := st.refusalEvents("", true)
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, events...)
		}
		output := make([]map[string]any, 0, len(st.output))
		for i := 0; i < st.nextOutputIndex; i++ {
			if item := st.output[i]; item != nil {
				output = append(output, item)
			}
		}
		response["output"] = output
		response["created_at"] = st.created
		if st.serviceTier != nil {
			response["service_tier"] = st.serviceTier
		}
		eventName := setResponsesCompletionStatus(response, st.finishReason)
		st.finished = true
		done := map[string]any{"type": eventName, "response": response}
		body, err := sonic.Marshal(done)
		if err != nil {
			return nil, err
		}
		completed := append([]byte("event: "+eventName+"\ndata: "), append(body, []byte("\n\n")...)...)
		chunks = append(chunks, completed)
		return chunks, nil
	}

	var chunk map[string]any
	if err := sonic.Unmarshal([]byte(line), &chunk); err != nil {
		return nil, err
	}
	if eventName := stringValue(chunk["type"]); isCodexResponseEventType(eventName) {
		return [][]byte{marshalRawCodexEvent(eventName, line)}, nil
	}
	if failure := chunk["error"]; failure != nil {
		st.finished = true
		body, err := appendCodexSSEEvent(nil, "response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"id": st.responseID, "status": "failed", "error": failure, "output": []any{}}})
		return [][]byte{body}, err
	}
	if chunkModel := stringValue(chunk["model"]); st.model == "" && chunkModel != "" {
		st.model = chunkModel
	}
	if tier, ok := chunk["service_tier"]; ok {
		st.serviceTier = tier
	}
	if usage := openAIUsageFromMap(chunk["usage"]); usage != nil {
		st.usage.promptTokens = usage.promptTokens
		st.usage.completionTokens = usage.completionTokens
		st.usage.totalTokens = usage.totalTokens
		st.usage.cachedTokens = usage.cachedTokens
		st.usage.cacheCreationInputTokens = usage.cacheCreationInputTokens
		st.usage.reasoningTokens = usage.reasoningTokens
		st.usage.seen = true
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	if reason := stringValue(choice["finish_reason"]); reason != "" {
		st.finishReason = reason
	}
	var chunks [][]byte
	delta, _ := choice["delta"].(map[string]any)
	content := stringValue(delta["content"])
	if refusal := stringValue(delta["refusal"]); refusal != "" {
		events, err := st.refusalEvents(refusal, false)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, events...)
	}
	if reasoning := stringValue(delta["reasoning_content"]); reasoning != "" {
		reasoningChunks, err := st.reasoningDelta(reasoning)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, reasoningChunks...)
	}
	if reasoning, _ := delta["reasoning"].(map[string]any); reasoning != nil {
		if encrypted := stringValue(reasoning["encrypted_content"]); encrypted != "" {
			st.reasoningEncrypted = encrypted
		}
	}
	// 累积增量 tool_calls（按 index 合并 id/name/arguments）
	if rawCalls, ok := delta["tool_calls"].([]any); ok && len(rawCalls) > 0 {
		if st.toolCalls == nil {
			st.toolCalls = make(map[int]*pendingToolCall)
		}
		for _, raw := range rawCalls {
			tc, _ := raw.(map[string]any)
			if tc == nil {
				continue
			}
			idx := int(int64Value(tc["index"]))
			if _, exists := st.toolCalls[idx]; !exists {
				st.toolCalls[idx] = &pendingToolCall{outputIndex: st.nextOutputIndex}
				st.nextOutputIndex++
			}
			p := st.toolCalls[idx]
			if id := stringValue(tc["id"]); id != "" {
				p.id = id
			}
			if fn, ok := tc["function"].(map[string]any); ok {
				if name := stringValue(fn["name"]); name != "" {
					p.name = name
				}
				if args := stringValue(fn["arguments"]); args != "" {
					p.arguments += args
				}
			}
			toolChunks, err := st.toolDeltas(p)
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, toolChunks...)
		}
	}
	if content == "" {
		return chunks, nil
	}
	reasoningChunks, err := finishOpenAICodexReasoning(st)
	if err != nil {
		return nil, err
	}
	chunks = append(chunks, reasoningChunks...)
	st.textValue += content
	if !st.textStarted {
		st.textStarted = true
		st.textOutputIndex = st.nextOutputIndex
		st.nextOutputIndex++
		added := map[string]any{
			"type": "response.output_item.added", "output_index": st.textOutputIndex,
			"item": map[string]any{"id": st.responseID + "_msg", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
		}
		partAdded := map[string]any{
			"type": "response.content_part.added", "item_id": st.responseID + "_msg", "output_index": st.textOutputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}
		var err error
		chunk, err := appendCodexSSEEvent(nil, "response.output_item.added", added)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
		chunk, err = appendCodexSSEEvent(nil, "response.content_part.added", partAdded)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	outputDelta := map[string]any{
		"type": "response.output_text.delta", "item_id": st.responseID + "_msg", "output_index": st.textOutputIndex, "content_index": 0, "delta": content,
	}
	deltaChunk, err := appendCodexSSEEvent(nil, "response.output_text.delta", outputDelta)
	if err != nil {
		return nil, err
	}
	return append(chunks, deltaChunk), nil
}

func finishOpenAICodexText(st *openAIToCodexStreamState) ([][]byte, error) {
	if st == nil || !st.textStarted {
		return nil, nil
	}
	value := st.textValue
	st.textValue = ""
	st.textStarted = false
	visible := strings.Trim(strings.TrimSpace(value), "\u200b\ufeff")
	if strings.TrimSpace(visible) == "" {
		return nil, nil
	}
	textDone := map[string]any{
		"type": "response.output_text.done", "item_id": st.responseID + "_msg", "output_index": st.textOutputIndex, "content_index": 0, "text": value,
	}
	part := map[string]any{"type": "output_text", "text": value, "annotations": []any{}}
	partDone := map[string]any{
		"type": "response.content_part.done", "item_id": st.responseID + "_msg", "output_index": st.textOutputIndex, "content_index": 0, "part": part,
	}
	itemDone := map[string]any{
		"type":         "response.output_item.done",
		"output_index": st.textOutputIndex,
		"item": map[string]any{
			"id": st.responseID + "_msg", "type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]any{part},
		},
	}
	st.output[st.textOutputIndex] = itemDone["item"].(map[string]any)
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

func finishOpenAICodexReasoning(st *openAIToCodexStreamState) ([][]byte, error) {
	if st == nil || (st.reasoningText == "" && st.reasoningEncrypted == "") {
		return nil, nil
	}
	text := st.reasoningText
	encrypted := st.reasoningEncrypted
	st.reasoningText = ""
	st.reasoningEncrypted = ""
	var chunks [][]byte
	if !st.reasoningStarted {
		started, err := st.reasoningDelta("")
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, started...)
	}
	outputIndex := st.reasoningIndex
	itemID := fmt.Sprintf("%s_rs_%d", st.responseID, outputIndex)
	st.reasoningStarted = false
	summary := make([]map[string]any, 0, 1)
	if text != "" {
		part := map[string]any{"type": "summary_text", "text": text}
		summary = append(summary, part)
		for _, event := range []struct {
			name    string
			payload map[string]any
		}{
			{"response.reasoning_summary_text.done", map[string]any{"type": "response.reasoning_summary_text.done", "item_id": itemID, "output_index": outputIndex, "summary_index": 0, "text": text}},
			{"response.reasoning_summary_part.done", map[string]any{"type": "response.reasoning_summary_part.done", "item_id": itemID, "output_index": outputIndex, "summary_index": 0, "part": part}},
		} {
			chunk, marshalErr := appendCodexSSEEvent(nil, event.name, event.payload)
			if marshalErr != nil {
				return nil, marshalErr
			}
			chunks = append(chunks, chunk)
		}
	}
	doneItem := codexReasoningItem(text, encrypted)
	doneItem["id"] = itemID
	doneItem["status"] = "completed"
	doneItem["summary"] = summary
	st.output[outputIndex] = doneItem
	done, err := appendCodexSSEEvent(nil, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": outputIndex, "item": doneItem,
	})
	if err != nil {
		return nil, err
	}
	return append(chunks, done), nil
}

func appendCodexSSEEvent(dst []byte, eventType string, payload map[string]any) ([]byte, error) {
	body, err := sonic.Marshal(payload)
	if err != nil {
		return nil, err
	}
	dst = append(dst, []byte("event: "+eventType+"\ndata: ")...)
	dst = append(dst, body...)
	return append(dst, '\n', '\n'), nil
}

func convertCodexResponseToOpenAIStream(_ context.Context, model string, rawReq, translatedReq, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var local any
		param = &local
	}
	if *param == nil {
		*param = &codexToOpenAIStreamState{model: model, responseID: "chatcmpl_" + uuid.NewString(), tools: make(map[string]*responsesChatToolState), reasoningSent: make(map[string]bool)}
	}
	st := (*param).(*codexToOpenAIStreamState)
	if st.finished {
		return nil, nil
	}
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
	if typ := stringValue(payload["type"]); typ != "" {
		eventType = typ
	}
	if chunks, handled, err := st.convertIncrementalEvent(eventType, payload, rawReq, translatedReq); handled || err != nil {
		return chunks, err
	}
	if response, ok := payload["response"].(map[string]any); ok {
		if responseModel := stringValue(response["model"]); st.model == "" && responseModel != "" {
			st.model = responseModel
		}
		if usage := codexUsageFromMap(response["usage"]); usage != nil {
			st.usage.inputTokens = usage.inputTokens
			st.usage.outputTokens = usage.outputTokens
			st.usage.totalTokens = usage.totalTokens
			st.usage.cachedTokens = usage.cachedTokens
			st.usage.cacheCreationInputTokens = usage.cacheCreationInputTokens
			st.usage.reasoningTokens = usage.reasoningTokens
			st.usage.seen = true
		}
	}
	if eventType == "response.completed" || eventType == "response.incomplete" {
		st.finished = true
		finishReason := "stop"
		if st.sawToolCall {
			finishReason = "tool_calls"
		}
		response, _ := payload["response"].(map[string]any)
		if reason := responsesFinishReason(response); reason != "" {
			finishReason = reason
		}
		chunk := map[string]any{
			"id":      st.responseID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   st.model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finishReason,
			}},
		}
		if st.usage.seen {
			chunk["usage"] = openAIUsagePayload(&openAIUsage{
				promptTokens:             st.usage.inputTokens,
				completionTokens:         st.usage.outputTokens,
				totalTokens:              st.usage.totalTokens,
				cachedTokens:             st.usage.cachedTokens,
				cacheCreationInputTokens: st.usage.cacheCreationInputTokens,
				reasoningTokens:          st.usage.reasoningTokens,
			})
		}
		body, err := sonic.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		return [][]byte{
			append([]byte("data: "), append(body, []byte("\n\n")...)...),
			[]byte("data: [DONE]\n\n"),
		}, nil
	}
	if eventType == "response.output_text.delta" || stringValue(payload["type"]) == "response.output_text.delta" {
		delta := stringValue(payload["delta"])
		if delta == "" {
			return nil, nil
		}
		chunk := map[string]any{
			"id":      st.responseID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   st.model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"content": delta},
			}},
		}
		body, err := sonic.Marshal(chunk)
		if err != nil {
			return nil, err
		}
		return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
	}
	if eventType == "response.output_item.done" || stringValue(payload["type"]) == "response.output_item.done" {
		item, _ := payload["item"].(map[string]any)
		itemType := stringValue(item["type"])
		switch {
		case normalizeRole(itemType) == "reasoning":
			text := extractCodexReasoningText(item)
			if text == "" {
				return nil, nil
			}
			chunk := map[string]any{
				"id":      st.responseID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   st.model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{"reasoning_content": text},
				}},
			}
			body, err := sonic.Marshal(chunk)
			if err != nil {
				return nil, err
			}
			return [][]byte{append([]byte("data: "), append(body, []byte("\n\n")...)...)}, nil
		default:
			return nil, nil
		}
	}
	return nil, nil
}

type openAIUsage struct {
	promptTokens             int64
	completionTokens         int64
	totalTokens              int64
	cachedTokens             int64
	cacheCreationInputTokens int64
	reasoningTokens          int64
}

type codexUsage struct {
	inputTokens              int64
	outputTokens             int64
	totalTokens              int64
	cachedTokens             int64
	cacheCreationInputTokens int64
	reasoningTokens          int64
}

func codexOutputItemsFromOpenAIResponse(resp map[string]any, toolRoutes map[string]codexOpenAIToolRoute) ([]map[string]any, error) {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if len(message) == 0 {
		return nil, nil
	}
	items := make([]map[string]any, 0)
	parts, err := extractOpenAIContentParts(message["content"])
	if err != nil {
		return nil, err
	}
	textContent := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		item, err := encodeCodexOutputContentPart(part)
		if err != nil {
			return nil, err
		}
		if item != nil {
			textContent = append(textContent, item)
		}
	}
	if refusal := stringValue(message["refusal"]); refusal != "" {
		textContent = append(textContent, map[string]any{"type": "refusal", "refusal": refusal})
	}
	if len(textContent) > 0 {
		items = append(items, map[string]any{"type": "message", "role": "assistant", "content": textContent})
	}
	var toolCalls []openAIChatToolCall
	if rawCalls, ok := message["tool_calls"]; ok {
		bytes, err := sonic.Marshal(rawCalls)
		if err != nil {
			return nil, err
		}
		if err := sonic.Unmarshal(bytes, &toolCalls); err != nil {
			return nil, err
		}
	}
	for _, call := range toolCalls {
		arguments := strings.TrimSpace(call.Function.Arguments)
		if arguments == "" {
			arguments = "{}"
		}
		items = append(items, codexToolCallItemFromOpenAI(call.ID, call.Function.Name, arguments, toolRoutes[call.Function.Name]))
	}
	items = append(items, codexReasoningItemsFromOpenAIMessage(message)...)
	return items, nil
}

type codexOpenAIToolRoute struct {
	Type      string
	Name      string
	Namespace string
}

func codexOpenAIToolRoutes(rawReq []byte) map[string]codexOpenAIToolRoute {
	if len(rawReq) == 0 {
		return nil
	}
	var req struct {
		Tools json.RawMessage `json:"tools"`
		Input json.RawMessage `json:"input"`
	}
	if err := sonic.Unmarshal(rawReq, &req); err != nil {
		return nil
	}
	tools, err := parseFunctionTools(req.Tools, "codex")
	if err != nil {
		return nil
	}
	var inputItems []map[string]any
	if err := sonic.Unmarshal(req.Input, &inputItems); err == nil {
		for _, item := range inputItems {
			typ := normalizeRole(stringValue(item["type"]))
			if typ != "tool_search_output" && typ != "additional_tools" {
				continue
			}
			if rawTools, ok := item["tools"]; ok {
				encoded, marshalErr := marshalStableJSON(rawTools)
				if marshalErr != nil {
					continue
				}
				loaded, parseErr := parseFunctionTools(encoded, "codex")
				if parseErr == nil {
					tools = append(tools, loaded...)
				}
			}
		}
	}
	conv := conversation{Tools: tools}
	aliases := buildCodexToolAliases(collectOpenAIWireToolNames(conv))
	routes := make(map[string]codexOpenAIToolRoute)
	for _, tool := range conv.Tools {
		base := codexOpenAIWireBaseName(tool.Namespace, tool.Name)
		wire := aliases.shorten(base)
		if wire != "" {
			routes[wire] = codexOpenAIToolRoute{Type: tool.toolType(), Name: tool.Name, Namespace: tool.Namespace}
		}
	}
	if len(routes) == 0 {
		return nil
	}
	return routes
}

func (st *openAIToCodexStreamState) toolRoute(name string) codexOpenAIToolRoute {
	if st == nil {
		return codexOpenAIToolRoute{}
	}
	return st.toolRoutes[name]
}

func codexToolCallItemFromOpenAI(callID, name, arguments string, route codexOpenAIToolRoute) map[string]any {
	realName := name
	if route.Name != "" {
		realName = route.Name
	}
	if route.Type == "custom" {
		return map[string]any{
			"type":    "custom_tool_call",
			"id":      callID,
			"status":  "completed",
			"call_id": callID,
			"name":    realName,
			"input":   customToolInputFromArguments(json.RawMessage(arguments)),
		}
	}
	if route.Type == "tool_search" {
		var parsed any = map[string]any{}
		if err := sonic.UnmarshalString(arguments, &parsed); err != nil {
			parsed = map[string]any{}
		}
		return map[string]any{"type": "tool_search_call", "id": callID, "call_id": callID, "execution": "client", "arguments": parsed, "status": "completed"}
	}
	item := map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      realName,
		"arguments": arguments,
	}
	if route.Namespace != "" {
		item["namespace"] = route.Namespace
	}
	return item
}

func openAIMessageFromCodexOutput(output any, restore func(string) string) (map[string]any, error) {
	if restore == nil {
		restore = func(name string) string { return name }
	}
	items, _ := output.([]any)
	contentParts := make([]map[string]any, 0)
	toolCalls := make([]map[string]any, 0)
	reasoning := make([]map[string]any, 0)
	var refusalBuilder strings.Builder
	var reasoningBuilder strings.Builder
	for i, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unsupported codex output item at index %d", i)
		}
		typ := normalizeRole(stringValue(itemMap["type"]))
		switch typ {
		case "message":
			content := itemMap["content"]
			if rawParts, ok := content.([]any); ok {
				filtered := make([]any, 0, len(rawParts))
				for _, rawPart := range rawParts {
					part, _ := rawPart.(map[string]any)
					if stringValue(part["type"]) == "refusal" {
						refusalBuilder.WriteString(stringValue(part["refusal"]))
						continue
					}
					filtered = append(filtered, rawPart)
				}
				content = filtered
			}
			parts, err := extractCodexContentParts(content)
			if err != nil {
				return nil, err
			}
			for _, part := range parts {
				encoded, err := encodeOpenAIContentPart(part)
				if err != nil {
					return nil, err
				}
				contentParts = append(contentParts, encoded)
			}
		case "function_call":
			call, err := decodeCodexToolCall(itemMap)
			if err != nil {
				return nil, err
			}
			call.Name = restore(call.Name)
			encoded, err := encodeOpenAIToolCall(&call)
			if err != nil {
				return nil, err
			}
			toolCalls = append(toolCalls, encoded)
		case "reasoning":
			text := extractCodexReasoningText(itemMap)
			if text != "" {
				reasoningBuilder.WriteString(text)
			}
			entry := map[string]any{"type": "reasoning"}
			if text != "" {
				entry["text"] = text
			}
			if encrypted := stringValue(itemMap["encrypted_content"]); encrypted != "" {
				entry["encrypted_content"] = encrypted
			}
			reasoning = append(reasoning, entry)
		}
	}
	message := map[string]any{
		"role":    "assistant",
		"content": encodeOpenAIContentValue(contentParts),
	}
	if refusalBuilder.Len() > 0 {
		message["refusal"] = refusalBuilder.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if reasoningBuilder.Len() > 0 {
		message["reasoning_content"] = reasoningBuilder.String()
	}
	if len(reasoning) > 0 {
		message["reasoning"] = reasoning
	}
	return message, nil
}

func (st *codexToOpenAIStreamState) restoreToolName(rawReq, translatedReq []byte, name string) string {
	if st.toolNameMap == nil {
		st.toolNameMap = codexToolAliasesFromRequests(protocol.OpenAI, rawReq, translatedReq).ShortToOriginal
	}
	if original := st.toolNameMap[name]; original != "" {
		return original
	}
	return name
}

func encodeCodexOutputContentPart(part conversationPart) (map[string]any, error) {
	switch part.Kind {
	case partKindText:
		return map[string]any{"type": "output_text", "text": part.Text}, nil
	case partKindImage, partKindFile:
		return nil, fmt.Errorf("unsupported non-text OpenAI response content for Codex output")
	default:
		return nil, nil
	}
}

func marshalRawCodexEvent(eventName, payload string) []byte {
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", eventName, payload))
}

func codexReasoningItemsFromOpenAIMessage(message map[string]any) []map[string]any {
	items := make([]map[string]any, 0)
	if rawReasoning, ok := message["reasoning"].([]any); ok {
		for _, item := range rawReasoning {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			text := stringValue(entry["text"])
			encrypted := stringValue(entry["encrypted_content"])
			items = append(items, codexReasoningItem(text, encrypted))
		}
	}
	if len(items) == 0 {
		if text := stringValue(message["reasoning_content"]); text != "" {
			items = append(items, codexReasoningItem(text, ""))
		}
	}
	return items
}

func extractCodexReasoningText(item map[string]any) string {
	for _, key := range []string{"summary", "content"} {
		var text strings.Builder
		parts, _ := item[key].([]any)
		for _, raw := range parts {
			part, _ := raw.(map[string]any)
			switch normalizeRole(stringValue(part["type"])) {
			case "reasoning_text", "summary_text":
				text.WriteString(stringValue(part["text"]))
			}
		}
		if text.Len() > 0 {
			return text.String()
		}
	}
	return ""
}

func openAIUsageFromMap(value any) *openAIUsage {
	usageMap, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	usage := &openAIUsage{
		promptTokens:             int64Value(usageMap["prompt_tokens"]),
		completionTokens:         int64Value(usageMap["completion_tokens"]),
		totalTokens:              int64Value(usageMap["total_tokens"]),
		cacheCreationInputTokens: int64Value(usageMap["cache_creation_input_tokens"]),
	}
	if details, ok := usageMap["prompt_tokens_details"].(map[string]any); ok {
		usage.cachedTokens = int64Value(details["cached_tokens"])
	}
	if details, ok := usageMap["completion_tokens_details"].(map[string]any); ok {
		usage.reasoningTokens = int64Value(details["reasoning_tokens"])
	}
	if usage.totalTokens == 0 {
		usage.totalTokens = usage.promptTokens + usage.completionTokens
	}

	return usage
}

func codexUsageFromMap(value any) *codexUsage {
	usageMap, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	usage := &codexUsage{
		inputTokens:              int64Value(usageMap["input_tokens"]),
		outputTokens:             int64Value(usageMap["output_tokens"]),
		totalTokens:              int64Value(usageMap["total_tokens"]),
		cacheCreationInputTokens: int64Value(usageMap["cache_creation_input_tokens"]),
	}
	if details, ok := usageMap["input_tokens_details"].(map[string]any); ok {
		usage.cachedTokens = int64Value(details["cached_tokens"])
		// OpenAI Responses / Codex 原生缓存建字段；仅在无顶层 cache_creation_input_tokens 时采用
		if usage.cacheCreationInputTokens == 0 {
			usage.cacheCreationInputTokens = int64Value(details["cache_write_tokens"])
		}
	} else {
		usage.cachedTokens = int64Value(usageMap["cache_read_input_tokens"])
	}
	if details, ok := usageMap["output_tokens_details"].(map[string]any); ok {
		usage.reasoningTokens = int64Value(details["reasoning_tokens"])
	}
	if usage.totalTokens == 0 {
		usage.totalTokens = usage.inputTokens + usage.outputTokens
	}

	return usage
}

func int64Value(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	default:
		return 0
	}
}

func coalesceModel(model string, fallback any) string {
	if strings.TrimSpace(model) != "" {
		return model
	}
	return stringValue(fallback)
}
