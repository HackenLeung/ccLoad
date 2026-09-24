package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func convertOpenAIResponseToCodexStream(ctx context.Context, model string, rawReq, translatedReq, rawJSON []byte, param *any) ([][]byte, error) {
	if param == nil {
		var state any
		param = &state
	}
	chunks, err := convertOpenAIResponseToCodexStreamEvent(ctx, model, rawReq, translatedReq, rawJSON, param)
	if err != nil || len(chunks) == 0 {
		return chunks, err
	}
	event, data := parseSSEEventBlockOrRaw(string(rawJSON))
	if isCodexResponseEventType(event) {
		return chunks, nil
	}
	if data != "[DONE]" && isCodexResponseEventType(gjson.Get(data, "type").String()) {
		return chunks, nil
	}
	st := (*param).(*openAIToCodexStreamState)
	if !st.started {
		created, err := appendCodexSSEEvent(nil, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": st.responseID, "object": "response", "created_at": st.created, "status": "in_progress", "model": st.model, "output": []any{}}})
		if err != nil {
			return nil, err
		}
		chunks = append([][]byte{created}, chunks...)
		st.started = true
	}
	for i, chunk := range chunks {
		typ, raw := parseSSEEventBlock(string(chunk))
		encoded, err := sjson.Set(raw, "sequence_number", st.sequence)
		if err != nil {
			return nil, err
		}
		st.sequence++
		chunks[i] = marshalRawCodexEvent(typ, encoded)
	}
	return chunks, nil
}

func setResponsesCompletionStatus(response map[string]any, reason string) string {
	response["status"] = "completed"
	switch reason {
	case "length":
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": "content_filter"}
	}
	return "response." + stringValue(response["status"])
}

func responsesFinishReason(response map[string]any) string {
	if stringValue(response["status"]) != "incomplete" {
		return ""
	}
	details, _ := response["incomplete_details"].(map[string]any)
	if stringValue(details["reason"]) == "content_filter" {
		return "content_filter"
	}
	return "length"
}

func copyServiceTier(src, dst map[string]any) {
	if tier, ok := src["service_tier"]; ok {
		dst["service_tier"] = tier
	}
}

func (st *openAIToCodexStreamState) reasoningDelta(text string) ([][]byte, error) {
	var chunks [][]byte
	if !st.reasoningStarted {
		st.reasoningIndex = st.nextOutputIndex
		st.nextOutputIndex++
		st.reasoningStarted = true
		id := fmt.Sprintf("%s_rs_%d", st.responseID, st.reasoningIndex)
		for _, payload := range []map[string]any{
			{"type": "response.output_item.added", "output_index": st.reasoningIndex, "item": map[string]any{"id": id, "type": "reasoning", "status": "in_progress", "summary": []any{}}},
			{"type": "response.reasoning_summary_part.added", "output_index": st.reasoningIndex, "item_id": id, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}},
		} {
			chunk, err := appendCodexSSEEvent(nil, stringValue(payload["type"]), payload)
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, chunk)
		}
	}
	if text != "" {
		st.reasoningText += text
		chunk, err := appendCodexSSEEvent(nil, "response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": fmt.Sprintf("%s_rs_%d", st.responseID, st.reasoningIndex), "output_index": st.reasoningIndex, "summary_index": 0, "delta": text})
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}

func (st *openAIToCodexStreamState) toolItemID(tool *pendingToolCall) string {
	return fmt.Sprintf("%s_fc_%d", st.responseID, tool.outputIndex)
}

