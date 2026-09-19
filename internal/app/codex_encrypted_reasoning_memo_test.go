package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

const encryptedReasoningBody = `{
	"model":"gpt-5-codex",
	"prompt_cache_key":"memo-session",
	"stream":true,
	"input":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"prior"}],"encrypted_content":"blob"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]}
	]
}`

func TestCodexEncryptedReasoningMemoRoundTrip(t *testing.T) {
	t.Parallel()

	s := &Server{}
	key := codexEncryptedReasoningKey{channel: 7, revision: time.Now().UnixNano()}

	if got := s.codexEncryptedReasoningStrategy(key); got != "" {
		t.Fatalf("fresh memo strategy=%q, want empty", got)
	}

	s.rememberCodexEncryptedReasoningStrategy(key, []string{"strip_codex_encrypted_input"})
	if got := s.codexEncryptedReasoningStrategy(key); got != "strip_codex_encrypted_input" {
		t.Fatalf("memo strategy=%q, want strip_codex_encrypted_input", got)
	}

	// 渠道改配置（UpdatedAt 变化）后记忆必须失效，避免沿用旧上游的结论。
	edited := key
	edited.revision++
	if got := s.codexEncryptedReasoningStrategy(edited); got != "" {
		t.Fatalf("memo survived channel edit: %q", got)
	}
}

func TestCodexEncryptedReasoningMemoIgnoresThinkingStrategy(t *testing.T) {
	t.Parallel()

	s := &Server{}
	key := codexEncryptedReasoningKey{channel: 9}

	// strip_codex_thinking 说明上游不支持 reasoning 参数，与加密块无关，不该武装记忆。
	s.rememberCodexEncryptedReasoningStrategy(key, []string{"strip_codex_thinking"})
	if got := s.codexEncryptedReasoningStrategy(key); got != "" {
		t.Fatalf("memo armed by unrelated strategy: %q", got)
	}

	// 串联里含加密块策略时应取到它。
	s.rememberCodexEncryptedReasoningStrategy(key,
		[]string{"strip_codex_encrypted_content", "strip_codex_thinking"})
	if got := s.codexEncryptedReasoningStrategy(key); got != "strip_codex_encrypted_content" {
		t.Fatalf("memo strategy=%q, want strip_codex_encrypted_content", got)
	}
}

func TestCodexEncryptedReasoningMemoExpiresAndProbes(t *testing.T) {
	t.Parallel()

	s := &Server{}
	key := codexEncryptedReasoningKey{channel: 11}

	// 直接写入已过期条目，模拟 TTL 到期：读取应返回空（放行一次原样试探）并清理。
	s.codexEncryptedReasoningUnsupported.Store(
		key,
		codexEncryptedReasoningMemo{
			strategy: "strip_codex_encrypted_input",
			expiry:   time.Now().Add(-time.Minute),
		})

	if got := s.codexEncryptedReasoningStrategy(key); got != "" {
		t.Fatalf("expired memo strategy=%q, want empty so the next request probes", got)
	}
	if _, ok := s.codexEncryptedReasoningUnsupported.Load(key); ok {
		t.Fatal("expired memo was not evicted")
	}
}

func TestApplyCodexEncryptedReasoningMemoMatchesRetryShape(t *testing.T) {
	t.Parallel()

	body := []byte(encryptedReasoningBody)

	tests := []struct {
		strategy string
		retry    func([]byte) ([]byte, bool)
	}{
		{"strip_codex_encrypted_input", codexBodyWithoutEncryptedInputItems},
		{"strip_codex_encrypted_content", codexBodyWithoutEncryptedContent},
	}

	for _, tt := range tests {
		t.Run(tt.strategy, func(t *testing.T) {
			t.Parallel()

			want, ok := tt.retry(body)
			if !ok {
				t.Fatalf("%s: fixture no longer triggers the rewrite", tt.strategy)
			}
			got := applyCodexEncryptedReasoningMemo(tt.strategy, body)
			// 预剥离必须与重试产出完全一致的字节，否则首发/重试两种前缀轮流出现，
			// prompt cache 依然命中不了。
			if string(got) != string(want) {
				t.Fatalf("%s: memo body\n got=%s\nwant=%s", tt.strategy, got, want)
			}
		})
	}
}

