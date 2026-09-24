package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/model"

	"github.com/tidwall/gjson"
)

func TestProxy_UnsupportedTransformFallsBackWithoutCooldown(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "all unsupported"
		if native {
			name = "native fallback"
		}
		t.Run(name, func(t *testing.T) {
			channels := []testChannel{{name: "local", channelType: "codex", models: "test"}}
			if native {
				channels = append(channels, testChannel{name: "native", channelType: "openai", models: "test"})
			}
			env := setupProxyTestEnv(t, channels, map[int]string{0: "https://local.example.com", 1: "https://native.example.com"})
			ctx := context.Background()
			configs, err := env.store.ListConfigs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, cfg := range configs {
				if cfg.Name != "local" {
					continue
				}
				cfg.ProtocolTransforms = []string{"openai"}
				cfg.ProtocolTransformMode = model.ProtocolTransformModeLocal
				cfg.URL = "https://local.example.com\nhttps://local-backup.example.com"
				if _, err := env.store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
					t.Fatal(err)
				}
			}
			env.server.InvalidateChannelListCache()
			calls := 0
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "native.example.com" {
					t.Errorf("unsupported request reached %s", r.URL.Host)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				if !gjson.GetBytes(body, "seed").Exists() || gjson.GetBytes(body, "stop.0").String() != "END" {
					t.Errorf("request parameters lost: %s", body)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"native","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))}, nil
			})}
			w := doProxyRequest(t, env.engine, http.MethodPost, "/v1/chat/completions", map[string]any{"model": "test", "messages": []map[string]string{{"role": "user", "content": "hi"}}, "seed": 0, "stop": []string{"END"}}, nil)
			wantStatus, wantCalls := http.StatusBadRequest, 0
			if native {
				wantStatus, wantCalls = http.StatusOK, 1
			}
			if w.Code != wantStatus || calls != wantCalls {
				t.Fatalf("status=%d calls=%d, want %d/%d: %s", w.Code, calls, wantStatus, wantCalls, w.Body.String())
			}
			channelCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
			if err != nil {
				t.Fatal(err)
			}
			keyCooldowns, err := env.store.GetAllKeyCooldowns(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(channelCooldowns) != 0 || len(keyCooldowns) != 0 {
				t.Fatalf("conversion error caused cooldown: channels=%v keys=%v", channelCooldowns, keyCooldowns)
			}
			for _, cfg := range configs {
				if cfg.Name != "local" {
					continue
				}
				for _, url := range cfg.GetURLs() {
					if env.server.urlSelector.IsCooledDown(cfg.ID, url) {
						t.Fatalf("conversion error cooled URL %s", url)
					}
				}
			}
		})
	}
}
