package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"ccLoad/internal/model"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

var (
	errResponsesWSOutcomeUnknown = errors.New("upstream WebSocket request may have executed; automatic replay stopped")
	errResponsesWSReplayUnsafe   = errors.New("conversation contains upstream-bound state; replay on another connection is unsafe")
)

type responsesWSCapabilityKey struct {
	channel  int64
	url      string
	revision int64
}

func validateResponsesTransport(cfg *model.Config) error {
	if cfg.ResponsesTransport == "" || model.NormalizeResponsesTransport(cfg.ResponsesTransport) == "" {
		return errors.New("invalid responses_transport (allowed: http, websocket_preferred, websocket_only)")
	}
	if cfg.GetResponsesTransport() != model.ResponsesTransportHTTP && (!cfg.SupportsProtocol("codex") || cfg.ResolveUpstreamProtocol("codex") != "codex") {
		return errors.New("native Responses WebSocket requires an upstream Responses protocol")
	}
	return nil
}

// Connections belong to one authenticated downstream session, never a global pool.
type responsesWSConnection struct {
	conn           *websocket.Conn
	frames         chan responsesWSFrame
	done           chan struct{}
	channel        int64
	url            string
	identity       [32]byte
	keyHash        [32]byte
	closed         atomic.Bool
	lastRequest    []byte
	lastOutput     []byte
	lastResponseID string
	lastUsed       time.Time
}

type responsesWSFrame struct {
	messageType int
	payload     []byte
	err         error
}

func (c *responsesWSConnection) readFrames() {
	defer close(c.frames)
	defer c.close()
	for {
		messageType, payload, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		select {
		case c.frames <- responsesWSFrame{messageType, payload, err}:
		case <-c.done:
			return
		}
	}
}

func (c *responsesWSConnection) close() {
	if c != nil && !c.closed.Swap(true) {
		if c.done != nil {
			close(c.done)
		}
		if c.conn != nil {
			_ = c.conn.Close()
		}
	}
}

func (s *responsesWebsocketSession) closeUpstream() { s.upstream.close() }

func (s *responsesWebsocketSession) orderCandidates(candidates []*model.Config) {
	var signature strings.Builder
	ordered := append([]*model.Config(nil), candidates...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, cfg := range ordered {
		fmt.Fprintf(&signature, "%d:%d:%s:%d;", cfg.ID, cfg.Priority, cfg.GetResponsesTransport(), cfg.UpdatedAt.UnixNano())
	}
	current := signature.String()
	if s.routingOrder == current && s.upstream != nil && !s.upstream.closed.Load() && len(s.lastRequest) > 0 {
		for i, cfg := range candidates {
			if cfg.ID == s.upstream.channel {
				copy(candidates[1:i+1], candidates[:i])
				candidates[0] = cfg
				break
			}
		}
	}
	s.routingOrder = current
}

func responsesWSIdentity(cfg *model.Config, req *http.Request) [32]byte {
	var key strings.Builder
	fmt.Fprintf(&key, "%d\n%s\n%s\n%d\n", cfg.ID, req.URL.String(), cfg.ProxyURL, cfg.UpdatedAt.UnixNano())
	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "Content-Type") {
			continue
		}
		fmt.Fprintf(&key, "%s:%s\n", name, strings.Join(req.Header.Values(name), "\x00"))
	}
	return sha256.Sum256([]byte(key.String()))
}

func (s *Server) doResponsesUpstreamRequest(cfg *model.Config, req *http.Request, session *responsesWebsocketSession) (*http.Response, *responsesWSBody, string, error) {
	mode := cfg.GetResponsesTransport()
	key := responsesWSCapabilityKey{cfg.ID, req.URL.String(), cfg.UpdatedAt.UnixNano()}
	info := "client=ws upstream=http"
	useHTTP := mode == model.ResponsesTransportHTTP
	if mode == model.ResponsesTransportPreferWebsocket {
		if cached, ok := s.responsesWSUnsupported.Load(key); ok {
			if time.Now().Before(cached.(time.Time)) {
				useHTTP = true
				info += " fallback=ws_unsupported_cached"
			} else {
				s.responsesWSUnsupported.Delete(key)
			}
		}
	}
	if useHTTP {
		if !session.canReplayRequest(req) {
			return nil, nil, info, errResponsesWSReplayUnsafe
		}
		session.closeUpstream()
		resp, err := s.doUpstreamRequest(cfg, req)
		return resp, nil, info, err
	}
	resp, turn, info, err := s.openResponsesWSTurn(cfg, req, session)
	// Only a rejected HTTP upgrade proves no response.create was sent. Auth and
	// rate-limit responses retain the existing Key/channel classification.
	if mode == model.ResponsesTransportPreferWebsocket && resp != nil && turn == nil && responsesWSUnsupportedResponse(resp) {
		_ = resp.Body.Close()
		s.responsesWSUnsupported.Range(func(k, v any) bool {
			if time.Now().After(v.(time.Time)) {
				s.responsesWSUnsupported.Delete(k)
			}
			return true
		})
		count := 0
		s.responsesWSUnsupported.Range(func(k, _ any) bool {
			count++
			if count >= 256 {
				s.responsesWSUnsupported.Delete(k)
			}
			return true
		})
		s.responsesWSUnsupported.Store(key, time.Now().Add(10*time.Minute))
		if !session.canReplayRequest(req) {
			return nil, nil, info, errResponsesWSReplayUnsafe
		}
		resp, err = s.doUpstreamRequest(cfg, req)
		return resp, nil, "client=ws upstream=http fallback=ws_unsupported", err
	}
	return resp, turn, info, err
}

