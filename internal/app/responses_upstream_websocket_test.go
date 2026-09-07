package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestResponsesWSNativeReuseAndAccounting(t *testing.T) {
	var connections atomic.Int32
	requests := make(chan []byte, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for turn := 1; turn <= 2; turn++ {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			requests <- payload
			if err := conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"r%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":10,"output_tokens":3}}}`, turn))); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	cfg := &model.Config{ID: 1, Name: "ws", ChannelType: "codex", ResponsesTransport: model.ResponsesTransportPreferWebsocket}
	session := newResponsesWebsocketSession()
	defer session.closeUpstream()
	body := []byte(`{"model":"test","input":[{"role":"user","content":"hi"}],"stream":true}`)
	for turn := 1; turn <= 2; turn++ {
		if turn == 2 {
			body = []byte(`{"model":"test","input":[{"role":"user","content":"hi"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"role":"user","content":"next"}],"stream":true}`)
		}
		plan := mustBuildTestTransformPlan(t, cfg, "/v1/responses", body)
		w := httptest.NewRecorder()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, _, err := srv.forwardOnceAsync(ctx, cfg, "test-key", http.MethodPost, plan, http.Header{"Content-Type": {"application/json"}}, "", upstream.URL, w, &ForwardObserver{responsesSession: session})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != 200 || res.InputTokens != 10 || res.OutputTokens != 3 {
			t.Fatalf("unexpected result: %+v", res)
		}
		if turn == 2 && !strings.Contains(res.TransportInfo, "reused=true continuation=true") {
			t.Fatalf("connection not reused: %s", res.TransportInfo)
		}
		payload := <-requests
		if gjson.GetBytes(payload, "stream").Exists() || gjson.GetBytes(payload, "type").String() != "response.create" {
			t.Fatalf("invalid WS request: %s", payload)
		}
		if turn == 2 && (gjson.GetBytes(payload, "previous_response_id").String() != "r1" || len(gjson.GetBytes(payload, "input").Array()) != 1) {
			t.Fatalf("not incremental: %s", payload)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("connections=%d", connections.Load())
	}
}

func TestResponsesWSTransportFallbackBoundaries(t *testing.T) {
	for _, status := range []int{404, 405, 426, 501, 401, 403, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var upgrades, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					upgrades.Add(1)
					w.WriteHeader(status)
					return
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"output\":[]}}\n\n")
			}))
			defer upstream.Close()
			srv := &Server{client: upstream.Client()}
			cfg := &model.Config{ID: 1, ResponsesTransport: model.ResponsesTransportPreferWebsocket}
			for i := 0; i < 2; i++ {
				session := newResponsesWebsocketSession()
				req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/v1/responses", strings.NewReader(`{"model":"test","input":[],"stream":true}`))
				resp, _, _, err := srv.doResponsesUpstreamRequest(cfg, req, session)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				session.closeUpstream()
			}
			if responsesWSUnsupportedStatus(status) {
				if upgrades.Load() != 1 || posts.Load() != 2 {
					t.Fatalf("upgrade=%d post=%d", upgrades.Load(), posts.Load())
				}
			} else if upgrades.Load() != 2 || posts.Load() != 0 {
				t.Fatalf("auth/server error fell back: upgrade=%d post=%d", upgrades.Load(), posts.Load())
			}
		})
	}
}

func TestResponsesWSDisconnectNeverReplays(t *testing.T) {
	var accepted atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err == nil {
			accepted.Add(1)
		}
	}))
	defer upstream.Close()
	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	cfg := &model.Config{ID: 1, ChannelType: "codex", ResponsesTransport: model.ResponsesTransportPreferWebsocket}
	body := []byte(`{"model":"test","input":[],"stream":true}`)
	session := newResponsesWebsocketSession()
	defer session.closeUpstream()
	res, _, err := srv.forwardOnceAsync(context.Background(), cfg, "key", http.MethodPost, mustBuildTestTransformPlan(t, cfg, "/v1/responses", body), http.Header{}, "", upstream.URL, httptest.NewRecorder(), &ForwardObserver{responsesSession: session})
	if res == nil || !res.NoRetry || accepted.Load() != 1 {
		t.Fatalf("unsafe retry result=%+v err=%v accepted=%d", res, err, accepted.Load())
	}
}

func TestResponsesWSOnlyAndHTTPModes(t *testing.T) {
	for _, mode := range []string{model.ResponsesTransportHTTP, model.ResponsesTransportWebsocketOnly} {
		t.Run(mode, func(t *testing.T) {
			var upgrades, posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					upgrades.Add(1)
					w.WriteHeader(426)
				} else {
					posts.Add(1)
					w.WriteHeader(200)
				}
			}))
			defer upstream.Close()
			srv := &Server{client: upstream.Client()}
			session := newResponsesWebsocketSession()
			defer session.closeUpstream()
			req, _ := http.NewRequest(http.MethodPost, upstream.URL, strings.NewReader(`{"model":"test","input":[]}`))
			resp, _, _, err := srv.doResponsesUpstreamRequest(&model.Config{ID: 1, ResponsesTransport: mode}, req, session)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if mode == model.ResponsesTransportHTTP && (posts.Load() != 1 || upgrades.Load() != 0) {
				t.Fatal("HTTP mode attempted websocket")
			}
			if mode == model.ResponsesTransportWebsocketOnly && (posts.Load() != 0 || upgrades.Load() != 1) {
				t.Fatal("WS-only fell back")
			}
		})
	}
}

func TestResponsesWSRejectUnsafeReplay(t *testing.T) {
	session := newResponsesWebsocketSession()
	if _, err := session.normalizeRequest([]byte(`{"type":"response.create","model":"test","previous_response_id":"unknown","input":[]}`)); err == nil {
		t.Fatal("unknown previous ID silently dropped")
	}
	session.upstream = &responsesWSConnection{}
	req, _ := http.NewRequest(http.MethodPost, "http://localhost/v1/responses", strings.NewReader(`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`))
	srv := &Server{}
	resp, _, _, err := srv.doResponsesUpstreamRequest(&model.Config{ResponsesTransport: "http"}, req, session)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, errResponsesWSReplayUnsafe) {
		t.Fatalf("err=%v", err)
	}
}
