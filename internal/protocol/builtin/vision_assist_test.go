package builtin

import (
	"strings"
	"testing"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
)

func TestBuildAndRewriteVisionAssistOpenAIRequest(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"model":"deepseek-v4",
		"metadata":{"keep":true},
		"messages":[{"role":"user","content":[
			{"type":"text","text":"What error is shown?"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}
		]}]
	}`)

	visionBody, hasImages, err := BuildVisionAssistRequest(protocol.OpenAI, raw, "qwen-vl")
	if err != nil {
		t.Fatalf("BuildVisionAssistRequest: %v", err)
	}
	if !hasImages {
		t.Fatal("expected image detection")
	}
	if !strings.Contains(string(visionBody), `"model":"qwen-vl"`) || !strings.Contains(string(visionBody), "data:image/png") {
		t.Fatalf("unexpected vision body: %s", visionBody)
	}

	rewritten, err := RewriteImagesAsText(protocol.OpenAI, raw, "The screenshot shows error 400.")
	if err != nil {
		t.Fatalf("RewriteImagesAsText: %v", err)
	}
	if strings.Contains(string(rewritten), "image_url") || strings.Contains(string(rewritten), "data:image") {
		t.Fatalf("image was not removed: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), "The screenshot shows error 400") || !strings.Contains(string(rewritten), `"keep":true`) {
		t.Fatalf("description or unknown metadata missing: %s", rewritten)
	}
}

func TestRewriteImagesAsTextSupportedProtocols(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		protocol protocol.Protocol
		raw      string
		removed  string
	}{
		{
			name: "anthropic", protocol: protocol.Anthropic,
			raw:     `{"model":"deepseek","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"read"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}`,
			removed: `"type":"image"`,
		},
		{
			name: "codex", protocol: protocol.Codex,
			raw:     `{"model":"deepseek","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"read"},{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]}`,
			removed: `"type":"input_image"`,
		},
		{
			name: "gemini", protocol: protocol.Gemini,
			raw:     `{"contents":[{"role":"user","parts":[{"text":"read"},{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}]}]}`,
			removed: `"inlineData"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			visionBody, hasImages, err := BuildVisionAssistRequest(tt.protocol, []byte(tt.raw), "vision-model")
			if err != nil || !hasImages || len(visionBody) == 0 {
				t.Fatalf("build: hasImages=%v err=%v body=%s", hasImages, err, visionBody)
			}
			rewritten, err := RewriteImagesAsText(tt.protocol, []byte(tt.raw), "visible text")
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if strings.Contains(string(rewritten), tt.removed) || !strings.Contains(string(rewritten), "visible text") {
				t.Fatalf("unexpected rewritten body: %s", rewritten)
			}
			var valid any
			if err := sonic.Unmarshal(rewritten, &valid); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
		})
	}
}

func TestExtractVisionAssistText(t *testing.T) {
	t.Parallel()
	text, err := ExtractVisionAssistText([]byte(`{"choices":[{"message":{"content":"screen description"}}]}`))
	if err != nil || text != "screen description" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestRemoveImagesPreservesTextWithoutAddingMarker(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"metadata":{"keep":true},"messages":[{"role":"user","content":[{"type":"text","text":"read this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}]}]}`)
	rewritten, err := RemoveImages(protocol.OpenAI, raw)
	if err != nil {
		t.Fatalf("RemoveImages: %v", err)
	}
	text := string(rewritten)
	if strings.Contains(text, "image_url") || strings.Contains(text, "data:image") {
		t.Fatalf("image was not removed: %s", text)
	}
	if !strings.Contains(text, "read this") || strings.Contains(text, "Vision assistant") {
		t.Fatalf("text was not preserved silently: %s", text)
	}
}