func TestApplyCodexEncryptedReasoningMemoKeepsSummaryForContentStrategy(t *testing.T) {
	t.Parallel()

	got := applyCodexEncryptedReasoningMemo("strip_codex_encrypted_content", []byte(encryptedReasoningBody))
	text := string(got)
	if strings.Contains(text, "encrypted_content") {
		t.Fatalf("encrypted blob survived: %s", text)
	}
	// 只删字段的策略要保住 summary 文本，这是它比整条删除质量更好的唯一理由。
	if !strings.Contains(text, "prior") {
		t.Fatalf("reasoning summary was dropped: %s", text)
	}
}

func TestApplyCodexEncryptedReasoningMemoUnknownStrategyIsNoop(t *testing.T) {
	t.Parallel()

	body := []byte(encryptedReasoningBody)
	if got := applyCodexEncryptedReasoningMemo("strip_codex_thinking", body); string(got) != string(body) {
		t.Fatal("unknown strategy must leave the body untouched")
	}
	if got := applyCodexEncryptedReasoningMemo("", body); string(got) != string(body) {
		t.Fatal("empty strategy must leave the body untouched")
	}
}

func TestShouldApplyCodexEncryptedReasoningMemoScope(t *testing.T) {
	t.Parallel()

	if !shouldApplyCodexEncryptedReasoningMemo(protocol.Codex, "/v1/responses") {
		t.Fatal("native Codex responses request must be in scope")
	}
	if shouldApplyCodexEncryptedReasoningMemo(protocol.OpenAI, "/v1/responses") {
		t.Fatal("non-Codex upstream must be out of scope")
	}
	if shouldApplyCodexEncryptedReasoningMemo(protocol.Codex, "/v1/chat/completions") {
		t.Fatal("non-responses family must be out of scope")
	}
	if shouldApplyCodexEncryptedReasoningMemo(protocol.Codex, "/v1/responses/compact") {
		t.Fatal("compaction requests must be out of scope")
	}
}

func TestCodexEncryptedReasoningMemoNilSafe(t *testing.T) {
	t.Parallel()

	var s *Server
	key := codexEncryptedReasoningKey{channel: 1}
	if got := s.codexEncryptedReasoningStrategy(key); got != "" {
		t.Fatalf("nil server strategy=%q", got)
	}
	s.rememberCodexEncryptedReasoningStrategy(key, []string{"strip_codex_encrypted_input"})

	if _, ok := codexEncryptedReasoningMemoKey(nil, "", "", nil, protocol.TransformPlan{}); ok {
		t.Fatal("nil request must not have a memo key")
	}
}

