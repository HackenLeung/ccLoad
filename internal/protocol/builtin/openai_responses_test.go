package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestOpenAIResponsesRequestSemantics(t *testing.T) {
	for _, effort := range []string{"none", "minimal", "low", "xhigh"} {
		t.Run(effort, func(t *testing.T) {
			src := `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + effort + `","store":false,"service_tier":"flex","prompt_cache_key":"key","response_format":{"type":"json_schema","json_schema":{"name":"result","strict":true,"schema":{"type":"object","properties":{}}}},"tools":[{"type":"function","function":{"name":"lookup","strict":false,"parameters":{"type":"object"}}}]}`
			out, err := convertOpenAIRequestToCodex("test", []byte(src), true)
			if err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]string{"reasoning.effort": effort, "store": "false", "service_tier": "flex", "prompt_cache_key": "key", "text.format.type": "json_schema", "text.format.name": "result", "text.format.strict": "true", "tools.0.strict": "false"} {
				if got := gjson.GetBytes(out, path); !got.Exists() || got.String() != want {
					t.Fatalf("%s=%s, want %s: %s", path, got, want, out)
				}
			}
			back, err := convertCodexRequestToOpenAI("test", out, true)
			if err != nil {
				t.Fatal(err)
			}
			if !gjson.GetBytes(back, "stream_options.include_usage").Bool() || gjson.GetBytes(back, "response_format.json_schema.name").String() != "result" {
				t.Fatalf("missing stream usage or schema: %s", back)
			}
		})
	}
}

func TestResponsesStringInputAndUnsupportedState(t *testing.T) {
	out, err := convertCodexRequestToOpenAI("test", []byte(`{"input":"hello","max_output_tokens":128}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "messages.0.content").String() != "hello" || gjson.GetBytes(out, "max_completion_tokens").Int() != 128 {
		t.Fatal(string(out))
	}
	for _, src := range []string{`{"input":"hi","previous_response_id":"resp_old"}`, `{"input":"hi","conversation":"conv_old"}`, `{"input":"hi","context_management":[{"type":"compaction"}]}`} {
		if _, err := convertCodexRequestToOpenAI("test", []byte(src), false); err == nil {
			t.Fatalf("silently accepted stateful request: %s", src)
		}
	}
	if _, err := convertOpenAIRequestToCodex("test", []byte(`{"messages":[{"role":"user","content":"hi"}],"n":2}`), false); err == nil {
		t.Fatal("accepted n=2")
	}
}

func TestFunctionStrictDefaultsAndValidation(t *testing.T) {
	for _, strict := range []string{"", `,"strict":false`, `,"strict":true`} {
		out, err := convertOpenAIRequestToCodex("test", []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup"`+strict+`}}]}`), false)
		if err != nil {
			t.Fatal(err)
		}
		got := gjson.GetBytes(out, "tools.0.strict")
		if !got.Exists() || got.Bool() != strings.Contains(strict, "true") {
			t.Fatalf("strict default changed: %s", out)
		}
	}
}

func TestOpenAIResponsesMixedDeltaAndSparseTools(t *testing.T) {
	var state any
	chunk := `data: {"choices":[{"delta":{"reasoning_content":"think","content":"answer","tool_calls":[{"index":2,"id":"call_2","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}` + "\n\n"
	first, err := convertOpenAIResponseToCodexStream(context.Background(), "alias", nil, nil, []byte(chunk), &state)
	if err != nil {
		t.Fatal(err)
	}
	firstBody := string(bytes.Join(first, nil))
	for _, want := range []string{"response.function_call_arguments.delta", `"delta":"answer"`, `"delta":"think"`} {
		if !strings.Contains(firstBody, want) {
			t.Fatalf("missing %s before completion: %s", want, firstBody)
		}
	}
	_, err = convertOpenAIResponseToCodexStream(context.Background(), "alias", nil, nil, []byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":2,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"length\"}]}\n\n"), &state)
	if err != nil {
		t.Fatal(err)
	}
	last, err := convertOpenAIResponseToCodexStream(context.Background(), "alias", nil, nil, []byte("data: [DONE]\n\n"), &state)
	if err != nil {
		t.Fatal(err)
	}
	_, raw := parseSSEEventBlock(string(last[len(last)-1]))
	if gjson.Get(raw, "type").String() != "response.incomplete" || gjson.Get(raw, "response.output.#").Int() != 3 {
		t.Fatalf("lost terminal output: %s", raw)
	}
	if gjson.Get(raw, "response.incomplete_details.reason").String() != "max_output_tokens" {
		t.Fatal(raw)
	}
}

