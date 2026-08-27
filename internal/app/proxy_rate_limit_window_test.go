package app

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/util"
)

// museaiRateLimitBody 复刻线上 429 原文案：code 为空、type 为站点自定义，
// 只有 message 里带着限流窗口长度。
const museaiRateLimitBody = `{"error":{"code":"","message":"您已达到请求数限制: 1分钟内最多请求5次 (request id: 20260827035010740)","type":"museai_error"}}`

// TestShortWindowRateLimitCooldownMatchesWindow 验证短窗口 429 按窗口长度冷却渠道，
// 而不是落到 1min→2→4→8→30min 的指数退避（上游 60 秒就恢复，不该封半小时）。
func TestShortWindowRateLimitCooldownMatchesWindow(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(museaiRateLimitBody))
	}))

	env := setupProxyTestEnv(t, []testChannel{
		{name: "museai", channelType: util.ChannelTypeOpenAI, models: "gpt-4o"},
	}, map[int]string{0: upstream.URL})
	// 隔离变量：本测试只验证冷却时长，不要让「全冷却等待」重试并二次 bump 冷却。
	env.server.allCooledWait = 0

	before := time.Now()
	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d, want 429（透传上游状态码）", w.Code)
	}

	ctx := context.Background()
	cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if len(cooldowns) != 1 {
		t.Fatalf("期望 1 个渠道进入冷却，实际 %d 个：%v", len(cooldowns), cooldowns)
	}

	for channelID, until := range cooldowns {
		got := until.Sub(before)
		// 窗口是 1 分钟；容差 5s 覆盖 unix 秒截断与请求耗时。
		if got < 55*time.Second || got > 65*time.Second {
			t.Errorf("渠道 %d 冷却时长 = %v, want 约 1m（按上游声明的限流窗口）", channelID, got)
		}
	}
}

// TestShortWindowRateLimitFallsThroughToNextChannel 验证短窗口 429 会继续顺到下一个渠道，
// 而不是直接把 429 返回给客户端。
func TestShortWindowRateLimitFallsThroughToNextChannel(t *testing.T) {
	var limitedHits atomic.Int32
	limited := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		limitedHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(museaiRateLimitBody))
	}))

	var healthyHits atomic.Int32
	healthy := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))

	env := setupProxyTestEnv(t, []testChannel{
		{name: "rate-limited", channelType: util.ChannelTypeOpenAI, models: "gpt-4o", priority: 200},
		{name: "healthy", channelType: util.ChannelTypeOpenAI, models: "gpt-4o", priority: 100},
	}, map[int]string{0: limited.URL, 1: healthy.URL})

	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200（应顺到健康渠道），body=%s", w.Code, w.Body.String())
	}
	if limitedHits.Load() == 0 {
		t.Error("被限流的高优先级渠道应先被尝试")
	}
	if healthyHits.Load() != 1 {
		t.Errorf("健康渠道命中次数 = %d, want 1", healthyHits.Load())
	}
}

// TestAllCooledWaitRetriesAfterCooldownExpires 验证「候选全冷却时等一轮再试」：
// 唯一渠道先返回 429（Retry-After: 2 → 冷却 2 秒），等待预算内应等到恢复并重试成功。
func TestAllCooledWaitRetriesAfterCooldownExpires(t *testing.T) {
	var hits atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))

	env := setupProxyTestEnv(t, []testChannel{
		{name: "only", channelType: util.ChannelTypeOpenAI, models: "gpt-4o"},
	}, map[int]string{0: upstream.URL})
	env.server.allCooledWait = 30 * time.Second

	start := time.Now()
	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200（等待冷却结束后应重试成功），body=%s", w.Code, w.Body.String())
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("上游命中次数 = %d, want 2（首次 429 + 等待后重试）", got)
	}
	if elapsed < time.Second {
		t.Errorf("耗时 = %v，应包含等待冷却的时间", elapsed)
	}
}

// TestAllCooledWaitDisabledReturnsImmediately 验证等待预算为 0 时行为不变：
// 直接把上游 429 透传给客户端，不做额外重试。
func TestAllCooledWaitDisabledReturnsImmediately(t *testing.T) {
	var hits atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))

	env := setupProxyTestEnv(t, []testChannel{
		{name: "only", channelType: util.ChannelTypeOpenAI, models: "gpt-4o"},
	}, map[int]string{0: upstream.URL})
	env.server.allCooledWait = 0

	start := time.Now()
	w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d, want 429（关闭等待时应直接透传）", w.Code)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("上游命中次数 = %d, want 1（不应重试）", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("耗时 = %v，关闭等待后应立即返回", elapsed)
	}
}
