package app

import (
	"bytes"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

func TestCapabilityRoutingPreservesRequestAndNativeFallback(t *testing.T) {
	local := &model.Config{ChannelType: "openai", ProtocolTransforms: []string{"codex"}, ProtocolTransformMode: model.ProtocolTransformModeLocal,
		ProtocolCapabilities: map[string]map[string]bool{"codex": {model.ProtocolCapabilityFunctionTools: false}}}
	native := &model.Config{ChannelType: "codex"}
	body := []byte(`{"input":[{"type":"function_call","call_id":"a","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"result"}]}`)
	before := bytes.Clone(body)
	got := filterProtocolCapabilityCandidates([]*model.Config{local, native}, protocol.Codex, body)
	if len(got) != 1 || got[0] != native {
		t.Fatalf("expected native fallback, got %#v", got)
	}
	if !bytes.Equal(body, before) {
		t.Fatal("routing changed tool history")
	}
	if got := filterProtocolCapabilityCandidates([]*model.Config{local}, protocol.Codex, []byte(`{"input":"hello"}`)); len(got) != 1 {
		t.Fatal("text request unnecessarily filtered")
	}
}

func TestResponsesStreamIDAndForkIsolation(t *testing.T) {
	for _, payload := range []string{`{"stream_id":""}`, `{"stream_id":null}`, `{"stream_id":12}`, `{"stream_id":"bad id"}`} {
		if _, err := validateResponsesStreamID([]byte(payload)); err == nil {
			t.Fatalf("accepted invalid stream: %s", payload)
		}
	}
	parent := newResponsesWebsocketSession()
	parent.lastRequest = []byte(`{"model":"test","input":[]}`)
	parent.lastResponseOutput = []byte(`[]`)
	parent.lastResponseID = "parent"
	parent.pendingToolCallIDs = []string{"call_a"}
	child := parent.fork()
	child.lastRequest[0] = ' '
	child.pendingToolCallIDs[0] = "child"
	if parent.lastRequest[0] != '{' || parent.pendingToolCallIDs[0] != "call_a" || child.upstream != nil {
		t.Fatal("fork shares mutable state")
	}
	parent.streamID = "named"
	if _, err := parent.normalizeRequest([]byte(`{"type":"response.create","model":"test","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if parent.streamID != "" {
		t.Fatal("omitted stream ID inherited named lane")
	}
}

func TestResponsesFailedEventRetainsClassifierError(t *testing.T) {
	parser := newSSEUsageParser("codex")
	if err := parser.Feed([]byte("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"server_error\",\"message\":\"broken\"}}}\n\n")); err != nil {
		t.Fatal(err)
	}
	if len(parser.GetLastError()) == 0 || !parser.IsStreamComplete() {
		t.Fatal("failed response lost error or terminal state")
	}
}

func TestResponsesContentCapabilityRouting(t *testing.T) {
	for key, body := range map[string]string{
		model.ProtocolCapabilityStructuredOutputs: `{"input":"hi","text":{"format":{"type":"json_schema"}}}`,
		model.ProtocolCapabilityImages:            `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/a.png"}]}]}`,
		model.ProtocolCapabilityFiles:             `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_a"}]}]}`,
	} {
		cfg := &model.Config{ChannelType: "openai", ProtocolTransforms: []string{"codex"}, ProtocolTransformMode: model.ProtocolTransformModeLocal, ProtocolCapabilities: map[string]map[string]bool{"codex": {key: false}}}
		if len(filterProtocolCapabilityCandidates([]*model.Config{cfg}, protocol.Codex, []byte(body))) != 0 {
			t.Fatalf("unsupported %s accepted", key)
		}
		if len(filterProtocolCapabilityCandidates([]*model.Config{cfg}, protocol.Codex, []byte(`{"input":"plain text"}`))) != 1 {
			t.Fatalf("plain text rejected by %s", key)
		}
	}
}

func TestChannelDeveloperRoleCompatibility(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"system"},{"role":"developer","content":"dev"},{"role":"user","content":"hi"}]}`)
	for _, enabled := range []bool{false, true} {
		cfg := &model.Config{ProtocolCapabilities: map[string]map[string]bool{"codex": {model.ProtocolCapabilityDeveloperRole: enabled}}}
		got := normalizeChannelDeveloperRole(cfg, "/v1/chat/completions", body)
		if bytes.Contains(got, []byte(`"role":"developer"`)) != enabled {
			t.Fatalf("enabled=%v body=%s", enabled, got)
		}
		if !bytes.Equal(normalizeChannelDeveloperRole(cfg, "/v1/responses", body), body) {
			t.Fatal("Responses was rewritten")
		}
	}
	if bytes.Contains(normalizeChannelDeveloperRole(&model.Config{}, "/v1/chat/completions", body), []byte(`"role":"developer"`)) {
		t.Fatal("legacy default changed")
	}
}