func responsesWSUnsupportedStatus(status int) bool {
	return status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusUpgradeRequired || status == http.StatusNotImplemented
}

func responsesWSUnsupportedResponse(resp *http.Response) bool {
	if !responsesWSUnsupportedStatus(resp.StatusCode) {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	prependToBody(resp, body)
	if err != nil {
		return false
	}
	code := gjson.GetBytes(body, "error.code").String()
	return code != "model_not_found" && code != "invalid_api_key" && code != "permission_denied"
}

func (s *responsesWebsocketSession) canReplayRequest(req *http.Request) bool {
	if s.upstream == nil {
		return true
	}
	reader, err := req.GetBody()
	if err != nil {
		return false
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	return err == nil && responsesTranscriptPortable(body)
}

func responsesTranscriptPortable(payload []byte) bool {
	root := gjson.ParseBytes(payload)
	portable := true
	var visit func(gjson.Result)
	visit = func(value gjson.Result) {
		if value.IsObject() {
			if value.Get("encrypted_content").String() != "" || value.Get("type").String() == "item_reference" {
				portable = false
			}
			if value.Get("type").String() == "reasoning" && !value.Get("content").Exists() {
				portable = false
			}
		}
		if value.IsObject() || value.IsArray() {
			value.ForEach(func(_, child gjson.Result) bool { visit(child); return portable })
		}
	}
	visit(root)
	return portable
}

func (s *Server) openResponsesWSTurn(cfg *model.Config, req *http.Request, session *responsesWebsocketSession) (*http.Response, *responsesWSBody, string, error) {
	info := "client=ws upstream=ws"
	reader, err := req.GetBody()
	if err != nil {
		return nil, nil, info, err
	}
	requestBody, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return nil, nil, info, err
	}
	identity := responsesWSIdentity(cfg, req)
	upstream := session.upstream
	reused := upstream != nil && upstream.identity == identity && !upstream.closed.Load() && time.Since(upstream.lastUsed) < 4*time.Minute
	if !reused && upstream != nil && !responsesTranscriptPortable(requestBody) {
		return nil, nil, info, errResponsesWSReplayUnsafe
	}
	release, err := s.reserveUpstreamRequest(cfg)
	if err != nil {
		return nil, nil, info, err
	}
	if !reused {
		session.closeUpstream()
		dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second, ReadBufferSize: 4096, WriteBufferSize: 4096}
		transport, ok := s.getClientForChannel(cfg).Transport.(*http.Transport)
		if !ok {
			release()
			return nil, nil, info, errors.New("WebSocket requires an HTTP transport with channel proxy settings")
		}
		dialer.Proxy = transport.Proxy
		dialer.NetDialContext = transport.DialContext
		if transport.TLSClientConfig != nil {
			dialer.TLSClientConfig = transport.TLSClientConfig.Clone()
		}
		upstreamURL := *req.URL
		if upstreamURL.Scheme == "https" {
			upstreamURL.Scheme = "wss"
		} else {
			upstreamURL.Scheme = "ws"
		}
		headers := responsesWebsocketUpstreamHeaders(req.Header)
		headers.Del("Content-Length")
		headers.Del("Content-Type")
		conn, handshake, dialErr := dialer.DialContext(req.Context(), upstreamURL.String(), headers)
		if dialErr != nil {
			release()
			if handshake != nil {
				if handshake.StatusCode >= 200 && handshake.StatusCode < 300 {
					handshake.StatusCode = http.StatusBadGateway
				}
				return handshake, nil, info, nil
			}
			return nil, nil, info, dialErr
		}
		if handshake != nil && handshake.Body != nil {
			_ = handshake.Body.Close()
		}
		conn.SetReadLimit(maxProxyBodyBytes("/v1/responses"))
		upstream = &responsesWSConnection{conn: conn, channel: cfg.ID, url: req.URL.String(), identity: identity,
			keyHash: sha256.Sum256([]byte(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))), frames: make(chan responsesWSFrame), done: make(chan struct{})}
		go upstream.readFrames()
		session.upstream = upstream
	}
	payload, continued, err := buildResponsesWSCreate(requestBody, upstream)
	if err != nil {
		release()
		upstream.close()
		return nil, nil, info, err
	}
	if !continued && upstream.lastResponseID != "" && !responsesTranscriptPortable(requestBody) {
		release()
		return nil, nil, info, errResponsesWSReplayUnsafe
	}
	stop := context.AfterFunc(req.Context(), upstream.close)
	if err = upstream.conn.SetWriteDeadline(time.Now().Add(responsesWebsocketWriteTimeout)); err == nil {
		err = upstream.conn.WriteMessage(websocket.TextMessage, payload)
	}
	if err != nil {
		stop()
		upstream.close()
		release()
		return nil, nil, info, fmt.Errorf("%w: %v", errResponsesWSOutcomeUnknown, err)
	}
	body := &responsesWSBody{upstream: upstream, request: requestBody, sentAt: time.Now(), stop: stop, release: release,
		output: newResponsesWebsocketBridgeWriter(nil, "")}
	info += fmt.Sprintf(" reused=%t continuation=%t", reused, continued)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: req}
	return resp, body, info, nil
}

