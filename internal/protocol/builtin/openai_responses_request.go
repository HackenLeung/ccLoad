package builtin

import (
	"bytes"
	"encoding/json"
	"fmt"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
)

type codexInput []json.RawMessage

func (input *codexInput) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var text string
		if err := sonic.Unmarshal(data, &text); err != nil {
			return err
		}
		message, err := sonic.Marshal(map[string]string{"role": "user", "content": text})
		if err != nil {
			return err
		}
		*input = codexInput{message}
		return nil
	}
	var items []json.RawMessage
	if err := sonic.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("%w: responses input must be a string or array: %v", protocol.ErrUnsupportedRequestShape, err)
	}
	*input = items
	return nil
}

// Preserve only fields with matching semantics. Unknown fields must not leak
// from one wire protocol into the other.
func preserveOpenAIResponseRequestFields(source, encoded []byte, toResponses, stream bool) ([]byte, error) {
	var src, dst map[string]json.RawMessage
	if err := sonic.Unmarshal(source, &src); err != nil {
		return nil, err
	}
	if err := sonic.Unmarshal(encoded, &dst); err != nil {
		return nil, err
	}
	// Reject parameters with no equivalent semantics instead of silently dropping them.
	unsupported := []string{"background", "max_tool_calls"}
	if toResponses {
		unsupported = []string{"audio", "modalities", "prediction", "logit_bias", "seed", "frequency_penalty", "presence_penalty", "stop", "logprobs", "top_logprobs"}
	}
	for _, key := range unsupported {
		value := bytes.TrimSpace(src[key])
		if hasJSONValue(value) && (key == "seed" || (string(value) != "false" && string(value) != "0" && string(value) != "[]" && string(value) != "{}")) {
			return nil, fmt.Errorf("%w: %s has no supported equivalent in the target protocol", protocol.ErrUnsupportedRequestShape, key)
		}
	}
	for _, key := range []string{"store", "metadata", "service_tier", "prompt_cache_key", "prompt_cache_retention", "safety_identifier", "parallel_tool_calls"} {
		if value, ok := src[key]; ok {
			dst[key] = value
		}
	}
	if toResponses {
		if raw := src["n"]; len(raw) > 0 {
			var n int
			if err := sonic.Unmarshal(raw, &n); err != nil || n != 1 {
				return nil, fmt.Errorf("%w: Responses supports only n=1", protocol.ErrUnsupportedRequestShape)
			}
		}
		if raw := src["response_format"]; hasJSONValue(raw) {
			var format map[string]json.RawMessage
			if err := sonic.Unmarshal(raw, &format); err != nil {
				return nil, fmt.Errorf("%w: invalid response_format", protocol.ErrUnsupportedRequestShape)
			}
			if string(format["type"]) == `"json_schema"` {
				var schema map[string]json.RawMessage
				if err := sonic.Unmarshal(format["json_schema"], &schema); err != nil || schema == nil {
					return nil, fmt.Errorf("%w: missing json_schema", protocol.ErrUnsupportedRequestShape)
				}
				schema["type"] = json.RawMessage(`"json_schema"`)
				format = schema
			}
			value, err := marshalStableJSON(map[string]any{"format": format})
			if err != nil {
				return nil, err
			}
			dst["text"] = value
		}
		if verbosity, ok := src["verbosity"]; ok {
			var text map[string]json.RawMessage
			if hasJSONValue(dst["text"]) {
				if err := sonic.Unmarshal(dst["text"], &text); err != nil {
					return nil, err
				}
			}
			if text == nil {
				text = make(map[string]json.RawMessage)
			}
			text["verbosity"] = verbosity
			value, err := marshalStableJSON(text)
			if err != nil {
				return nil, err
			}
			dst["text"] = value
		}
	} else {
		for _, key := range []string{"previous_response_id", "conversation", "context_management"} {
			if hasJSONValue(src[key]) && string(src[key]) != `""` {
				return nil, fmt.Errorf("%w: %s requires native Responses or replayed full context", protocol.ErrUnsupportedRequestShape, key)
			}
		}
		if raw := src["text"]; hasJSONValue(raw) {
			var text map[string]json.RawMessage
			if err := sonic.Unmarshal(raw, &text); err != nil {
				return nil, err
			}
			if rawFormat := text["format"]; hasJSONValue(rawFormat) {
				var format map[string]json.RawMessage
				if err := sonic.Unmarshal(rawFormat, &format); err != nil {
					return nil, err
				}
				if string(format["type"]) == `"json_schema"` {
					delete(format, "type")
					value, err := marshalStableJSON(map[string]any{"type": "json_schema", "json_schema": format})
					if err != nil {
						return nil, err
					}
					dst["response_format"] = value
				} else {
					dst["response_format"] = rawFormat
				}
			}
			if value, ok := text["verbosity"]; ok {
				dst["verbosity"] = value
			}
		}
		if stream {
			dst["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		}
		// Use the modern OpenAI token limit; legacy compatible providers can
		// override this with channel request rules.
		if value, ok := src["max_output_tokens"]; ok {
			delete(dst, "max_tokens")
			dst["max_completion_tokens"] = value
		}
	}
	return marshalStableJSON(dst)
}

func functionToolStrict(fn map[string]any, source string) (*bool, error) {
	value, exists := fn["strict"]
	if !exists || value == nil {
		if source == "openai" {
			value = false
		} else {
			return nil, nil
		}
	}
	strict, ok := value.(bool)
	if !ok {
		return nil, fmt.Errorf("%w: tool strict must be boolean", protocol.ErrUnsupportedRequestShape)
	}
	return &strict, nil
}
