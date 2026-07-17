package protocol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

func TestRegistry_TranslateRequest_CodexCustomToolToOpenAIFunction(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{
		"model":"gpt-5.5",
		"tools":[
			{"type":"custom","name":"apply_patch","description":"Apply a patch","format":{"type":"text"}},
			{"type":"function","name":"lookup","description":"Look up data","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}},
			{"type":"namespace","name":"codex_app","description":"Tools provided by the Codex app","tools":[{"type":"function","name":"list_threads","description":"List threads","parameters":{"type":"object"}}]},
			{"type":"tool_search","execution":"client"},
			{"type":"future_codex_tool","name":"future_tool"}
		],
		"tool_choice":{"type":"custom","name":"apply_patch"},
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"update the file"}]},
			{"type":"custom_tool_call","call_id":"call_patch_1","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},
			{"type":"custom_tool_call_output","call_id":"call_patch_1","output":"Done!"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)

	got, err := reg.TranslateRequest(protocol.Codex, protocol.OpenAI, "grok-4.5", rawReq, false)
	if err != nil {
		t.Fatalf("TranslateRequest failed: %v", err)
	}

	var req map[string]any
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("unmarshal translated request: %v", err)
	}
	tools, _ := req["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("tools len=%d, want 4; body=%s", len(tools), got)
	}
	for _, omitted := range [][]byte{[]byte(`future_codex_tool`)} {
		if bytes.Contains(got, omitted) {
			t.Fatalf("unsupported Codex tool %s must be omitted from OpenAI request: %s", omitted, got)
		}
	}
	for _, included := range [][]byte{[]byte(`codex_app__list_threads`), []byte(`tool_search`)} {
		if !bytes.Contains(got, included) {
			t.Fatalf("translated request missing %s: %s", included, got)
		}
	}
	customFn := tools[0].(map[string]any)["function"].(map[string]any)
	if customFn["name"] != "apply_patch" {
		t.Fatalf("custom function name=%v, want apply_patch", customFn["name"])
	}
	parameters, _ := customFn["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	inputSchema, _ := properties["input"].(map[string]any)
	if inputSchema["type"] != "string" {
		t.Fatalf("custom input schema=%#v, want string", inputSchema)
	}
	choice, _ := req["tool_choice"].(map[string]any)
	choiceFn, _ := choice["function"].(map[string]any)
	if choice["type"] != "function" || choiceFn["name"] != "apply_patch" {
		t.Fatalf("tool_choice=%#v, want named apply_patch function", choice)
	}

	messages, _ := req["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages len=%d, want 4; body=%s", len(messages), got)
	}
	assistant := messages[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls=%#v", assistant["tool_calls"])
	}
	callFn := calls[0].(map[string]any)["function"].(map[string]any)
	var arguments map[string]any
	if err := json.Unmarshal([]byte(callFn["arguments"].(string)), &arguments); err != nil {
		t.Fatalf("custom arguments are not JSON: %v", err)
	}
	if arguments["input"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom arguments input=%#v", arguments["input"])
	}
	toolResult := messages[2].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "call_patch_1" || toolResult["content"] != "Done!" {
		t.Fatalf("tool result=%#v", toolResult)
	}
}