func buildResponsesWSCreate(body []byte, upstream *responsesWSConnection) ([]byte, bool, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, false, err
	}
	delete(request, "stream")
	delete(request, "background")
	delete(request, "previous_response_id")
	delete(request, "stream_id")
	request["type"] = json.RawMessage(`"response.create"`)
	continued := false
	if upstream.lastResponseID != "" && gjson.GetBytes(body, "model").String() == gjson.GetBytes(upstream.lastRequest, "model").String() {
		var input, oldInput, output []json.RawMessage
		if json.Unmarshal(request["input"], &input) == nil && json.Unmarshal([]byte(gjson.GetBytes(upstream.lastRequest, "input").Raw), &oldInput) == nil && json.Unmarshal(upstream.lastOutput, &output) == nil {
			prefix := append(oldInput, output...)
			if len(input) >= len(prefix) {
				continued = true
				for i := range prefix {
					var a, b bytes.Buffer
					if json.Compact(&a, prefix[i]) != nil || json.Compact(&b, input[i]) != nil || !bytes.Equal(a.Bytes(), b.Bytes()) {
						continued = false
						break
					}
				}
				if continued {
					request["input"], _ = json.Marshal(input[len(prefix):])
					request["previous_response_id"], _ = json.Marshal(upstream.lastResponseID)
				}
			}
		}
	}
	payload, err := json.Marshal(request)
	return payload, continued, err
}

// Adapts upstream WS frames to the existing SSE parser, preserving accounting,
// error classification, response commit checks and the downstream event bridge.
type responsesWSBody struct {
	upstream         *responsesWSConnection
	request          []byte
	sentAt           time.Time
	pending          bytes.Reader
	terminal         atomic.Bool
	closed           atomic.Bool
	unsafeTerminal   bool
	observedResponse bool
	output           *responsesWebsocketBridgeWriter
	stop             func() bool
	release          func()
}

func (b *responsesWSBody) Read(p []byte) (int, error) {
	if b.pending.Len() > 0 {
		return b.pending.Read(p)
	}
	if b.terminal.Load() {
		return 0, io.EOF
	}
	wsFrame, ok := <-b.upstream.frames
	if !ok {
		return 0, errResponsesWSOutcomeUnknown
	}
	messageType, payload, err := wsFrame.messageType, wsFrame.payload, wsFrame.err
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errResponsesWSOutcomeUnknown, err)
	}
	if messageType != websocket.TextMessage || !gjson.ValidBytes(payload) {
		return 0, fmt.Errorf("%w: invalid upstream event", errResponsesWSOutcomeUnknown)
	}
	eventType := gjson.GetBytes(payload, "type").String()
	b.output.collectOutputItem(eventType, payload)
	switch eventType {
	case "response.completed", "response.done":
		if status := gjson.GetBytes(payload, "response.status").String(); status != "" && status != "completed" {
			b.unsafeTerminal = true
			b.terminal.Store(true)
			b.upstream.close()
			return 0, fmt.Errorf("%w: terminal response status %s", errResponsesWSOutcomeUnknown, status)
		}
		b.upstream.lastResponseID = gjson.GetBytes(payload, "response.id").String()
		b.upstream.lastRequest = bytes.Clone(b.request)
		b.upstream.lastOutput = []byte(gjson.GetBytes(payload, "response.output").Raw)
		if !gjson.GetBytes(payload, "response.output").IsArray() || len(gjson.GetBytes(payload, "response.output").Array()) == 0 {
			b.upstream.lastOutput = b.output.collectedOutput()
		}
		b.upstream.lastUsed = time.Now()
		b.terminal.Store(true)
	case "error", "response.failed", "response.incomplete":
		b.unsafeTerminal = b.observedResponse || eventType != "error"
		b.terminal.Store(true)
		b.upstream.close()
	}
	if strings.HasPrefix(eventType, "response.") {
		b.observedResponse = true
	}
	sseFrame := append([]byte("data: "), payload...)
	sseFrame = append(sseFrame, '\n', '\n')
	b.pending.Reset(sseFrame)
	return b.pending.Read(p)
}

func (b *responsesWSBody) Close() error {
	if !b.closed.Swap(true) {
		b.stop()
		if !b.terminal.Load() {
			b.upstream.close()
		}
		b.release()
	}
	return nil
}
