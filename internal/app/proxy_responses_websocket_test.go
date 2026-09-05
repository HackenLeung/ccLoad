package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"
)

func TestIsResponsesWebsocketUpgradeRequest(t *testing.T) {
	cases := []struct {
		name string
		path string
		set  func(*http.Request)
		want bool
	}{
		{
			name: "codex upgrade",
			path: "/v1/responses",
			set: func(r *http.Request) {
				r.Header.Set("Connection", "Upgrade")
				r.Header.Set("Upgrade", "websocket")
				r.Header.Set("Sec-WebSocket-Version", "13")
				r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			},
			want: true,
		},
		{
			name: "ordinary get",
			path: "/v1/responses",
			set:  func(*http.Request) {},
			want: false,
		},
		{
			name: "wrong path",
			path: "/v1/models",
			set: func(r *http.Request) {
				r.Header.Set("Connection", "Upgrade")
				r.Header.Set("Upgrade", "websocket")
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			tc.set(r)
			if got := isResponsesWebsocketUpgradeRequest(r); got != tc.want {
				t.Fatalf("isResponsesWebsocketUpgradeRequest()=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestNormalizeInitialResponsesWebsocketRequest(t *testing.T) {
	session := newResponsesWebsocketSession()
	got, err := session.normalizeRequest([]byte(`{"type":"response.create","model":"gpt-5.6-codex","stream_id":"stream-1","input":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "" || string(got) == "null" {
		t.Fatalf("normalized request is empty: %s", got)
	}
	if gotModel := gjson.GetBytes(got, "model").String(); gotModel != "gpt-5.6-codex" {
		t.Fatalf("model=%q, want gpt-5.6-codex", gotModel)
	}
	if gotType := gjson.GetBytes(got, "type").String(); gotType != "" {
		t.Fatalf("type=%q, want removed", gotType)
	}
	if gotStreamID := gjson.GetBytes(got, "stream_id").String(); gotStreamID != "" {
		t.Fatalf("stream_id=%q, want removed from upstream HTTP body", gotStreamID)
	}
	if gotStream := gjson.GetBytes(got, "stream").Bool(); !gotStream {
		t.Fatal("stream=false, want true")
	}
	if gotStreamID := session.responseStreamID(); gotStreamID != "stream-1" {
		t.Fatalf("session stream_id=%q, want stream-1", gotStreamID)
	}
}

func TestResponsesWebsocketBridgeAddsStreamID(t *testing.T) {
	payload, err := addResponsesWebsocketStreamID([]byte(`{"type":"response.completed"}`), "stream-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(payload, "stream_id").String(); got != "stream-1" {
		t.Fatalf("stream_id=%q, want stream-1", got)
	}
}
