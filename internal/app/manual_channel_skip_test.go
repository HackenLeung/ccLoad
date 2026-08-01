package app

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

func TestRunProxyAttemptLoop_ManualSkipMovesToNextChannel(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	firstCanceled := make(chan struct{}, 1)
	var firstCalls atomic.Int32
	firstUpstream := newTestHTTPServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		firstStarted <- struct{}{}
		<-r.Context().Done()
		firstCanceled <- struct{}{}
	}))

	var unusedURLCalls atomic.Int32
	unusedURL := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unusedURLCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))

	var secondCalls atomic.Int32
	secondUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"from-second-channel"}`))
	}))

	srv := newInMemoryServer(t)
	// Keep URL order deterministic so this also proves a manual skip does not try the next URL.
	srv.urlSelector = nil
	srv.maxKeyRetries = 2

	createChannel := func(name, upstreamURL string, keys ...string) *model.Config {
		t.Helper()
		cfg, err := srv.store.CreateConfig(context.Background(), &model.Config{
			Name:        name,
			URL:         upstreamURL,
			Priority:    1,
			ChannelType: "openai",
			Enabled:     true,
			ModelEntries: []model.ModelEntry{
				{Model: "test-model"},
			},
		})
		if err != nil {
			t.Fatalf("CreateConfig(%s) error = %v", name, err)
		}

		apiKeys := make([]*model.APIKey, 0, len(keys))
		for index, key := range keys {
			apiKeys = append(apiKeys, &model.APIKey{
				ChannelID:   cfg.ID,
				KeyIndex:    index,
				APIKey:      key,
				KeyStrategy: model.KeyStrategySequential,
			})
		}
		if err := srv.store.CreateAPIKeysBatch(context.Background(), apiKeys); err != nil {
			t.Fatalf("CreateAPIKeysBatch(%s) error = %v", name, err)
		}
		srv.InvalidateAPIKeysCache(cfg.ID)
		return cfg
	}

	firstCfg := createChannel("blocking-first", firstUpstream.URL+"\n"+unusedURL.URL, "first-key", "second-key")
	secondCfg := createChannel("working-second", secondUpstream.URL, "working-key")

	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	clientReq := newRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	clientReq.Header.Set("Content-Type", "application/json")
	c, recorder := newTestContext(t, clientReq)

	activeID := srv.activeRequests.Register(time.Now(), "test-model", "127.0.0.1", false)
	reqCtx := &proxyRequestContext{
		originalModel:  "test-model",
		clientProtocol: protocol.OpenAI,
		requestMethod:  http.MethodPost,
		requestPath:    "/v1/chat/completions",
		body:           body,
		translatedBody: body,
		header:         clientReq.Header,
		activeReqID:    activeID,
		startTime:      time.Now(),
		clientIP:       "127.0.0.1",
	}

	type loopResult struct {
		last      *proxyResult
		succeeded bool
	}
	done := make(chan loopResult, 1)
	go func() {
		last, succeeded := srv.runProxyAttemptLoop(context.Background(), []*model.Config{firstCfg, secondCfg}, reqCtx, c.Writer)
		done <- loopResult{last: last, succeeded: succeeded}
	}()

	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first channel did not receive the request")
	}

	active := srv.activeRequests.List()
	if len(active) != 1 || active[0].AttemptID <= 0 || !active[0].CanSkip {
		t.Fatalf("active request is not skippable: %+v", active)
	}
	if active[0].ChannelID != firstCfg.ID {
		t.Fatalf("active channel ID = %d, want %d", active[0].ChannelID, firstCfg.ID)
	}
	if err := srv.activeRequests.RequestChannelSkip(activeID, active[0].AttemptID); err != nil {
		t.Fatalf("RequestChannelSkip() error = %v", err)
	}

	select {
	case result := <-done:
		if !result.succeeded || result.last != nil {
			t.Fatalf("runProxyAttemptLoop() = (%+v, %v), want (nil, true)", result.last, result.succeeded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy loop did not advance to the second channel")
	}

	select {
	case <-firstCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("manual skip did not cancel the first upstream request")
	}
	if firstCalls.Load() != 1 {
		t.Fatalf("first channel calls = %d, want 1", firstCalls.Load())
	}
	if unusedURLCalls.Load() != 0 {
		t.Fatalf("next URL in skipped channel was called %d times", unusedURLCalls.Load())
	}
	if secondCalls.Load() != 1 {
		t.Fatalf("second channel calls = %d, want 1", secondCalls.Load())
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "from-second-channel") {
		t.Fatalf("client response = %d %q, want success from second channel", recorder.Code, recorder.Body.String())
	}

	channelCooldowns, err := srv.store.GetAllChannelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns() error = %v", err)
	}
	if _, found := channelCooldowns[firstCfg.ID]; found {
		t.Fatalf("manual skip unexpectedly cooled channel %d", firstCfg.ID)
	}
	keyCooldowns, err := srv.store.GetAllKeyCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns() error = %v", err)
	}
	if _, found := keyCooldowns[firstCfg.ID]; found {
		t.Fatalf("manual skip unexpectedly cooled a key for channel %d", firstCfg.ID)
	}
}

func TestWriteFinalProxyResponse_ManualChannelSkipExhausted(t *testing.T) {
	srv := &Server{}
	c, recorder := newTestContext(t, newRequest(http.MethodPost, "/v1/chat/completions", nil))
	reqCtx := &proxyRequestContext{startTime: time.Now(), clientIP: "127.0.0.1"}

	srv.writeFinalProxyResponse(c, reqCtx, "test-model", false, manualChannelSkipResult(&model.Config{ID: 1}), 1)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(recorder.Body.String(), "manual channel skip exhausted available channels") {
		t.Fatalf("unexpected body: %s", recorder.Body.String())
	}
}
