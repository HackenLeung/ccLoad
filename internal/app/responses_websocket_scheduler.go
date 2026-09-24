package app

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const responsesWebsocketContextBudget = 64 << 20

type responsesLane struct {
	session *responsesWebsocketSession
	cancel  context.CancelFunc
	touched time.Time
}
type responsesTurnCompletion struct {
	id     string
	body   []byte
	result responsesWebsocketTurnResult
	err    error
}
type responsesHistoryEntry struct {
	session *responsesWebsocketSession
	touched time.Time
}

// The scheduler alone owns lane and history maps. Workers own active sessions;
// forks are taken only from immutable snapshots after a worker has returned.
func (s *Server) runResponsesWebsocketScheduler(ctx context.Context, cancel context.CancelFunc, c *gin.Context, writer *responsesWebsocketWriter, messages <-chan responsesWebsocketInboundMessage) {
	lanes := make(map[string]*responsesLane)
	history := make(map[string]responsesHistoryEntry)
	done := make(chan responsesTurnCompletion, 16)
	var queue []responsesWebsocketInboundMessage
	active := 0
	reserved := 0
	defer func() {
		cancel()
		_ = writer.conn.Close()
		for active > 0 {
			<-done
			active--
		}
		for _, lane := range lanes {
			if lane.cancel != nil {
				lane.cancel()
			}
			lane.session.closeUpstream()
		}
	}()
	report := func(id, code, message string) {
		event := gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "code": code, "message": message}}
		if id != "" {
			event["stream_id"] = id
		}
		if writer.WriteJSON(event) != nil {
			cancel()
		}
	}
	retainedBytes := func() int {
		total := 0
		for _, lane := range lanes {
			total += len(lane.session.lastRequest) + len(lane.session.lastResponseOutput)
		}
		for _, entry := range history {
			total += len(entry.session.lastRequest) + len(entry.session.lastResponseOutput)
		}
		return total
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	prune := func() {
		now := time.Now()
		for id, entry := range history {
			if now.Sub(entry.touched) > responsesWebsocketIdleTimeout {
				delete(history, id)
			}
		}
		for id, lane := range lanes {
			if lane.cancel == nil && now.Sub(lane.touched) > responsesWebsocketIdleTimeout {
				lane.session.closeUpstream()
				delete(lanes, id)
			}
		}
		for len(history) > 32 || (len(history) > 0 && retainedBytes() > responsesWebsocketContextBudget) {
			oldest := ""
			var stamp time.Time
			for id, entry := range history {
				if stamp.IsZero() || entry.touched.Before(stamp) {
					oldest, stamp = id, entry.touched
				}
			}
			delete(history, oldest)
		}
		// Evict only idle lanes; running workers exclusively own their sessions.
		for retainedBytes()+reserved > responsesWebsocketContextBudget {
			oldest := ""
			var stamp time.Time
			for id, lane := range lanes {
				if lane.cancel == nil && (stamp.IsZero() || lane.touched.Before(stamp)) {
					oldest, stamp = id, lane.touched
				}
			}
			if stamp.IsZero() {
				break
			}
			lanes[oldest].session.closeUpstream()
			delete(lanes, oldest)
		}
	}
	for {
		// Scan past busy lanes, keeping each lane's requests in FIFO order.
		for i := 0; i < len(queue) && active < 16 && ctx.Err() == nil; {
			message := queue[i]
			id, _ := validateResponsesStreamID(message.payload)
			lane := lanes[id]
			if lane != nil && lane.cancel != nil {
				i++
				continue
			}
			copy(queue[i:], queue[i+1:])
			queue[len(queue)-1] = responsesWebsocketInboundMessage{}
			queue = queue[:len(queue)-1]
			if lane == nil {
				named := len(lanes)
				if lanes[""] != nil {
					named--
				}
				if id != "" && named >= 32 {
					report(id, "stream_limit", "too many named streams on this connection")
					continue
				}
				lane = &responsesLane{session: newResponsesWebsocketSession()}
				lanes[id] = lane
			}
			lane.touched = time.Now()
			previous := gjson.GetBytes(message.payload, "previous_response_id").String()
			if previous == "" && gjson.GetBytes(message.payload, "type").String() == responsesWebsocketRequestCreate && lane.session.outcomeUnknown.Load() {
				lane.session.closeUpstream()
				lane.session = newResponsesWebsocketSession()
			}
			if previous != "" && previous != lane.session.lastResponseID {
				if parent, ok := history[previous]; ok {
					lane.session.closeUpstream()
					lane.session = parent.session.fork()
				}
			}
			body, err := lane.session.normalizeRequest(message.payload)
			if err != nil {
				report(id, "invalid_request", err.Error())
				continue
			}
			// Reserve transcript bytes before starting another worker.
			if retainedBytes()+reserved+len(body) > responsesWebsocketContextBudget {
				report(id, "context_limit", "connection context budget exceeded; compact and replay on a new connection")
				continue
			}
			turnCtx, stop := context.WithCancel(ctx)
			lane.cancel = stop
			active++
			reserved += len(body)
			turnContext := c.Copy()
			go func(id string, body []byte, session *responsesWebsocketSession) {
				defer stop()
				result, err := s.executeResponsesWebsocketTurn(turnCtx, turnContext, writer, body, session)
				done <- responsesTurnCompletion{id, body, result, err}
			}(id, body, lane.session)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		case completed := <-done:
			active--
			reserved -= len(completed.body)
			lane := lanes[completed.id]
			lane.cancel()
			lane.cancel = nil
			lane.touched = time.Now()
			if completed.err != nil {
				report(completed.id, "upstream_error", completed.err.Error())
			} else if completed.result.completedResponseID != "" {
				lane.session.commit(completed.body, completed.result)
				history[lane.session.lastResponseID] = responsesHistoryEntry{lane.session.fork(), time.Now()}
			}
			prune()
		case message := <-messages:
			if message.messageType != websocket.TextMessage {
				report("", "unsupported_frame", "only text websocket messages are supported")
				continue
			}
			if !gjson.ValidBytes(message.payload) {
				report("", "invalid_request", "invalid websocket request JSON")
				continue
			}
			id, err := validateResponsesStreamID(message.payload)
			if err != nil {
				report("", "invalid_request", err.Error())
				continue
			}
			switch gjson.GetBytes(message.payload, "type").String() {
			case "response.cancel":
				if lane := lanes[id]; lane != nil && lane.cancel != nil {
					lane.cancel()
				} else {
					report(id, "no_active_response", "no response is currently running on this stream")
				}
			case responsesWebsocketRequestCreate, responsesWebsocketRequestAppend:
				bytes := len(message.payload)
				for _, pending := range queue {
					bytes += len(pending.payload)
				}
				if len(queue) >= 32 || bytes+retainedBytes()+reserved > responsesWebsocketContextBudget {
					report(id, "queue_limit", "too many queued websocket requests")
					continue
				}
				queue = append(queue, message)
			default:
				report(id, "unsupported_event", "unsupported websocket request type")
			}
		}
	}
}
