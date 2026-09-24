package app

import (
	"bytes"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

// Capability mismatch is a routing constraint, not an upstream failure. Never
// silently remove tool definitions/history or explicit reasoning preferences.
func filterProtocolCapabilityCandidates(candidates []*model.Config, client protocol.Protocol, body []byte) []*model.Config {
	filtered := make([]*model.Config, 0, len(candidates))
	for _, cfg := range candidates {
		if cfg == nil {
			continue
		}
		upstream := protocol.Protocol(cfg.ResolveUpstreamProtocol(string(client)))
		if cfg.GetProtocolTransformMode() != model.ProtocolTransformModeLocal || client == upstream {
			filtered = append(filtered, cfg)
			continue
		}
		if client == protocol.Codex && upstream == protocol.OpenAI {
			if !supportsResponsesContent(cfg, body) {
				continue
			}
			prepared := applyCodexToOpenAICapabilities(cfg, client, upstream, "/v1/responses", body)
			if !bytes.Equal(prepared, body) {
				continue
			}
		}
		if client == protocol.Codex && upstream != protocol.Codex {
			unsupported := false
			for _, key := range []string{"previous_response_id", "conversation", "context_management"} {
				value := gjson.GetBytes(body, key)
				if value.Exists() && value.Type != gjson.Null && value.String() != "" {
					unsupported = true
					break
				}
			}
			if unsupported {
				continue
			}
		}
		if client == protocol.OpenAI && upstream == protocol.Codex {
			if n := gjson.GetBytes(body, "n"); n.Exists() && n.Int() != 1 {
				continue
			}
		}
		filtered = append(filtered, cfg)
	}
	return filtered
}

func supportsResponsesContent(cfg *model.Config, body []byte) bool {
	if format := gjson.GetBytes(body, "text.format.type").String(); format != "" && format != "text" && !cfg.ProtocolCapabilityEnabled("codex", model.ProtocolCapabilityStructuredOutputs) {
		return false
	}
	supported := true
	var visit func(gjson.Result)
	visit = func(value gjson.Result) {
		if !supported {
			return
		}
		if value.IsObject() {
			switch value.Get("type").String() {
			case "input_image":
				supported = cfg.ProtocolCapabilityEnabled("codex", model.ProtocolCapabilityImages)
			case "input_file":
				supported = cfg.ProtocolCapabilityEnabled("codex", model.ProtocolCapabilityFiles)
			}
		}
		if value.IsArray() || value.IsObject() {
			value.ForEach(func(_, child gjson.Result) bool { visit(child); return supported })
		}
	}
	visit(gjson.GetBytes(body, "input"))
	return supported
}
