package app

import (
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
)

const channelRiskResponseSampleLimit = 64 * 1024

// ChannelRiskFinding is a reason shown by the channel risk observer.
type ChannelRiskFinding struct {
	Code   string `json:"code"`
	Score  int    `json:"score"`
	Detail string `json:"detail,omitempty"`
}

// ChannelRiskSummary is embedded in the channel list response.
type ChannelRiskSummary struct {
	Status         string     `json:"risk_status"`
	Score          *int       `json:"risk_score,omitempty"`
	SampleCount    int        `json:"risk_sample_count"`
	LastObservedAt *time.Time `json:"risk_last_observed_at,omitempty"`
}

// ChannelRiskReport is returned by the local-only "risk check" endpoint.
type ChannelRiskReport struct {
	ChannelRiskSummary
	ConfigFindings   []ChannelRiskFinding `json:"config_findings"`
	GlobalFindings   []ChannelRiskFinding `json:"global_findings"`
	ObservedFindings []ChannelRiskFinding `json:"observed_findings"`
	ActiveUpstreamIO bool                 `json:"active_upstream_io"`
	DebugLogRequired bool                 `json:"debug_log_required"`
}

type channelRiskState struct {
	sampleCount    int
	lastObservedAt time.Time
	findings       map[string]ChannelRiskFinding
}

type channelRiskObserver struct {
	mu        sync.RWMutex
	byChannel map[int64]*channelRiskState
}

type channelRiskResponseCapture struct {
	body []byte
}

func newChannelRiskObserver() *channelRiskObserver {
	return &channelRiskObserver{byChannel: make(map[int64]*channelRiskState)}
}

func (c *channelRiskResponseCapture) append(data []byte) {
	if c == nil || len(data) == 0 {
		return
	}
	if len(c.body) >= channelRiskResponseSampleLimit {
		return
	}
	remaining := channelRiskResponseSampleLimit - len(c.body)
	if len(data) > remaining {
		data = data[:remaining]
	}
	c.body = append(c.body, data...)
}

func (o *channelRiskObserver) record(channelID int64, result *fwResult, capture *channelRiskResponseCapture) {
	if o == nil || result == nil || result.Header == nil || channelID <= 0 {
		return
	}
	if capture != nil {
		defer func() { capture.body = nil }()
	}
	// HTTP errors, stream interruptions and SSE error events belong to channel
	// stability, not injection-risk evidence.
	if result.Status < 200 || result.Status >= 300 || result.SSEErrorEvent != nil || result.StreamDiagMsg != "" {
		return
	}

	findings := detectChannelResponseRisk(result.Status, result.Header, captureBody(capture))
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	state := o.byChannel[channelID]
	if state == nil {
		state = &channelRiskState{findings: make(map[string]ChannelRiskFinding)}
		o.byChannel[channelID] = state
	}
	state.sampleCount++
	state.lastObservedAt = now
	for _, finding := range findings {
		if _, exists := state.findings[finding.Code]; !exists {
			state.findings[finding.Code] = finding
		}
	}
}

func captureBody(capture *channelRiskResponseCapture) []byte {
	if capture == nil {
		return nil
	}
	return capture.body
}