func TestResponsesChatInterleavedArgumentsNotDuplicated(t *testing.T) {
	var state any
	events := []map[string]any{
		{"type": "response.output_item.added", "output_index": 3, "item": map[string]any{"id": "fc_a", "type": "function_call", "call_id": "a", "name": "one", "arguments": ""}},
		{"type": "response.output_item.added", "output_index": 4, "item": map[string]any{"id": "fc_b", "type": "function_call", "call_id": "b", "name": "two", "arguments": ""}},
		{"type": "response.function_call_arguments.delta", "item_id": "fc_b", "delta": "{\"b\":2}"},
		{"type": "response.function_call_arguments.delta", "item_id": "fc_a", "delta": "{\"a\":"},
		{"type": "response.function_call_arguments.delta", "item_id": "fc_a", "delta": "1}"},
		{"type": "response.output_item.done", "item": map[string]any{"id": "fc_a", "type": "function_call", "call_id": "a", "name": "one", "arguments": "{\"a\":1}"}},
		{"type": "response.output_item.done", "item": map[string]any{"id": "fc_b", "type": "function_call", "call_id": "b", "name": "two", "arguments": "{\"b\":2}"}},
	}
	args := map[int64]string{}
	for i, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		chunks, err := convertCodexResponseToOpenAIStream(context.Background(), "alias", nil, nil, append(append([]byte("data: "), raw...), '\n', '\n'), &state)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 && len(chunks) == 0 {
			t.Fatal("buffered arguments until done")
		}
		for _, chunk := range chunks {
			_, data := parseSSEEventBlock(string(chunk))
			call := gjson.Get(data, "choices.0.delta.tool_calls.0")
			args[call.Get("index").Int()] += call.Get("function.arguments").String()
		}
	}
	if args[0] != `{"a":1}` || args[1] != `{"b":2}` {
		t.Fatalf("corrupt tool arguments: %#v", args)
	}
}

func TestResponsesChatToolArgumentsPreserveWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delta     string
		complete  string
		wantError bool
	}{
		{name: "leading and trailing whitespace", delta: " \n{\"q\":1}\t ", complete: " \n{\"q\":1}\t "},
		{name: "terminal suffix", delta: " {\"q\":", complete: " {\"q\":1} "},
		{name: "terminal only", complete: " {\"q\":1} "},
		{name: "changed arguments", delta: " {\"q\":1} ", complete: " {\"q\":2} ", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state any
			events := []map[string]any{
				{"type": "response.output_item.added", "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": ""}},
				{"type": "response.function_call_arguments.delta", "item_id": "fc_1", "delta": tc.delta},
				{"type": "response.output_item.done", "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": tc.complete}},
			}
			var arguments strings.Builder
			for i, event := range events {
				raw, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				chunks, err := convertCodexResponseToOpenAIStream(context.Background(), "test", nil, nil, append(append([]byte("data: "), raw...), '\n', '\n'), &state)
				if i == len(events)-1 && tc.wantError {
					if err == nil {
						t.Fatal("accepted inconsistent arguments")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, chunk := range chunks {
					_, data := parseSSEEventBlock(string(chunk))
					arguments.WriteString(gjson.Get(data, "choices.0.delta.tool_calls.0.function.arguments").String())
				}
			}
			if arguments.String() != tc.complete {
				t.Fatalf("arguments = %q, want %q", arguments.String(), tc.complete)
			}
		})
	}
}

