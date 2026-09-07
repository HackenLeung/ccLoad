package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func addResponsesFixtureChannel(t *testing.T, srv *Server, name, url, mode string, priority int) *model.Config {
	t.Helper()
	cfg, err := srv.store.CreateConfig(context.Background(), &model.Config{Name: name, URL: url, ChannelType: "codex", Enabled: true, Priority: priority, ResponsesTransport: mode, ModelEntries: []model.ModelEntry{{Model: "test"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "fixture-key", KeyStrategy: model.KeyStrategySequential}}); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func connectResponsesFixture(t *testing.T, srv *Server) *websocket.Conn {
	t.Helper()
	router := gin.New()
	router.GET("/v1/responses", srv.HandleProxyRequest)
	downstream := httptest.NewServer(router)
	t.Cleanup(downstream.Close)
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(downstream.URL, "http")+"/v1/responses", nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readResponsesFixtureEvent(t *testing.T, conn *websocket.Conn, want string) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		_, event, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		typ := gjson.GetBytes(event, "type").String()
		if typ == want {
			return event
		}
		if typ == "error" {
			t.Fatalf("unexpected error: %s", event)
		}
	}
}

func TestResponsesWSEndToEndFailoverAndWarmup(t *testing.T) {
	var unavailable atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { unavailable.Add(1); w.WriteHeader(426) }))
	defer first.Close()
	requests := make(chan []byte, 2)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture-response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}}\n\n")
	}))
	defer second.Close()
	srv := newInMemoryServer(t)
	srv.client = second.Client()
	firstCfg := addResponsesFixtureChannel(t, srv, "ws-only", first.URL, model.ResponsesTransportWebsocketOnly, 20)
	addResponsesFixtureChannel(t, srv, "http", second.URL, model.ResponsesTransportHTTP, 10)
	conn := connectResponsesFixture(t, srv)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test","generate":false,"input":[{"role":"user","content":"hi"}],"tools":[{"type":"function","name":"example","parameters":{"type":"object"}}]}`)); err != nil {
		t.Fatal(err)
	}
	warm := readResponsesFixtureEvent(t, conn, "response.completed")
	if unavailable.Load() != 0 || len(requests) != 0 {
		t.Fatal("warmup contacted upstream")
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.create","model":"test","previous_response_id":%q,"stream_id":"one","input":[]}`, gjson.GetBytes(warm, "response.id").String()))); err != nil {
		t.Fatal(err)
	}
	result := readResponsesFixtureEvent(t, conn, "response.completed")
	if gjson.GetBytes(result, "stream_id").String() != "one" {
		t.Fatalf("missing stream id: %s", result)
	}
	body := <-requests
	if len(gjson.GetBytes(body, "input").Array()) != 1 || len(gjson.GetBytes(body, "tools").Array()) != 1 {
		t.Fatalf("warmup state lost: %s", body)
	}
	if gjson.GetBytes(body, "generate").Exists() || gjson.GetBytes(body, "previous_response_id").Exists() {
		t.Fatalf("WS fields leaked: %s", body)
	}
	cfg, err := srv.store.GetConfig(context.Background(), firstCfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CooldownUntil != 0 {
		t.Fatal("WS unsupported cooled down HTTP channel")
	}
	if unavailable.Load() != 1 {
		t.Fatalf("unexpected attempts=%d", unavailable.Load())
	}
}

func TestResponsesWSEndToEndCancel(t *testing.T) {
	received := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		received <- struct{}{}
		_, _, _ = conn.ReadMessage()
		closed <- struct{}{}
	}))
	defer upstream.Close()
	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	addResponsesFixtureChannel(t, srv, "native", upstream.URL, model.ResponsesTransportWebsocketOnly, 10)
	conn := connectResponsesFixture(t, srv)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("request never arrived")
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop upstream")
	}
	readResponsesFixtureEvent(t, conn, "error")
}