func (o *channelRiskObserver) snapshot(channelID int64) (sampleCount int, lastObservedAt time.Time, findings []ChannelRiskFinding) {
	if o == nil {
		return 0, time.Time{}, nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	state := o.byChannel[channelID]
	if state == nil {
		return 0, time.Time{}, nil
	}
	findings = make([]ChannelRiskFinding, 0, len(state.findings))
	for _, finding := range state.findings {
		findings = append(findings, finding)
	}
	return state.sampleCount, state.lastObservedAt, findings
}

func assessChannelConfigRisk(cfg *model.Config) []ChannelRiskFinding {
	if cfg == nil {
		return nil
	}
	findings := make([]ChannelRiskFinding, 0, 5)
	addFinding := func(finding ChannelRiskFinding) {
		for _, existing := range findings {
			if existing.Code == finding.Code {
				return
			}
		}
		findings = append(findings, finding)
	}

	for _, rawURL := range cfg.GetURLs() {
		parsed, err := neturl.Parse(strings.TrimSpace(rawURL))
		if err != nil {
			addFinding(ChannelRiskFinding{Code: "invalid_endpoint", Score: 20, Detail: "invalid URL"})
			continue
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http":
			addFinding(ChannelRiskFinding{Code: "http_endpoint", Score: 25, Detail: "endpoint is not HTTPS"})
		case "https":
		default:
			addFinding(ChannelRiskFinding{Code: "invalid_endpoint", Score: 20, Detail: "endpoint is not HTTP(S)"})
		}
	}

	if strings.TrimSpace(cfg.ProxyURL) != "" {
		addFinding(ChannelRiskFinding{Code: "channel_proxy", Score: 5, Detail: "channel-specific proxy is configured"})
	}

	headerRules := cfg.HeaderRules()
	if len(headerRules) > 0 {
		addFinding(ChannelRiskFinding{
			Code:   "custom_header_rules",
			Score:  8,
			Detail: fmt.Sprintf("%d custom header rule(s)", len(headerRules)),
		})
	}
	for _, rule := range headerRules {
		switch strings.ToLower(strings.TrimSpace(rule.Name)) {
		case "host", "origin", "referer":
			addFinding(ChannelRiskFinding{Code: "routing_header_override", Score: 15, Detail: "Host/Origin/Referer is rewritten"})
		}
	}
	if bodyRules := cfg.BodyRules(); len(bodyRules) > 0 {
		addFinding(ChannelRiskFinding{
			Code:   "custom_body_rules",
			Score:  10,
			Detail: fmt.Sprintf("%d custom body rule(s)", len(bodyRules)),
		})
	}
	return findings
}

func assessGlobalChannelRisk(skipTLSVerify bool) []ChannelRiskFinding {
	if !skipTLSVerify {
		return nil
	}
	return []ChannelRiskFinding{{
		Code:   "tls_verify_disabled",
		Score:  35,
		Detail: "global TLS certificate verification is disabled",
	}}
}

func detectChannelResponseRisk(status int, headers http.Header, body []byte) []ChannelRiskFinding {
	if status < 200 || status >= 300 {
		return nil
	}
	findings := make([]ChannelRiskFinding, 0, 3)
	if looksLikeHTMLResponse(headers.Get("Content-Type"), string(body)) {
		findings = append(findings, ChannelRiskFinding{
			Code:   "unexpected_html_response",
			Score:  10,
			Detail: "successful response has HTML content type",
		})
	}

	text := strings.ToLower(string(body))
	if containsAny(text,
		"ignore previous instructions",
		"ignore all previous instructions",
		"忽略之前的指令",
		"忽略所有之前的指令",
	) {
		findings = append(findings, ChannelRiskFinding{
			Code:   "instruction_override",
			Score:  15,
			Detail: "response contains an instruction-override phrase",
		})
	}
	if containsCredentialRequest(text) {
		findings = append(findings, ChannelRiskFinding{
			Code:   "credential_exfiltration",
			Score:  35,
			Detail: "response asks for or forwards an API credential",
		})
	}
	return findings
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func containsCredentialRequest(text string) bool {
	// Bare "token", "secret" and "密钥" are common in normal API
	// documentation and model output. Only keep credential-specific terms so a
	// generic explanation does not become an exfiltration finding.
	targets := []string{
		"api key",
		"apikey",
		"access token",
		"bearer token",
		"authorization header",
		"api secret",
		"client secret",
		"api密钥",
		"访问令牌",
		"授权头",
	}
	verbs := []string{"send", "share", "paste", "provide", "reveal", "upload", "forward", "copy", "发送", "分享", "粘贴", "提供", "泄露", "上传"}
	for _, target := range targets {
		for _, verb := range verbs {
			if strings.Contains(text, verb+" your "+target) || strings.Contains(text, verb+" the "+target) || strings.Contains(text, verb+" "+target) || strings.Contains(text, verb+target) {
				return true
			}
		}
	}
	return false
}

func (s *Server) buildChannelRiskReport(cfg *model.Config) ChannelRiskReport {
	configFindings := assessChannelConfigRisk(cfg)
	globalFindings := assessGlobalChannelRisk(s != nil && s.skipTLSVerify)
	var sampleCount int
	var lastObservedAt time.Time
	var observedFindings []ChannelRiskFinding
	if cfg != nil && s != nil && s.channelRisk != nil {
		sampleCount, lastObservedAt, observedFindings = s.channelRisk.snapshot(cfg.ID)
	}

	score := 0
	for _, finding := range configFindings {
		score += finding.Score
	}
	for _, finding := range observedFindings {
		score += finding.Score
	}
	if score > 100 {
		score = 100
	}

	status := "unobserved"
	if score >= 60 {
		status = "high_risk"
	} else if len(observedFindings) > 0 {
		status = "attention"
	} else if sampleCount > 0 {
		status = "clear"
	} else if len(configFindings) > 0 {
		status = "configuration_attention"
	}

	var scorePtr *int
	if score > 0 || sampleCount > 0 {
		scorePtr = &score
	}
	var observedPtr *time.Time
	if !lastObservedAt.IsZero() {
		observed := lastObservedAt
		observedPtr = &observed
	}
	return ChannelRiskReport{
		ChannelRiskSummary: ChannelRiskSummary{
			Status:         status,
			Score:          scorePtr,
			SampleCount:    sampleCount,
			LastObservedAt: observedPtr,
		},
		ConfigFindings:   configFindings,
		GlobalFindings:   globalFindings,
		ObservedFindings: observedFindings,
		ActiveUpstreamIO: false,
		DebugLogRequired: false,
	}
}

func (s *Server) recordChannelRiskObservation(channelID int64, result *fwResult, capture *channelRiskResponseCapture) {
	if s == nil || s.channelRisk == nil {
		return
	}
	s.channelRisk.record(channelID, result, capture)
}
