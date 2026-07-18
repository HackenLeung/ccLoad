package model

import "testing"

func TestProtocolCapabilityEnabledDefaultsToEnabled(t *testing.T) {
	t.Parallel()

	var nilConfig *Config
	if !nilConfig.ProtocolCapabilityEnabled("codex", ProtocolCapabilityHostedWebSearch) {
		t.Fatal("nil config should preserve legacy enabled behavior")
	}
	cfg := &Config{}
	if !cfg.ProtocolCapabilityEnabled("codex", ProtocolCapabilityHostedWebSearch) {
		t.Fatal("missing capability config should default to enabled")
	}
}

func TestProtocolCapabilityEnabledHonorsExplicitFalseAndCloneIsolation(t *testing.T) {
	t.Parallel()

	cfg := &Config{ProtocolCapabilities: map[string]map[string]bool{
		"codex": {ProtocolCapabilityHostedWebSearch: false},
	}}
	if cfg.ProtocolCapabilityEnabled("CODEX", "HOSTED_WEB_SEARCH") {
		t.Fatal("explicit false capability should be disabled")
	}
	if !cfg.ProtocolCapabilityEnabled("codex", ProtocolCapabilityFunctionTools) {
		t.Fatal("unconfigured capability should remain enabled")
	}
	clone := cfg.Clone()
	clone.ProtocolCapabilities["codex"][ProtocolCapabilityHostedWebSearch] = true
	if cfg.ProtocolCapabilityEnabled("codex", ProtocolCapabilityHostedWebSearch) {
		t.Fatal("clone must not share nested capability maps")
	}
}