func TestRegistry_TranslateResponse_OpenAIFunctionRestoresNamespaceAndToolSearch(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	rawReq := []byte(`{"model":"gpt-5.5","tools":[{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"list_threads","parameters":{"type":"object"}}]},{"type":"tool_search","execution":"client"}],"input":"go"}`)
	rawResp := []byte(`{"model":"grok-4.5","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_ns","type":"function","function":{"name":"codex_app__list_threads","arguments":"{}"}},{"id":"call_search","type":"function","function":{"name":"tool_search","arguments":"{\"query\":\"threads\",\"limit\":3}"}}]}}]}`)
	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Codex, "gpt-5.5", rawReq, nil, rawResp)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{`"type":"function_call"`, `"name":"list_threads"`, `"namespace":"codex_app"`, `"type":"tool_search_call"`, `"query":"threads"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
}

func TestRegistry_TranslateRequest_ToolSearchHistoryReloadsDeferredTools(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	rawReq := []byte(`{
		"model":"gpt-5.5",
		"tools":[{"type":"tool_search","execution":"client"}],
		"input":[
			{"type":"tool_search_call","call_id":"search_1","arguments":{"query":"threads"}},
			{"type":"tool_search_output","call_id":"search_1","status":"completed","tools":[{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"read_thread","parameters":{"type":"object"}}]}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)
	got, err := reg.TranslateRequest(protocol.Codex, protocol.OpenAI, "grok-4.5", rawReq, false)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{`"name":"tool_search"`, `"tool_call_id":"search_1"`, `codex_app__read_thread`, `Tool search loaded these tools`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
}

func TestRegistry_TranslateRequest_GrokFlattensNestedRootToolUnion(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	rawReq := []byte(`{"model":"gpt-5.5","tools":[{"type":"function","name":"automation_update","parameters":{"oneOf":[{"type":"object","properties":{"mode":{"type":"string"}}},{"oneOf":[{"type":"object","properties":{"path":{"type":"string"}}},{"type":"object","properties":{}}]}],"$defs":{"shared":{"type":"string"}}}}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}]}`)
	got, err := reg.TranslateRequest(protocol.Codex, protocol.OpenAI, "grok-4.5", rawReq, false)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatal(err)
	}
	tools := req["tools"].([]any)
	params := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	oneOf := params["oneOf"].([]any)
	if len(oneOf) != 3 {
		t.Fatalf("flattened oneOf len=%d, want 3: %s", len(oneOf), got)
	}
	for _, raw := range oneOf {
		if raw.(map[string]any)["type"] != "object" {
			t.Fatalf("non-object Grok branch: %s", got)
		}
	}
}

func TestRegistry_TranslateRequest_GrokRejectsNamedToolWhenSchemaIsOmitted(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	rawReq := []byte(`{
		"model":"gpt-5.5",
		"tools":[
			{"type":"function","name":"invalid_union","parameters":{"oneOf":[{"type":"object"},{"type":"string"}]}},
			{"type":"function","name":"valid_tool","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"function","name":"invalid_union"},
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}]
	}`)
	_, err := reg.TranslateRequest(protocol.Codex, protocol.OpenAI, "grok-4.5", rawReq, false)
	if err == nil || !strings.Contains(err.Error(), `selected tool "invalid_union" was omitted`) {
		t.Fatalf("expected explicit omitted named-tool error, got %v", err)
	}
}

func TestRegistry_TranslateResponseNonStream_OpenAIFunctionToCodexCustomTool(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gpt-5.5","tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input":"go"}`)
	rawResp := []byte(`{"id":"chatcmpl_1","object":"chat.completion","model":"grok-4.5","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}"}},{"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"go\"}"}}]},"finish_reason":"tool_calls"}]}`)

	got, err := reg.TranslateResponseNonStream(context.Background(), protocol.OpenAI, protocol.Codex, "gpt-5.5", rawReq, nil, rawResp)
	if err != nil {
		t.Fatalf("TranslateResponseNonStream failed: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(got, &resp); err != nil {
		t.Fatalf("unmarshal translated response: %v", err)
	}
	output, _ := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output len=%d, want 2; body=%s", len(output), got)
	}
	customCall := output[0].(map[string]any)
	if customCall["type"] != "custom_tool_call" || customCall["call_id"] != "call_patch" || customCall["name"] != "apply_patch" {
		t.Fatalf("custom call=%#v", customCall)
	}
	if customCall["input"] != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom input=%#v", customCall["input"])
	}
	functionCall := output[1].(map[string]any)
	if functionCall["type"] != "function_call" || functionCall["name"] != "lookup" {
		t.Fatalf("ordinary function call was misclassified: %#v", functionCall)
	}
}

func TestRegistry_TranslateResponseStream_OpenAIFunctionToCodexCustomTool(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	rawReq := []byte(`{"model":"gpt-5.5","tools":[{"type":"custom","name":"apply_patch"}],"input":"go","stream":true}`)
	chunks := []string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"grok-4.5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":""}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"grok-4.5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"input\":\"patch text\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}

	var state any
	var output bytes.Buffer
	for _, chunk := range chunks {
		translated, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Codex, "gpt-5.5", rawReq, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		for _, item := range translated {
			output.Write(item)
		}
	}
	result := output.String()
	for _, want := range []string{`"type":"custom_tool_call"`, `"call_id":"call_patch"`, `"name":"apply_patch"`, `"input":"patch text"`, `event: response.completed`} {
		if !strings.Contains(result, want) {
			t.Fatalf("stream output missing %s:\n%s", want, result)
		}
	}
}