func TestResponsesWSRoutingOrderChanges(t *testing.T) {
	session := newResponsesWebsocketSession()
	a := &model.Config{ID: 1, Priority: 10}
	b := &model.Config{ID: 2, Priority: 10}
	session.orderCandidates([]*model.Config{a, b})
	session.upstream = &responsesWSConnection{channel: 2}
	session.lastRequest = []byte(`{}`)
	channels := []*model.Config{a, b}
	session.orderCandidates(channels)
	if channels[0].ID != 2 {
		t.Fatal("existing session lost affinity")
	}
	a.Priority = 30
	channels = []*model.Config{a, b}
	session.orderCandidates(channels)
	if channels[0].ID != 1 {
		t.Fatal("manual priority change ignored")
	}
}

func TestResponsesWSMigrateAfterManualPriorityChange(t *testing.T) {
	firstRequests := make(chan []byte, 1)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, body, err := conn.ReadMessage()
		if err != nil {
			return
		}
		firstRequests <- body
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"native-first","status":"completed","output":[{"type":"function_call","call_id":"call_one","name":"example","arguments":"{}"}]}}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer first.Close()
	secondRequests := make(chan []byte, 1)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/exact-responses" || r.Header.Get("Authorization") != "Bearer fixture-key" {
			w.WriteHeader(400)
			return
		}
		body, _ := io.ReadAll(r.Body)
		secondRequests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"http-second\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer second.Close()
	srv := newInMemoryServer(t)
	srv.client = first.Client()
	addResponsesFixtureChannel(t, srv, "native", first.URL, model.ResponsesTransportWebsocketOnly, 20)
	secondCfg := addResponsesFixtureChannel(t, srv, "http", second.URL+"/exact-responses#", model.ResponsesTransportHTTP, 10)
	conn := connectResponsesFixture(t, srv)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test","input":[{"role":"user","content":"hi"}],"tools":[{"type":"function","name":"example","parameters":{"type":"object"}}]}`)); err != nil {
		t.Fatal(err)
	}
	readResponsesFixtureEvent(t, conn, "response.completed")
	<-firstRequests
	secondCfg.Priority = 30
	secondCfg.ModelEntries = []model.ModelEntry{{Model: "redirected", RedirectEnabled: true, ProtocolAliases: map[string][]string{"codex": {"test"}}}}
	if _, err := srv.store.UpdateConfig(context.Background(), secondCfg.ID, secondCfg); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateChannelListCache()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"native-first","input":[{"type":"function_call_output","call_id":"call_one","output":"ok"}]}`)); err != nil {
		t.Fatal(err)
	}
	readResponsesFixtureEvent(t, conn, "response.completed")
	body := <-secondRequests
	if gjson.GetBytes(body, "model").String() != "redirected" || gjson.GetBytes(body, "previous_response_id").Exists() || len(gjson.GetBytes(body, "input").Array()) != 3 || len(gjson.GetBytes(body, "tools").Array()) != 1 {
		t.Fatalf("migration lost conversation or routing: %s", body)
	}
}

func TestResponsesWSUncertainExecutionDoesNotFailOver(t *testing.T) {
	for _, mode := range []string{model.ResponsesTransportWebsocketOnly, model.ResponsesTransportHTTP} {
		t.Run(mode, func(t *testing.T) {
			var accepted, replayed atomic.Int32
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					conn, err := responsesWebsocketUpgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer func() { _ = conn.Close() }()
					if _, _, err := conn.ReadMessage(); err == nil {
						accepted.Add(1)
					}
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				accepted.Add(1)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				replayed.Add(1)
				w.WriteHeader(500)
			}))
			defer second.Close()
			srv := newInMemoryServer(t)
			srv.client = first.Client()
			addResponsesFixtureChannel(t, srv, "uncertain", first.URL, mode, 20)
			addResponsesFixtureChannel(t, srv, "fallback", second.URL, model.ResponsesTransportHTTP, 10)
			conn := connectResponsesFixture(t, srv)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
				t.Fatal(err)
			}
			readResponsesFixtureEvent(t, conn, "error")
			if accepted.Load() != 1 || replayed.Load() != 0 {
				t.Fatalf("accepted=%d replayed=%d", accepted.Load(), replayed.Load())
			}
			if _, _, err := conn.ReadMessage(); err == nil {
				t.Fatal("uncertain session remained usable")
			}
		})
	}
}