func TestResponsesNonStreamIncomplete(t *testing.T) {
	out, err := convertCodexResponseToOpenAINonStream(context.Background(), "test", nil, nil, []byte(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "choices.0.finish_reason").String() != "length" {
		t.Fatal(string(out))
	}
	out, err = convertOpenAIResponseToCodexNonStream(context.Background(), "test", nil, nil, []byte(`{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"content_filter"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "status").String() != "incomplete" {
		t.Fatal(string(out))
	}
}

func TestResponsesLifecycleIDsAndUsageTail(t *testing.T) {
	var state any
	var events []map[string]any
	for _, raw := range []string{
		`data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}` + "\n\n",
		"data: [DONE]\n\n",
	} {
		chunks, err := convertOpenAIResponseToCodexStream(context.Background(), "alias", nil, nil, []byte(raw), &state)
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range chunks {
			_, data := parseSSEEventBlock(string(chunk))
			var event map[string]any
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
	}
	for i, event := range events {
		if int64Value(event["sequence_number"]) != int64(i) {
			t.Fatalf("bad sequence: %#v", event)
		}
	}
	first := events[0]["response"].(map[string]any)
	last := events[len(events)-1]["response"].(map[string]any)
	if first["id"] != last["id"] || first["id"] == "resp-proxy" || last["usage"] == nil {
		t.Fatalf("invalid identity or missing zero usage: %#v", last)
	}
	if len(last["output"].([]any)) != 1 {
		t.Fatal("missing completed output")
	}
}

func TestResponsesStreamIncompleteAndFailed(t *testing.T) {
	for _, event := range []string{
		`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
		`{"type":"response.failed","response":{"status":"failed","error":{"type":"server_error","message":"broken"}}}`,
	} {
		var state any
		chunks, err := convertCodexResponseToOpenAIStream(context.Background(), "test", nil, nil, []byte("data: "+event+"\n\n"), &state)
		if err != nil {
			t.Fatal(err)
		}
		body := string(bytes.Join(chunks, nil))
		if strings.Contains(event, "incomplete") && !strings.Contains(body, `"finish_reason":"length"`) {
			t.Fatal(body)
		}
		if strings.Contains(event, "failed") && (!strings.Contains(body, `"error"`) || strings.Contains(body, `"finish_reason":"stop"`)) {
			t.Fatal(body)
		}
	}
}

func TestResponsesRefusalLifecycle(t *testing.T) {
	var state any
	var types []string
	for _, input := range []string{`{"choices":[{"delta":{"refusal":"I cannot "}}]}`, `{"choices":[{"delta":{"refusal":"help"}}]}`, `[DONE]`} {
		chunks, err := convertOpenAIResponseToCodexStream(context.Background(), "test", nil, nil, []byte("data: "+input+"\n\n"), &state)
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range chunks {
			_, raw := parseSSEEventBlock(string(chunk))
			typ := gjson.Get(raw, "type").String()
			types = append(types, typ)
			if typ == "response.completed" && gjson.Get(raw, "response.output.0.content.0.refusal").String() != "I cannot help" {
				t.Fatal(raw)
			}
		}
	}
	want := "response.created,response.output_item.added,response.content_part.added,response.refusal.delta,response.refusal.delta,response.refusal.done,response.content_part.done,response.output_item.done,response.completed"
	if strings.Join(types, ",") != want {
		t.Fatalf("unexpected lifecycle: %v", types)
	}
}

func BenchmarkResponsesLongContext(b *testing.B) {
	raw, err := json.Marshal(map[string]any{"input": strings.Repeat("context ", 32768)})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		if _, err := convertCodexRequestToOpenAI("test", raw, true); err != nil {
			b.Fatal(err)
		}
	}
}

func TestResponsesRejectsUnmappableRequestParameters(t *testing.T) {
	for _, field := range []string{`"seed":0`, `"stop":["END"]`, `"audio":{"voice":"alloy"}`, `"logit_bias":{"123":5}`, `"logprobs":true`} {
		_, err := convertOpenAIRequestToCodex("test", []byte(`{"messages":[{"role":"user","content":"hi"}],`+field+`}`), false)
		if err == nil {
			t.Fatalf("silently lost %s", field)
		}
	}
	for _, field := range []string{`"background":true`, `"max_tool_calls":2`} {
		_, err := convertCodexRequestToOpenAI("test", []byte(`{"input":"hi",`+field+`}`), false)
		if err == nil {
			t.Fatalf("silently lost %s", field)
		}
	}
	out, err := convertOpenAIRequestToCodex("test", []byte(`{"messages":[{"role":"user","content":"hi"}],"verbosity":"low"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "text.verbosity").String() != "low" {
		t.Fatal(string(out))
	}
}
