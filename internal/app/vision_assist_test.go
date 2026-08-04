package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestProxyVisionAssistUsesSameChannelAndRewritesRequest(t *testing.T) {
	var visionCalls atomic.Int32
	var mainCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch {
		case strings.Contains(string(body), `"model":"vision-vl"`):
			visionCalls.Add(1)
			if !strings.Contains(string(body), "image_url") {
				t.Errorf("vision request missing image: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Screenshot OCR: DeepSeek is not a multimodal model."}}],"usage":{"prompt_tokens":10,"completion_tokens":8}}`))
		case strings.Contains(string(body), `"model":"deepseek-v4"`):
			mainCalls.Add(1)
			if strings.Contains(string(body), "image_url") || !strings.Contains(string(body), "DeepSeek is not a multimodal model") {
				t.Errorf("main request was not rewritten: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"explained"}}],"usage":{"prompt_tokens":20,"completion_tokens":4}}`))
		default:
			t.Errorf("unexpected request body: %s", body)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{name: "same-channel", models: "deepseek-v4,vision-vl"}}, map[int]string{0: upstream.URL})
	cfgs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(cfgs) != 1 {
		t.Fatalf("ListConfigs: len=%d err=%v", len(cfgs), err)
	}
	for i := range cfgs[0].ModelEntries {
		switch cfgs[0].ModelEntries[i].Model {
		case "deepseek-v4":
			cfgs[0].ModelEntries[i].VisionAssistEnabled = true
		case "vision-vl":
			cfgs[0].ModelEntries[i].VisionPoolEnabled = true
			cfgs[0].ModelEntries[i].VisionPriority = 100
		}
	}
	if _, err := env.store.UpdateConfig(context.Background(), cfgs[0].ID, cfgs[0]); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	env.server.InvalidateChannelListCache()

	response := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "deepseek-v4",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "What happened?"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
		}}},
	}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "explained") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if visionCalls.Load() != 1 || mainCalls.Load() != 1 {
		t.Fatalf("vision calls=%d main calls=%d", visionCalls.Load(), mainCalls.Load())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, listErr := env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceVisionAssist})
		if listErr == nil && len(logs) == 1 && logs[0].Model == "vision-vl" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("vision assist log was not persisted")
}

