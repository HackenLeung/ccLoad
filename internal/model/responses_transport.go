package model

import "strings"

// Responses transports apply only to downstream Responses WebSocket requests.
const (
	ResponsesTransportHTTP            = "http"
	ResponsesTransportPreferWebsocket = "websocket_preferred"
	ResponsesTransportWebsocketOnly   = "websocket_only"
)

// NormalizeResponsesTransport defaults legacy channels to HTTP and rejects unknown modes.
func NormalizeResponsesTransport(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ResponsesTransportHTTP:
		return ResponsesTransportHTTP
	case ResponsesTransportPreferWebsocket:
		return ResponsesTransportPreferWebsocket
	case ResponsesTransportWebsocketOnly:
		return ResponsesTransportWebsocketOnly
	default:
		return ""
	}
}

// GetResponsesTransport returns the effective upstream transport for this channel.
func (c *Config) GetResponsesTransport() string {
	return NormalizeResponsesTransport(c.ResponsesTransport)
}