func TestCodexEncryptedReasoningMemoKeyIsolation(t *testing.T) {
	t.Parallel()

	cfg := &model.Config{ID: 7, UpdatedAt: model.JSONTime{Time: time.Now()}}
	reqCtx := &proxyRequestContext{requestMethod: http.MethodPost, tokenHash: "client-a"}
	plan := protocol.TransformPlan{
		ClientProtocol: protocol.Codex, UpstreamProtocol: protocol.Codex,
		UpstreamPath: "/v1/responses", ActualModel: "gpt-5-codex", TranslatedBody: []byte(encryptedReasoningBody),
	}
	key, ok := codexEncryptedReasoningMemoKey(cfg, "key-a", "https://upstream-a.invalid", reqCtx, plan)
	if !ok {
		t.Fatal("encrypted reasoning fixture must allow memoization")
	}
	s := &Server{}
	s.rememberCodexEncryptedReasoningStrategy(key, []string{"strip_codex_encrypted_input"})

	tests := []struct {
		name   string
		mutate func(*model.Config, *proxyRequestContext, *protocol.TransformPlan)
	}{
		{"channel", func(c *model.Config, _ *proxyRequestContext, _ *protocol.TransformPlan) { c.ID++ }},
		{"revision", func(c *model.Config, _ *proxyRequestContext, _ *protocol.TransformPlan) {
			c.UpdatedAt.Time = c.UpdatedAt.Add(time.Second)
		}},
		{"client", func(_ *model.Config, r *proxyRequestContext, _ *protocol.TransformPlan) { r.tokenHash = "client-b" }},
		{"session", func(_ *model.Config, r *proxyRequestContext, _ *protocol.TransformPlan) {
			r.header = http.Header{"Session_id": {"other-session"}}
		}},
		{"model", func(_ *model.Config, _ *proxyRequestContext, p *protocol.TransformPlan) { p.ActualModel = "gpt-5.5" }},
		{"new reasoning", func(_ *model.Config, _ *proxyRequestContext, p *protocol.TransformPlan) {
			p.TranslatedBody = bytes.ReplaceAll(p.TranslatedBody, []byte("blob"), []byte("new-blob"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, r, p := cfg.Clone(), *reqCtx, plan
			tt.mutate(c, &r, &p)
			other, ok := codexEncryptedReasoningMemoKey(c, "key-a", "https://upstream-a.invalid", &r, p)
			if !ok || s.codexEncryptedReasoningStrategy(other) != "" {
				t.Fatal("another scope must not inherit the remembered strategy")
			}
		})
	}
	for _, target := range []struct{ apiKey, url string }{
		{"key-b", "https://upstream-a.invalid"},
		{"key-a", "https://upstream-b.invalid"},
	} {
		other, ok := codexEncryptedReasoningMemoKey(cfg, target.apiKey, target.url, reqCtx, plan)
		if !ok || s.codexEncryptedReasoningStrategy(other) != "" {
			t.Fatal("another Key or URL inherited the remembered strategy")
		}
	}

	for _, body := range []string{
		strings.ReplaceAll(encryptedReasoningBody, `"prompt_cache_key":"memo-session",`, ""),
		strings.ReplaceAll(encryptedReasoningBody, `"type":"reasoning"`, `"type":"compaction"`),
		`{"prompt_cache_key":"memo-session","input":[{"type":"reasoning","summary":[]}]}`,
	} {
		p := plan
		p.TranslatedBody = []byte(body)
		if _, ok := codexEncryptedReasoningMemoKey(cfg, "key-a", "https://upstream-a.invalid", reqCtx, p); ok {
			t.Fatalf("unsafe or unencrypted input must not use memo: %s", body)
		}
	}
	r := *reqCtx
	r.observer = &ForwardObserver{responsesSession: newResponsesWebsocketSession()}
	if _, ok := codexEncryptedReasoningMemoKey(cfg, "key-a", "https://upstream-a.invalid", &r, plan); ok {
		t.Fatal("WebSocket must preserve upstream-bound state")
	}
	plan.NeedsTransform = true
	if _, ok := codexEncryptedReasoningMemoKey(cfg, "key-a", "https://upstream-a.invalid", reqCtx, plan); ok {
		t.Fatal("local translation must not use native request memo")
	}
}

const memoTestSuccessBody = `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`
const memoTestRejectBody = `{"error":{"code":"invalid_encrypted_content","message":"could not decrypt"}}`

func newCodexMemoTestEnv(t *testing.T, handler http.Handler) *proxyTestEnv {
	t.Helper()
	upstream := newTestHTTPServer(t, handler)
	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-memo", channelType: "codex", models: "gpt-5-codex", apiKey: "key-a"},
	}, map[int]string{0: upstream.URL})
	env.server.allCooledWait = 0
	return env
}