func (st *openAIToCodexStreamState) toolDeltas(tool *pendingToolCall) ([][]byte, error) {
	// JSON-wrapped custom inputs and tool search arguments cannot be emitted
	// as raw function argument deltas. Their existing terminal adapter remains.
	route := st.toolRoute(tool.name)
	if route.Type == "custom" || route.Type == "tool_search" || tool.name == "" || tool.id == "" {
		return nil, nil
	}
	var chunks [][]byte
	if !tool.started {
		item := codexToolCallItemFromOpenAI(tool.id, tool.name, "", route)
		item["id"], item["status"] = st.toolItemID(tool), "in_progress"
		chunk, err := appendCodexSSEEvent(nil, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": tool.outputIndex, "item": item})
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
		tool.started = true
	}
	if tool.sent < len(tool.arguments) {
		chunk, err := appendCodexSSEEvent(nil, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": st.toolItemID(tool), "output_index": tool.outputIndex, "delta": tool.arguments[tool.sent:]})
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
		tool.sent = len(tool.arguments)
	}
	return chunks, nil
}

type responsesChatToolState struct {
	index     int
	id        string
	name      string
	arguments string
	started   bool
	done      bool
}

func responsesEventItemKey(payload, item map[string]any) string {
	if id := stringValue(payload["item_id"]); id != "" {
		return id
	}
	if id := stringValue(item["id"]); id != "" {
		return id
	}
	if idx, exists := payload["output_index"]; exists {
		return fmt.Sprint(idx)
	}
	return stringValue(item["call_id"])
}

func (st *codexToOpenAIStreamState) deltaChunk(delta map[string]any) ([][]byte, error) {
	chunk := map[string]any{"id": st.responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": st.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}}}
	body, err := sonic.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return [][]byte{append(append([]byte("data: "), body...), '\n', '\n')}, nil
}

func (st *codexToOpenAIStreamState) convertIncrementalEvent(event string, payload map[string]any, rawReq, translatedReq []byte) ([][]byte, bool, error) {
	item, _ := payload["item"].(map[string]any)
	key := responsesEventItemKey(payload, item)
	switch event {
	case "error", "response.failed":
		failure := payload["error"]
		if response, ok := payload["response"].(map[string]any); ok {
			failure = response["error"]
		}
		if failure == nil {
			failure = map[string]any{"type": "upstream_error", "message": "upstream response failed"}
		}
		body, err := sonic.Marshal(map[string]any{"error": failure})
		st.finished = true
		return [][]byte{append(append([]byte("data: "), body...), '\n', '\n'), []byte("data: [DONE]\n\n")}, true, err
	case "response.refusal.delta":
		chunks, err := st.deltaChunk(map[string]any{"refusal": stringValue(payload["delta"])})
		return chunks, true, err
	case "response.reasoning_summary_text.delta":
		st.reasoningSent[key] = true
		chunks, err := st.deltaChunk(map[string]any{"reasoning_content": stringValue(payload["delta"])})
		return chunks, true, err
	case "response.output_item.done":
		if stringValue(item["type"]) == "reasoning" && st.reasoningSent[key] {
			return nil, true, nil
		}
		if stringValue(item["type"]) != "function_call" {
			return nil, false, nil
		}
	case "response.output_item.added":
		if stringValue(item["type"]) != "function_call" {
			return nil, false, nil
		}
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
	default:
		return nil, false, nil
	}
	tool := st.tools[key]
	if tool == nil {
		tool = &responsesChatToolState{index: st.toolCallIndex}
		st.toolCallIndex++
		st.tools[key] = tool
	}
	if tool.done {
		return nil, true, nil
	}
	if item != nil {
		if id := stringValue(item["call_id"]); id != "" {
			tool.id = id
		}
		if name := stringValue(item["name"]); name != "" {
			tool.name = st.restoreToolName(rawReq, translatedReq, name)
		}
	}
	arguments := ""
	switch event {
	case "response.function_call_arguments.delta":
		arguments = stringValue(payload["delta"])
	case "response.output_item.done", "response.function_call_arguments.done":
		complete := stringValue(payload["arguments"])
		if item != nil {
			call, err := decodeCodexToolCall(item)
			if err != nil {
				return nil, true, err
			}
			complete = string(call.Arguments)
			// Streaming arguments are byte-preserving. The shared decoder trims
			// JSON strings for request conversion, but deltas retain that whitespace.
			if raw, ok := item["arguments"].(string); ok {
				complete = raw
			}
		}
		if !strings.HasPrefix(complete, tool.arguments) {
			return nil, true, fmt.Errorf("inconsistent completed tool arguments for %s", key)
		}
		arguments = strings.TrimPrefix(complete, tool.arguments)
	}
	tool.arguments += arguments
	if tool.id == "" || tool.name == "" {
		return nil, true, nil
	}
	function := map[string]any{"arguments": arguments}
	call := map[string]any{"index": tool.index, "function": function}
	if !tool.started {
		call["id"], call["type"] = tool.id, "function"
		function["name"], function["arguments"] = tool.name, tool.arguments
		tool.started = true
	} else if arguments == "" {
		if event == "response.output_item.done" {
			tool.done = true
		}
		return nil, true, nil
	}
	if event == "response.output_item.done" {
		tool.done = true
	}
	st.sawToolCall = true
	chunks, err := st.deltaChunk(map[string]any{"tool_calls": []map[string]any{call}})
	return chunks, true, err
}

// Refusals have their own content part and stable output index throughout the stream.
func (st *openAIToCodexStreamState) refusalEvents(delta string, done bool) ([][]byte, error) {
	var events []map[string]any
	id := st.responseID + "_refusal"
	if !st.refusalStarted {
		st.refusalStarted = true
		st.refusalIndex = st.nextOutputIndex
		st.nextOutputIndex++
		events = append(events, map[string]any{"type": "response.output_item.added", "output_index": st.refusalIndex, "item": map[string]any{"id": id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
		events = append(events, map[string]any{"type": "response.content_part.added", "part": map[string]any{"type": "refusal", "refusal": ""}})
	}
	if delta != "" {
		st.refusal += delta
		events = append(events, map[string]any{"type": "response.refusal.delta", "delta": delta})
	}
	if done {
		part := map[string]any{"type": "refusal", "refusal": st.refusal}
		item := map[string]any{"id": id, "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}
		st.output[st.refusalIndex] = item
		events = append(events, map[string]any{"type": "response.refusal.done", "refusal": st.refusal}, map[string]any{"type": "response.content_part.done", "part": part}, map[string]any{"type": "response.output_item.done", "output_index": st.refusalIndex, "item": item})
	}
	chunks := make([][]byte, 0, len(events))
	for _, event := range events {
		typ := stringValue(event["type"])
		if typ != "response.output_item.added" && typ != "response.output_item.done" {
			event["item_id"], event["output_index"], event["content_index"] = id, st.refusalIndex, 0
		}
		chunk, err := appendCodexSSEEvent(nil, typ, event)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}
