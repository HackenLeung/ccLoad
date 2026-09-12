package app

import (
	"bytes"
	"encoding/json"
	"sort"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

// 请求体改写路径必须输出 byte-level 稳定的 JSON：这些函数都是
// map[string]any 进、重新序列化出，若用默认 sonic.Marshal（SortMapKeys=false），
// Go map 的随机迭代顺序会让每次请求产出不同字节，直接打掉上游 prompt cache 的
// prefix 命中。缓存读单价通常是 input 的一折，Codex 长会话下这是真实成本。
//
// 断言 key 有序而非「跑两次一样」：后者在 key 少时有很大概率偶然通过。
func assertTopLevelKeysSorted(t *testing.T, label string, body []byte) {
	t.Helper()

	// 用 encoding/json 的流式 Token 按文档顺序读顶层 key（普通 Unmarshal 到 map 会丢顺序）。
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("%s: parse rewritten body: %v", label, err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		t.Fatalf("%s: rewritten body is not a JSON object", label)
	}

	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("%s: scan rewritten body: %v", label, err)
		}
		key, ok := tok.(string)
		if !ok {
			t.Fatalf("%s: expected top-level key, got %T", label, tok)
		}
		keys = append(keys, key)
		if err := skipJSONValue(dec); err != nil {
			t.Fatalf("%s: skip value of %q: %v", label, key, err)
		}
	}

	if !sort.StringsAreSorted(keys) {
		t.Fatalf("%s: top-level keys not sorted (prompt cache prefix unstable): %v", label, keys)
	}
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// codexRewriteBody 覆盖多个顶层 key，字母序与常见书写顺序不一致，
// 未排序时乱序概率高。
const codexRewriteBody = `{
	"model":"gpt-5-codex",
	"stream":true,
	"instructions":"be brief",
	"prompt_cache_key":"cache-1",
	"reasoning":{"effort":"medium","summary":"auto"},
	"include":["reasoning.encrypted_content","file_search_call.results"],
	"tool_choice":{"type":"web_search"},
	"tools":[
		{"type":"web_search"},
		{"type":"function","name":"shell"},
		{"type":"tool_search"}
	],
	"input":[
		{"type":"reasoning","summary":[],"encrypted_content":"blob"},
		{"type":"tool_search_call","arguments":"{\"q\":\"x\"}"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]}
	]
}`

func TestCodexBodyRewritesKeepPromptCachePrefixStable(t *testing.T) {
	t.Parallel()

	body := []byte(codexRewriteBody)
	tests := []struct {
		name    string
		rewrite func([]byte) ([]byte, bool)
	}{
		{"codexBodyWithoutEncryptedInputItems", codexBodyWithoutEncryptedInputItems},
		{"codexBodyWithoutThinking", codexBodyWithoutThinking},
		{"codexBodyWithoutPromptCache", codexBodyWithoutPromptCache},
		{"codexBodyWithoutHostedWebSearch", codexBodyWithoutHostedWebSearch},
		{"normalizeCodexToolSearchInputItems", normalizeCodexToolSearchInputItems},
		{"codexBodyWithoutToolSearchOnlyInputItems", codexBodyWithoutToolSearchOnlyInputItems},
		{"codexBodyWithoutEncryptedContent", codexBodyWithoutEncryptedContent},
		{"codexBodyWithoutDisabledToolCapabilities", func(b []byte) ([]byte, bool) {
			return codexBodyWithoutDisabledToolCapabilities(b, false, false)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := tt.rewrite(body)
			if !ok {
				t.Fatalf("%s returned ok=false, fixture no longer triggers the rewrite", tt.name)
			}
			assertTopLevelKeysSorted(t, tt.name, got)
		})
	}
}

func TestProxyBodyRewritesKeepPromptCachePrefixStable(t *testing.T) {
	t.Parallel()

	anthropicBody := []byte(`{
		"model":"claude-sonnet-4-5",
		"stream":true,
		"max_tokens":1024,
		"thinking":{"type":"enabled","budget_tokens":8192},
		"system":[{"type":"text","text":"keep"}],
		"messages":[{"role":"user","content":"hi"}]
	}`)

	cfg := &model.Config{Name: "anyrouter-main", ChannelType: "anthropic"}
	got := normalizeAnyrouterAdaptiveThinking(cfg, "/v1/messages", anthropicBody)
	assertTopLevelKeysSorted(t, "normalizeAnyrouterAdaptiveThinking", got)

	rules := []model.CustomBodyRule{{Action: "remove", Path: "stream"}}
	got = applyBodyRules("application/json", anthropicBody, rules)
	assertTopLevelKeysSorted(t, "applyBodyRules", got)
}

func TestPrepareRequestBodyKeepsPromptCachePrefixStable(t *testing.T) {
	t.Parallel()

	s := &Server{}
	cfg := &model.Config{
		// 旧语义：Model 为对外模型，RedirectModel 为上游模型。
		ModelEntries: []model.ModelEntry{
			{Model: "gpt-5-codex", RedirectModel: "gpt-5.4-codex"},
		},
	}
	reqCtx := &proxyRequestContext{
		originalModel:  "gpt-5-codex",
		clientProtocol: protocol.Codex,
		body:           []byte(codexRewriteBody),
	}

	actualModel, bodyToSend := s.prepareRequestBody(cfg, reqCtx)
	if actualModel != "gpt-5.4-codex" {
		t.Fatalf("actualModel=%q, want redirected model", actualModel)
	}
	assertTopLevelKeysSorted(t, "prepareRequestBody", bodyToSend)
}