func TestProxy_CodexMemoPreservesNewEncryptedContext(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	var lastBody atomic.Value
	env := newCodexMemoTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		lastBody.Store(body)
		w.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, memoTestRejectBody)
			return
		}
		_, _ = io.WriteString(w, memoTestSuccessBody)
	}))
	body := strings.ReplaceAll(encryptedReasoningBody, `"stream":true`, `"stream":false`)
	if w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/responses", json.RawMessage(body), nil); w.Code != http.StatusOK || attempts.Load() != 2 {
		t.Fatalf("initial retry: status=%d calls=%d", w.Code, attempts.Load())
	}

	for _, tt := range []struct{ name, body, want string }{
		{"new reasoning", strings.ReplaceAll(body, "blob", "valid-new-blob"), "valid-new-blob"},
		{"new session", strings.ReplaceAll(body, "memo-session", "other-session"), "blob"},
		{"no session", strings.ReplaceAll(body, `"prompt_cache_key":"memo-session",`, ""), "blob"},
		{"compaction", strings.ReplaceAll(body, `"input":[`, `"input":[{"type":"compaction","encrypted_content":"valid-compaction"},`), "valid-compaction"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := attempts.Load()
			w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/responses", json.RawMessage(tt.body), nil)
			if w.Code != http.StatusOK || attempts.Load() != before+1 {
				t.Fatalf("request: status=%d calls=%d", w.Code, attempts.Load()-before)
			}
			if got := lastBody.Load().([]byte); !bytes.Contains(got, []byte(tt.want)) {
				t.Fatalf("valid encrypted context was removed: %s", got)
			}
		})
	}
}

func TestProxy_CodexMemoKeepsFixedProbeDeadline(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	env := newCodexMemoTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if bytes.Contains(body, []byte(`"encrypted_content"`)) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, memoTestRejectBody)
			return
		}
		_, _ = io.WriteString(w, memoTestSuccessBody)
	}))
	body := strings.ReplaceAll(encryptedReasoningBody, `"stream":true`, `"stream":false`)
	request := func(body string, wantCalls int32) {
		t.Helper()
		before := attempts.Load()
		if w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/responses", json.RawMessage(body), nil); w.Code != http.StatusOK || attempts.Load()-before != wantCalls {
			t.Fatalf("request: status=%d calls=%d, want %d", w.Code, attempts.Load()-before, wantCalls)
		}
	}
	request(body, 2)
	var key codexEncryptedReasoningKey
	var memo codexEncryptedReasoningMemo
	env.server.codexEncryptedReasoningUnsupported.Range(func(k, v any) bool {
		key, memo = k.(codexEncryptedReasoningKey), v.(codexEncryptedReasoningMemo)
		return false
	})
	if memo.strategy == "" {
		t.Fatal("successful retry did not arm memo")
	}
	memo.expiry = time.Now().Add(time.Minute)
	env.server.codexEncryptedReasoningUnsupported.Store(key, memo)
	request(body, 1)
	request(`{"model":"gpt-5-codex","prompt_cache_key":"memo-session","input":"hello"}`, 1)
	cached, _ := env.server.codexEncryptedReasoningUnsupported.Load(key)
	if got := cached.(codexEncryptedReasoningMemo).expiry; !got.Equal(memo.expiry) {
		t.Fatalf("memo hit or ordinary request postponed the probe: %v -> %v", memo.expiry, got)
	}
	memo.expiry = time.Now().Add(-time.Minute)
	env.server.codexEncryptedReasoningUnsupported.Store(key, memo)
	request(body, 2)
	cached, _ = env.server.codexEncryptedReasoningUnsupported.Load(key)
	if got := cached.(codexEncryptedReasoningMemo).expiry; !got.After(time.Now()) {
		t.Fatal("expired memo was not rearmed after a successful retry")
	}
}

func TestProxy_CodexMemoRequiresCompleteResponse(t *testing.T) {
	t.Parallel()

	const delta = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
	for _, tt := range []struct {
		name, response string
		wantMemo       bool
	}{
		{"SSE error", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"message\":\"failed\"}}\n\n", false},
		{"incomplete stream", delta, false},
		{"completed stream", delta + "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + memoTestSuccessBody + "}\n\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			env := newCodexMemoTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if attempts.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, memoTestRejectBody)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.response)
			}))
			w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/responses", json.RawMessage(encryptedReasoningBody), nil)
			armed := false
			env.server.codexEncryptedReasoningUnsupported.Range(func(_, _ any) bool { armed = true; return false })
			if armed != tt.wantMemo {
				t.Fatalf("memo=%t, want %t; downstream=%d %s", armed, tt.wantMemo, w.Code, w.Body.String())
			}
			if attempts.Load() < 2 {
				t.Fatal("test did not exercise the retry")
			}
		})
	}
}