func TestRegistry_TranslateResponseStream_OpenAITextToCompletedCodexMessage(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	chunks := []string{
		`data: {"model":"grok-4.5","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
		`data: {"model":"grok-4.5","choices":[{"index":0,"delta":{"content":"# Miner"},"finish_reason":null}]}` + "\n\n",
		`data: {"model":"grok-4.5","choices":[{"index":0,"delta":{"content":"adio"},"finish_reason":null}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	var output bytes.Buffer
	for _, chunk := range chunks {
		translated, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Codex, "gpt-5.5", []byte(`{"model":"gpt-5.5","stream":true}`), nil, []byte(chunk), &state)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range translated {
			output.Write(item)
		}
	}
	result := output.String()
	for _, want := range []string{`event: response.output_item.done`, `"type":"message"`, `"role":"assistant"`, `"text":"# Mineradio"`, `event: response.completed`} {
		if !strings.Contains(result, want) {
			t.Fatalf("stream output missing %s:\n%s", want, result)
		}
	}
	for _, want := range []string{`event: response.output_item.added`, `event: response.content_part.added`, `event: response.output_text.done`} {
		if !strings.Contains(result, want) {
			t.Fatalf("stream output missing lifecycle event %s:\n%s", want, result)
		}
	}
}

func TestRegistry_TranslateResponseStream_OpenAIReasoningPrecedesTextWithDistinctIndexes(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	chunks := []string{
		`data: {"model":"grok-4.5","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}` + "\n\n",
		`data: {"model":"grok-4.5","choices":[{"index":0,"delta":{"content":"answer"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	var output bytes.Buffer
	for _, chunk := range chunks {
		translated, err := reg.TranslateResponseStream(context.Background(), protocol.OpenAI, protocol.Codex, "gpt-5.5", []byte(`{"model":"gpt-5.5","stream":true}`), nil, []byte(chunk), &state)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range translated {
			output.Write(item)
		}
	}
	result := output.String()
	reasoningAdded := strings.Index(result, `event: response.output_item.added`)
	reasoningDelta := strings.Index(result, `event: response.reasoning_summary_text.delta`)
	reasoningDone := -1
	if reasoningDelta >= 0 {
		reasoningDone = strings.Index(result[reasoningDelta:], `event: response.output_item.done`)
		if reasoningDone >= 0 {
			reasoningDone += reasoningDelta
		}
	}
	textAdded := -1
	if reasoningDone >= 0 {
		textAdded = strings.Index(result[reasoningDone:], `event: response.output_item.added`)
		if textAdded >= 0 {
			textAdded += reasoningDone
		}
	}
	if reasoningAdded < 0 || reasoningDelta < reasoningAdded || reasoningDone < reasoningDelta || textAdded < reasoningDone {
		t.Fatalf("reasoning lifecycle must complete before text starts:\n%s", result)
	}
	for _, want := range []string{`"id":"rs-proxy-0"`, `"output_index":0`, `"output_index":1`, `event: response.reasoning_summary_text.done`, `"delta":"answer"`, `event: response.completed`} {
		if !strings.Contains(result, want) {
			t.Fatalf("stream output missing %s:\n%s", want, result)
		}
	}
	reasoningSummary := func(eventName string) []any {
		marker := "event: " + eventName + "\ndata: "
		start := strings.Index(result, marker)
		if start < 0 {
			t.Fatalf("stream output missing %s:\n%s", eventName, result)
		}
		data := result[start+len(marker):]
		if end := strings.Index(data, "\n\n"); end >= 0 {
			data = data[:end]
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("decode %s: %v", eventName, err)
		}
		item, _ := payload["item"].(map[string]any)
		summary, ok := item["summary"].([]any)
		if !ok {
			t.Fatalf("%s reasoning item missing summary: %#v", eventName, item)
		}
		return summary
	}
	if summary := reasoningSummary("response.output_item.added"); len(summary) != 0 {
		t.Fatalf("added reasoning summary=%#v, want empty", summary)
	}
	summary := reasoningSummary("response.output_item.done")
	if len(summary) != 1 {
		t.Fatalf("done reasoning summary=%#v, want one part", summary)
	}
	part, ok := summary[0].(map[string]any)
	if !ok || part["type"] != "summary_text" || part["text"] != "think" {
		t.Fatalf("done reasoning summary=%#v, want think", summary)
	}
}

func TestRegistry_TranslateRequest_CodexToolsToAnthropicLocal(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	rawReq := []byte(`{"model":"gpt-5.5","tools":[{"type":"custom","name":"apply_patch"},{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"list_threads","parameters":{"type":"object"}}]},{"type":"tool_search","execution":"client"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}]}`)
	got, err := reg.TranslateRequest(protocol.Codex, protocol.Anthropic, "grok-4.5", rawReq, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name":"apply_patch"`, `"name":"codex_app__list_threads"`, `"name":"tool_search"`, `"input_schema"`} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
}

