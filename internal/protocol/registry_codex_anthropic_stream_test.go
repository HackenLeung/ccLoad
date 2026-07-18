package protocol_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

func TestRegistryCodexAnthropicStream(t *testing.T) {
	t.Parallel()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	t.Run("message done fallback emits text when no delta arrived", func(t *testing.T) {
		var state any
		out, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Anthropic,
			"claude-3-5-sonnet",
			nil,
			nil,
			[]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\n"),
			&state,
		)
		if err != nil {
			t.Fatalf("message done failed: %v", err)
		}
		body := string(bytes.Join(out, nil))
		if !strings.Contains(body, `"text_delta"`) || !strings.Contains(body, `"hello"`) {
			t.Fatalf("unexpected anthropic fallback stream: %s", body)
		}
	})

	t.Run("message done fallback ignores duplicate text after delta", func(t *testing.T) {
		var state any
		if _, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Anthropic,
			"claude-3-5-sonnet",
			nil,
			nil,
			[]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"),
			&state,
		); err != nil {
			t.Fatalf("delta failed: %v", err)
		}
		out, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Anthropic,
			"claude-3-5-sonnet",
			nil,
			nil,
			[]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\n"),
			&state,
		)
		if err != nil {
			t.Fatalf("duplicate message done failed: %v", err)
		}
		if out != nil {
			t.Fatalf("expected duplicate fallback to be ignored, got %#v", out)
		}
	})

	t.Run("reasoning summary emits thinking block with cached signature", func(t *testing.T) {
		var state any
		chunks := []string{
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_1\"}}\n\n",
			"event: response.reasoning_summary_part.added\ndata: {\"type\":\"response.reasoning_summary_part.added\"}\n\n",
			"event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"step by step\"}\n\n",
			"event: response.reasoning_summary_part.done\ndata: {\"type\":\"response.reasoning_summary_part.done\"}\n\n",
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}\n\n",
		}
		var joined bytes.Buffer
		for _, chunk := range chunks {
			out, err := reg.TranslateResponseStream(
				context.Background(),
				protocol.Codex,
				protocol.Anthropic,
				"claude-3-5-sonnet",
				nil,
				nil,
				[]byte(chunk),
				&state,
			)
			if err != nil {
				t.Fatalf("chunk failed: %v", err)
			}
			for _, b := range out {
				joined.Write(b)
			}
		}
		body := joined.String()
		if !strings.Contains(body, `"type":"thinking"`) || !strings.Contains(body, `"thinking_delta"`) || !strings.Contains(body, `"signature_delta"`) || !strings.Contains(body, `"signature":"enc_sig_1"`) || !strings.Contains(body, `event: content_block_stop`) {
			t.Fatalf("unexpected anthropic reasoning stream: %s", body)
		}
	})

	t.Run("function call string arguments stay raw json", func(t *testing.T) {
		var state any
		out, err := reg.TranslateResponseStream(
			context.Background(),
			protocol.Codex,
			protocol.Anthropic,
			"claude-3-5-sonnet",
			nil,
			nil,
			[]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_skill_1\",\"name\":\"Skill\",\"arguments\":\"{\\\"args\\\":\\\"skill: \\\\\\\"superpowers:using-superpowers\\\\\\\"\\\",\\\"skill\\\":\\\"superpowers:using-superpowers\\\"}\"}}\n\n"),
			&state,
		)
		if err != nil {
			t.Fatalf("function_call done failed: %v", err)
		}
		body := string(bytes.Join(out, nil))
		if !strings.Contains(body, `"partial_json":"{\"args\":\"skill: \\\"superpowers:using-superpowers\\\"\",\"skill\":\"superpowers:using-superpowers\"}"`) {
			t.Fatalf("expected raw object json in partial_json, got: %s", body)
		}
	})

	t.Run("web search call emits anthropic server tool result and usage", func(t *testing.T) {
		var state any
		chunks := []string{
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\",\"status\":\"completed\",\"action\":{\"type\":\"search\",\"query\":\"latest bun release\"},\"sources\":[{\"url\":\"https://bun.sh/blog\",\"title\":\"Bun Blog\"}]}}\n\n",
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":9,\"output_tokens\":3,\"total_tokens\":12}}}\n\n",
		}
		var joined bytes.Buffer
		for _, chunk := range chunks {
			out, err := reg.TranslateResponseStream(
				context.Background(),
				protocol.Codex,
				protocol.Anthropic,
				"claude-3-5-sonnet",
				nil,
				nil,
				[]byte(chunk),
				&state,
			)
			if err != nil {
				t.Fatalf("chunk failed: %v", err)
			}
			for _, b := range out {
				joined.Write(b)
			}
		}
		body := joined.String()
		for _, want := range []string{
			`"type":"server_tool_use"`,
			`"partial_json":"{\"query\":\"latest bun release\"}"`,
			`"type":"web_search_tool_result"`,
			`"type":"web_search_result"`,
			`"web_search_requests":1`,
			`"stop_reason":"end_turn"`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("expected %s in stream, got: %s", want, body)
			}
		}
	})

	t.Run("non-search web search action is ignored", func(t *testing.T) {
		var state any
		chunks := []string{
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\",\"status\":\"completed\",\"action\":{\"type\":\"open_page\",\"url\":\"https://bun.sh/blog\"}}}\n\n",
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"opened page\"}\n\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":9,\"output_tokens\":3,\"total_tokens\":12}}}\n\n",
		}
		var joined bytes.Buffer
		for _, chunk := range chunks {
			out, err := reg.TranslateResponseStream(
				context.Background(),
				protocol.Codex,
				protocol.Anthropic,
				"claude-3-5-sonnet",
				nil,
				nil,
				[]byte(chunk),
				&state,
			)
			if err != nil {
				t.Fatalf("chunk failed: %v", err)
			}
			for _, b := range out {
				joined.Write(b)
			}
		}
		body := joined.String()
		if strings.Contains(body, `"server_tool_use"`) || strings.Contains(body, `"web_search_tool_result"`) || strings.Contains(body, `"web_search_requests"`) {
			t.Fatalf("non-search web_search_call must not become anthropic search blocks or usage: %s", body)
		}
		if !strings.Contains(body, `"opened page"`) {
			t.Fatalf("expected text delta to remain, got: %s", body)
		}
	})
}
