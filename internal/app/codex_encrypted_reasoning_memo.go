package app

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
)

// 只记住同一会话向同一上游发送同一组推理块时成功用过的剥离策略。
// 一次解密失败不代表整个渠道拒收加密状态；新推理块及 compaction 必须原样试送。
// 有效期从拒收后的成功重试起算，命中不续期，到期后恢复原样发送。
const (
	codexEncryptedReasoningMemoTTL     = 30 * time.Minute
	codexEncryptedReasoningMemoMaxSize = 256
)

type codexEncryptedReasoningKey struct {
	channel       int64
	revision      int64
	url           string
	keyHash       [32]byte
	model         string
	tokenHash     string
	sessionHash   [32]byte
	reasoningHash [32]byte
}

type codexEncryptedReasoningMemo struct {
	strategy string
	expiry   time.Time
}

func codexEncryptedReasoningMemoKey(cfg *model.Config, selectedKey, baseURL string, reqCtx *proxyRequestContext, plan protocol.TransformPlan) (codexEncryptedReasoningKey, bool) {
	if cfg == nil || reqCtx == nil || plan.NeedsTransform || plan.ClientProtocol != protocol.Codex ||
		reqCtx.requestMethod != http.MethodPost ||
		!shouldApplyCodexEncryptedReasoningMemo(plan.UpstreamProtocol, plan.UpstreamPath) ||
		(reqCtx.observer != nil && reqCtx.observer.responsesSession != nil) {
		return codexEncryptedReasoningKey{}, false
	}

	var payload struct {
		PromptCacheKey string            `json:"prompt_cache_key"`
		Input          []json.RawMessage `json:"input"`
	}
	if err := sonic.Unmarshal(plan.TranslatedBody, &payload); err != nil {
		return codexEncryptedReasoningKey{}, false
	}
	sessionID := strings.TrimSpace(reqCtx.header.Get("Session_id"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(payload.PromptCacheKey)
	}
	if sessionID == "" {
		return codexEncryptedReasoningKey{}, false
	}

	var reasoning []json.RawMessage
	hasEncryptedReasoning := false
	for _, raw := range payload.Input {
		var item struct {
			Type             string          `json:"type"`
			EncryptedContent json.RawMessage `json:"encrypted_content"`
		}
		if err := sonic.Unmarshal(raw, &item); err != nil || item.Type == "compaction" ||
			(item.Type != "reasoning" && len(item.EncryptedContent) > 0) {
			return codexEncryptedReasoningKey{}, false
		}
		if item.Type == "reasoning" {
			reasoning = append(reasoning, raw)
			var encrypted string
			if sonic.Unmarshal(item.EncryptedContent, &encrypted) == nil && encrypted != "" {
				hasEncryptedReasoning = true
			}
		}
	}
	if !hasEncryptedReasoning {
		return codexEncryptedReasoningKey{}, false
	}
	encoded, err := sonic.Marshal(reasoning)
	if err != nil {
		return codexEncryptedReasoningKey{}, false
	}
	return codexEncryptedReasoningKey{
		channel: cfg.ID, revision: cfg.UpdatedAt.UnixNano(),
		url:     buildUpstreamURL(baseURL, plan.UpstreamPath, reqCtx.rawQuery),
		keyHash: sha256.Sum256([]byte(selectedKey)), model: plan.RequestModel(),
		tokenHash: reqCtx.tokenHash, sessionHash: sha256.Sum256([]byte(sessionID)),
		reasoningHash: sha256.Sum256(encoded),
	}, true
}

// codexEncryptedReasoningStrategy 返回此前成功用过的策略；过期后恢复原样发送。
func (s *Server) codexEncryptedReasoningStrategy(key codexEncryptedReasoningKey) string {
	if s == nil {
		return ""
	}
	cached, ok := s.codexEncryptedReasoningUnsupported.Load(key)
	if !ok {
		return ""
	}
	memo, ok := cached.(codexEncryptedReasoningMemo)
	if !ok || !time.Now().Before(memo.expiry) {
		s.codexEncryptedReasoningUnsupported.CompareAndDelete(key, cached)
		return ""
	}
	return memo.strategy
}

// rememberCodexEncryptedReasoningStrategy 在剥离加密推理块后重试成功时武装记忆。
// strategies 是本次成功用到的策略串联，取其中与加密推理块相关的那个。
func (s *Server) rememberCodexEncryptedReasoningStrategy(key codexEncryptedReasoningKey, strategies []string) {
	if s == nil {
		return
	}
	strategy := ""
	for _, candidate := range strategies {
		if isCodexEncryptedReasoningStrategy(candidate) {
			strategy = candidate
			break
		}
	}
	if strategy == "" {
		return
	}

	now := time.Now()
	s.codexEncryptedReasoningUnsupported.Range(func(k, v any) bool {
		memo, ok := v.(codexEncryptedReasoningMemo)
		if !ok || now.After(memo.expiry) {
			s.codexEncryptedReasoningUnsupported.CompareAndDelete(k, v)
		}
		return true
	})
	count := 0
	s.codexEncryptedReasoningUnsupported.Range(func(k, _ any) bool {
		count++
		if count >= codexEncryptedReasoningMemoMaxSize {
			s.codexEncryptedReasoningUnsupported.Delete(k)
		}
		return true
	})

	s.codexEncryptedReasoningUnsupported.Store(
		key,
		codexEncryptedReasoningMemo{strategy: strategy, expiry: now.Add(codexEncryptedReasoningMemoTTL)},
	)
}

// isCodexEncryptedReasoningStrategy 认定哪些策略是「上游拒收加密推理块」的证据。
// strip_codex_thinking 是另一回事（上游不支持 reasoning 参数本身），不武装记忆。
func isCodexEncryptedReasoningStrategy(strategy string) bool {
	switch strategy {
	case "strip_codex_encrypted_input", "strip_codex_encrypted_content":
		return true
	default:
		return false
	}
}

// applyCodexEncryptedReasoningMemo 在已知上游拒收时，按记住的策略预先剥离。
// 复用重试路径的同一函数，保证首发与重试产出同一形状（前缀一致才谈得上 cache 命中）。
func applyCodexEncryptedReasoningMemo(strategy string, body []byte) []byte {
	var (
		stripped []byte
		ok       bool
	)
	switch strategy {
	case "strip_codex_encrypted_input":
		stripped, ok = codexBodyWithoutEncryptedInputItems(body)
	case "strip_codex_encrypted_content":
		stripped, ok = codexBodyWithoutEncryptedContent(body)
	default:
		return body
	}
	if !ok {
		return body
	}
	return stripped
}

// shouldApplyCodexEncryptedReasoningMemo 不把普通 Responses 的结论复用到 compact。
func shouldApplyCodexEncryptedReasoningMemo(
	upstreamProtocol protocol.Protocol,
	requestPath string,
) bool {
	return upstreamProtocol == protocol.Codex &&
		strings.TrimSuffix(requestPath, "/") == "/v1/responses"
}
