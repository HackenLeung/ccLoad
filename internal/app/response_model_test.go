package app

import (
	"testing"
)

func TestExtractResponseModel(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name:    "nil payload",
			payload: nil,
			want:    "",
		},
		{
			name:    "empty payload",
			payload: map[string]any{},
			want:    "",
		},
		{
			name:    "openai top-level model",
			payload: map[string]any{"model": "gpt-5.4"},
			want:    "gpt-5.4",
		},
		{
			name:    "gemini modelVersion",
			payload: map[string]any{"modelVersion": "gemini-2.5-pro"},
			want:    "gemini-2.5-pro",
		},
		{
			name:    "anthropic nested message.model",
			payload: map[string]any{"message": map[string]any{"model": "claude-sonnet-4-5"}},
			want:    "claude-sonnet-4-5",
		},
		{
			name:    "codex nested response.model",
			payload: map[string]any{"response": map[string]any{"model": "gpt-5-codex"}},
			want:    "gpt-5-codex",
		},
		{
			name:    "top-level wins over nested",
			payload: map[string]any{"model": "outer", "message": map[string]any{"model": "inner"}},
			want:    "outer",
		},
		{
			name:    "blank string ignored",
			payload: map[string]any{"model": "   "},
			want:    "",
		},
		{
			name:    "non-string model ignored",
			payload: map[string]any{"model": 123},
			want:    "",
		},
		{
			name:    "trims whitespace",
			payload: map[string]any{"model": "  gpt-5  "},
			want:    "gpt-5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractResponseModel(tt.payload); got != tt.want {
				t.Fatalf("extractResponseModel() = %q, want %q", got, tt.want)
			}
		})
	}
}