func TestRegistry_TranslateResponseStream_AnthropicStartTextAndToolRoutesToCodex(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	rawReq := []byte(`{"model":"gpt-5.5","tools":[{"type":"custom","name":"apply_patch"},{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"list_threads","parameters":{"type":"object"}}]},{"type":"tool_search","execution":"client"}],"input":"go","stream":true}`)
	var state any
	textChunks, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5.5", rawReq, nil, []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"# Mineradio\"}}\n\n"), &state)
	if err != nil {
		t.Fatal(err)
	}
	started := string(bytes.Join(textChunks, nil))
	for _, want := range []string{`event: response.output_item.added`, `event: response.content_part.added`, `event: response.output_text.delta`, `"delta":"# Mineradio"`, `"id":"msg-proxy-1"`} {
		if !strings.Contains(started, want) {
			t.Fatalf("streaming text start missing %s: %s", want, started)
		}
	}
	textChunks, err = reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5.5", rawReq, nil, []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"), &state)
	if err != nil {
		t.Fatal(err)
	}
	if joined := string(bytes.Join(textChunks, nil)); !strings.Contains(joined, `"type":"message"`) || !strings.Contains(joined, `"text":"# Mineradio"`) {
		t.Fatalf("completed text item missing: %s", joined)
	}

	state = nil
	start := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"codex_app__list_threads\",\"input\":{}}}\n\n")
	if out, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5.5", rawReq, nil, start, &state); err != nil || out != nil {
		t.Fatalf("tool start=%q err=%v", bytes.Join(out, nil), err)
	}
	done, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5.5", rawReq, nil, []byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"), &state)
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(done, nil))
	for _, want := range []string{`"name":"list_threads"`, `"namespace":"codex_app"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %s", want, joined)
		}
	}
}

func TestRegistry_TranslateResponseStream_AnthropicTextBlocksUseUniqueCodexMessageIDs(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	events := []string{
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"first\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"text\",\"text\":\"second\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	var output bytes.Buffer
	for _, event := range events {
		chunks, err := reg.TranslateResponseStream(context.Background(), protocol.Anthropic, protocol.Codex, "gpt-5.5", []byte(`{"model":"gpt-5.5","stream":true}`), nil, []byte(event), &state)
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range chunks {
			output.Write(chunk)
		}
	}
	result := output.String()
	for _, want := range []string{`"id":"msg-proxy-1"`, `"id":"msg-proxy-2"`, `"output_index":0`, `"output_index":2`, `"text":"first"`, `"text":"second"`} {
		if !strings.Contains(result, want) {
			t.Fatalf("multi-block stream missing %s:\n%s", want, result)
		}
	}
}

func TestRegistry_TranslateRequest_CodexDataURLImageToAnthropicBase64(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	rawReq := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]}`)
	got, err := reg.TranslateRequest(protocol.Codex, protocol.Anthropic, "grok-4.5", rawReq, true)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{`"type":"base64"`, `"media_type":"image/png"`, `"data":"aGVsbG8="`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s in %s", want, text)
		}
	}
	if strings.Contains(text, "data:image/png;base64") {
		t.Fatalf("data URL prefix leaked into Anthropic base64 payload: %s", text)
	}
}

func TestRegistry_TranslateRequest_CodexHTTPSImageStaysAnthropicURL(t *testing.T) {
	t.Parallel()
	reg := protocol.NewRegistry()
	builtin.Register(reg)
	rawReq := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.com/image.png"}]}]}`)
	got, err := reg.TranslateRequest(protocol.Codex, protocol.Anthropic, "grok-4.5", rawReq, false)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, `"type":"url"`) || !strings.Contains(text, `"url":"https://example.com/image.png"`) {
		t.Fatalf("HTTPS image was not preserved as URL: %s", text)
	}
}