func TestProxyVisionAssistReusesDescriptionForSameImageAcrossRequests(t *testing.T) {
	var visionCalls atomic.Int32
	var mainCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), `"model":"vision-vl"`):
			visionCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"cached screenshot description"}}]}`))
		case strings.Contains(string(body), `"model":"deepseek-v4"`):
			mainCalls.Add(1)
			if strings.Contains(string(body), "image_url") || !strings.Contains(string(body), "cached screenshot description") {
				t.Errorf("main request did not use vision description: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		default:
			t.Errorf("unexpected request body: %s", body)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{name: "same-channel", models: "deepseek-v4,vision-vl"}}, map[int]string{0: upstream.URL})
	cfgs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(cfgs) != 1 {
		t.Fatalf("ListConfigs: len=%d err=%v", len(cfgs), err)
	}
	for i := range cfgs[0].ModelEntries {
		switch cfgs[0].ModelEntries[i].Model {
		case "deepseek-v4":
			cfgs[0].ModelEntries[i].VisionAssistEnabled = true
		case "vision-vl":
			cfgs[0].ModelEntries[i].VisionPoolEnabled = true
		}
	}
	if _, err := env.store.UpdateConfig(context.Background(), cfgs[0].ID, cfgs[0]); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	env.server.InvalidateChannelListCache()

	firstRequestBody := map[string]any{
		"model": "deepseek-v4",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "read this image"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
		}}},
	}
	secondRequestBody := map[string]any{
		"model": "deepseek-v4",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "read this image"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
			}},
			map[string]any{"role": "assistant", "content": "I will inspect the relevant files."},
			map[string]any{"role": "user", "content": "The previous Agent step returned the file contents; continue."},
		},
	}
	for i, requestBody := range []map[string]any{firstRequestBody, secondRequestBody} {
		response := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", requestBody, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i+1, response.Code, response.Body.String())
		}
	}
	if visionCalls.Load() != 1 || mainCalls.Load() != 2 {
		t.Fatalf("vision calls=%d main calls=%d", visionCalls.Load(), mainCalls.Load())
	}
}

func TestSameChannelVisionPoolPriority(t *testing.T) {
	t.Parallel()
	pool := sameChannelVisionPool(&model.Config{ModelEntries: []model.ModelEntry{
		{Model: "low", VisionPoolEnabled: true, VisionPriority: 1},
		{Model: "off", VisionPoolEnabled: false, VisionPriority: 999},
		{Model: "high", VisionPoolEnabled: true, VisionPriority: 20},
	}})
	if len(pool) != 2 || pool[0].Model != "high" || pool[1].Model != "low" {
		t.Fatalf("unexpected pool: %+v", pool)
	}
}

func TestProxyVisionAssistWithoutConfiguredPoolRemovesImages(t *testing.T) {
	var mainCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mainCalls.Add(1)
		if strings.Contains(string(body), "image_url") {
			t.Errorf("image was not removed: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{name: "text-only", models: "deepseek-v4"}}, map[int]string{0: upstream.URL})
	cfgs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(cfgs) != 1 {
		t.Fatalf("ListConfigs: len=%d err=%v", len(cfgs), err)
	}
	cfgs[0].ModelEntries[0].VisionAssistEnabled = true
	if _, err := env.store.UpdateConfig(context.Background(), cfgs[0].ID, cfgs[0]); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	env.server.InvalidateChannelListCache()

	response := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "deepseek-v4",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "read this"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
		}}},
	}, nil)
	if response.Code != http.StatusOK || mainCalls.Load() != 1 {
		t.Fatalf("status=%d main_calls=%d body=%s", response.Code, mainCalls.Load(), response.Body.String())
	}
}

func TestProxyVisionAssistFailureDoesNotRequeryAfterChannelSwitch(t *testing.T) {
	var visionFirstCalls, visionSecondCalls, firstMainCalls, secondMainCalls atomic.Int32
	firstUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"model":"vision-a"`) {
			visionFirstCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		firstMainCalls.Add(1)
		if strings.Contains(string(body), "image_url") {
			t.Errorf("first main request was not degraded (image not removed): %s", body)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer firstUpstream.Close()
	secondUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"model":"vision-b"`) {
			visionSecondCalls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		secondMainCalls.Add(1)
		if strings.Contains(string(body), "image_url") {
			t.Errorf("second main request was not degraded (image not removed): %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer secondUpstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "first", models: "deepseek-v4,vision-a", priority: 200},
		{name: "second", models: "deepseek-v4,vision-b", priority: 100},
	}, map[int]string{0: firstUpstream.URL, 1: secondUpstream.URL})
	cfgs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(cfgs) != 2 {
		t.Fatalf("ListConfigs: len=%d err=%v", len(cfgs), err)
	}
	for _, cfg := range cfgs {
		for i := range cfg.ModelEntries {
			cfg.ModelEntries[i].VisionAssistEnabled = cfg.ModelEntries[i].Model == "deepseek-v4"
			cfg.ModelEntries[i].VisionPoolEnabled = strings.HasPrefix(cfg.ModelEntries[i].Model, "vision-")
			if cfg.ModelEntries[i].Model == "vision-a" {
				cfg.ModelEntries[i].VisionPriority = 20
			}
			if cfg.ModelEntries[i].Model == "vision-b" {
				cfg.ModelEntries[i].VisionPriority = 10
			}
		}
		if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
			t.Fatalf("UpdateConfig: %v", err)
		}
	}
	env.server.InvalidateChannelListCache()

	response := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "deepseek-v4",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
		}}},
	}, nil)
	if response.Code != http.StatusOK || visionFirstCalls.Load() != 1 || visionSecondCalls.Load() != 1 || firstMainCalls.Load() != 1 || secondMainCalls.Load() != 1 {
		t.Fatalf("status=%d vision_first=%d vision_second=%d first_main=%d second_main=%d body=%s", response.Code, visionFirstCalls.Load(), visionSecondCalls.Load(), firstMainCalls.Load(), secondMainCalls.Load(), response.Body.String())
	}
}

func TestProxyVisionAssistDescriptionIsReusedAfterMainChannelSwitch(t *testing.T) {
	var visionCalls, firstMainCalls, secondMainCalls atomic.Int32
	firstUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"model":"vision-vl"`) {
			visionCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"reusable description"}}]}`))
			return
		}
		firstMainCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer firstUpstream.Close()
	secondUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "image_url") || !strings.Contains(string(body), "reusable description") {
			t.Errorf("second main request did not reuse description: %s", body)
		}
		secondMainCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer secondUpstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "first", models: "deepseek-v4,vision-vl", priority: 200},
		{name: "second", models: "deepseek-v4", priority: 100},
	}, map[int]string{0: firstUpstream.URL, 1: secondUpstream.URL})
	cfgs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(cfgs) != 2 {
		t.Fatalf("ListConfigs: len=%d err=%v", len(cfgs), err)
	}
	for _, cfg := range cfgs {
		for i := range cfg.ModelEntries {
			cfg.ModelEntries[i].VisionAssistEnabled = cfg.ModelEntries[i].Model == "deepseek-v4"
			cfg.ModelEntries[i].VisionPoolEnabled = cfg.ModelEntries[i].Model == "vision-vl"
		}
		if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
			t.Fatalf("UpdateConfig: %v", err)
		}
	}
	env.server.InvalidateChannelListCache()

	response := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model": "deepseek-v4",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
		}}},
	}, nil)
	if response.Code != http.StatusOK || visionCalls.Load() != 1 || firstMainCalls.Load() != 1 || secondMainCalls.Load() != 1 {
		t.Fatalf("status=%d vision=%d first_main=%d second_main=%d body=%s", response.Code, visionCalls.Load(), firstMainCalls.Load(), secondMainCalls.Load(), response.Body.String())
	}
}
