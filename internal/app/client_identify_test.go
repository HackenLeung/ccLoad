package app

import (
	"net/http"
	"strings"
	"testing"
)

func TestClassifyClient(t *testing.T) {
	tests := []struct {
		name         string
		header       http.Header
		wantName     string
		wantUASubstr string
	}{
		{
			name:     "nil header",
			header:   nil,
			wantName: "",
		},
		{
			name:     "empty header",
			header:   http.Header{},
			wantName: "",
		},
		{
			name:         "claude code cli UA",
			header:       http.Header{"User-Agent": {"claude-cli/2.0.0 (external, cli)"}},
			wantName:     ClientNameClaudeCode,
			wantUASubstr: "claude-cli/2.0.0",
		},
		{
			name:     "codex originator header wins over UA",
			header:   http.Header{"User-Agent": {"curl/8.0"}, "Originator": {"codex_cli_rs"}},
			wantName: ClientNameCodexCLI,
		},
		{
			name:     "codex cli UA",
			header:   http.Header{"User-Agent": {"codex_cli_rs/0.21.0"}},
			wantName: ClientNameCodexCLI,
		},
		{
			name:     "gemini cli UA",
			header:   http.Header{"User-Agent": {"gemini-cli/0.4.0"}},
			wantName: ClientNameGeminiCLI,
		},
		{
			name:     "x-app header",
			header:   http.Header{"X-App": {"Cursor"}},
			wantName: ClientNameCursor,
		},
		{
			name:     "anthropic sdk",
			header:   http.Header{"User-Agent": {"Anthropic/Python 0.40.0"}},
			wantName: ClientNameAnthropicSDK,
		},
		{
			name:     "openai sdk",
			header:   http.Header{"User-Agent": {"OpenAI/Python 1.55.0"}},
			wantName: ClientNameOpenAISDK,
		},
		{
			name:     "gemini sdk",
			header:   http.Header{"User-Agent": {"google-genai-sdk/1.0.0 gl-python/3.12"}},
			wantName: ClientNameGeminiSDK,
		},
		{
			name:     "vscode UA",
			header:   http.Header{"User-Agent": {"vscode/1.95.0"}},
			wantName: ClientNameVSCode,
		},
		{
			name:     "cherry studio UA",
			header:   http.Header{"User-Agent": {"CherryStudio/1.2.3"}},
			wantName: ClientNameCherryStudio,
		},
		{
			name:     "windsurf UA",
			header:   http.Header{"User-Agent": {"Windsurf/1.0"}},
			wantName: ClientNameWindsurf,
		},
		{
			name:     "unknown UA falls back to other",
			header:   http.Header{"User-Agent": {"SomeRandomClient/9.9"}},
			wantName: ClientNameOther,
		},
		{
			name:     "blank UA yields empty name",
			header:   http.Header{"User-Agent": {"   "}},
			wantName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotUA := classifyClient(tt.header)
			if gotName != tt.wantName {
				t.Fatalf("classifyClient() name = %q, want %q", gotName, tt.wantName)
			}
			if tt.wantUASubstr != "" && !strings.Contains(gotUA, tt.wantUASubstr) {
				t.Fatalf("classifyClient() ua = %q, want to contain %q", gotUA, tt.wantUASubstr)
			}
		})
	}
}

func TestClassifyClientTruncatesUA(t *testing.T) {
	longUA := strings.Repeat("x", clientUAMaxStoreLen*2)
	_, ua := classifyClient(http.Header{"User-Agent": {longUA}})
	if len(ua) != clientUAMaxStoreLen {
		t.Fatalf("UA length = %d, want %d", len(ua), clientUAMaxStoreLen)
	}
}
