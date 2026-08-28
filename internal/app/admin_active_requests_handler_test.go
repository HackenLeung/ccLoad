package app

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestHandleActiveRequests(t *testing.T) {
	t.Parallel()

	m := newActiveRequestManager()
	id := m.Register(time.Now(), "m1", "1.2.3.4", true)
	m.Update(id, 10, "ch", "openai", "anthropic", "sk-test", 7, 1.5) //nolint:gosec // 测试用假凭证
	m.AddBytes(id, 123)
	m.SetClientFirstByteTime(id, 50*time.Millisecond)

	s := &Server{activeRequests: m}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/active_requests", nil))

	s.HandleActiveRequests(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Success bool            `json:"success"`
		Data    []ActiveRequest `json:"data"`
		Count   int             `json:"count"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if !resp.Success || resp.Count != 1 || len(resp.Data) != 1 {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	if resp.Data[0].BytesReceived != 123 {
		t.Fatalf("bytes_received=%d, want 123", resp.Data[0].BytesReceived)
	}
	if resp.Data[0].ClientFirstByteTime <= 0 {
		t.Fatalf("client_first_byte_time=%v, want >0", resp.Data[0].ClientFirstByteTime)
	}
	if resp.Data[0].CostMultiplier != 1.5 {
		t.Fatalf("cost_multiplier=%v, want 1.5", resp.Data[0].CostMultiplier)
	}
	if resp.Data[0].UpstreamProtocol != "anthropic" {
		t.Fatalf("upstream_protocol=%q, want anthropic", resp.Data[0].UpstreamProtocol)
	}
}

func TestActiveRequestManagerCount(t *testing.T) {
	t.Parallel()

	manager := newActiveRequestManager()
	if got := manager.Count(); got != 0 {
		t.Fatalf("Count on empty manager = %d, want 0", got)
	}

	first := manager.Register(time.Now(), "model-a", "127.0.0.1", true)
	second := manager.Register(time.Now(), "model-b", "127.0.0.1", false)
	if got := manager.Count(); got != 2 {
		t.Fatalf("Count after register = %d, want 2", got)
	}

	manager.Remove(first)
	if got := manager.Count(); got != 1 {
		t.Fatalf("Count after one remove = %d, want 1", got)
	}

	manager.Remove(second)
	if got := manager.Count(); got != 0 {
		t.Fatalf("Count after all removed = %d, want 0", got)
	}
}

func TestHandleActiveRequests_PreservesZeroCostMultiplier(t *testing.T) {
	t.Parallel()

	m := newActiveRequestManager()
	id := m.Register(time.Now(), "m1", "1.2.3.4", true)
	m.Update(id, 10, "free-channel", "openai", "openai", "sk-test", 7, 0) //nolint:gosec // 测试用假凭证

	s := &Server{activeRequests: m}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/active_requests", nil))

	s.HandleActiveRequests(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Success bool             `json:"success"`
		Data    []map[string]any `json:"data"`
		Count   int              `json:"count"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &resp)
	if !resp.Success || resp.Count != 1 || len(resp.Data) != 1 {
		t.Fatalf("unexpected resp: %+v", resp)
	}

	value, ok := resp.Data[0]["cost_multiplier"]
	if !ok {
		t.Fatalf("cost_multiplier missing in response: %+v", resp.Data[0])
	}
	if got, ok := value.(float64); !ok || got != 0 {
		t.Fatalf("cost_multiplier=%v, want 0", value)
	}
}

func TestHandleReadOnlySubRequestRejectsSkipAndCancel(t *testing.T) {
	m := newActiveRequestManager()
	srv := &Server{activeRequests: m}

	// 注册只读子请求（模拟视觉转文字辅助）
	subID := m.RegisterSub(time.Now(), "vision-vl", "1.2.3.4", model.LogSourceVisionAssist, false)
	subIDText := strconv.FormatInt(subID, 10)

	// 不允许跳过：只读子请求没有独立尝试
	skipReq := newRequest(http.MethodPost, "/admin/active-requests/"+subIDText+"/skip", bytes.NewBufferString(`{"attempt_id":1}`))
	skipReq.Header.Set("Content-Type", "application/json")
	skipC, skipW := newTestContext(t, skipReq)
	skipC.Params = gin.Params{{Key: "request_id", Value: subIDText}}
	srv.HandleSkipActiveRequest(skipC)
	if skipW.Code != http.StatusConflict {
		t.Fatalf("skip status=%d, want %d", skipW.Code, http.StatusConflict)
	}

	// 不允许单独取消
	cancelReq := newRequest(http.MethodPost, "/admin/active-requests/"+subIDText+"/cancel", bytes.NewBufferString(`{}`))
	cancelC, cancelW := newTestContext(t, cancelReq)
	cancelC.Params = gin.Params{{Key: "request_id", Value: subIDText}}
	srv.HandleCancelActiveRequest(cancelC)
	if cancelW.Code != http.StatusConflict {
		t.Fatalf("cancel status=%d, want %d", cancelW.Code, http.StatusConflict)
	}
}

func TestHandleSkipActiveRequest(t *testing.T) {
	newAttempt := func(t *testing.T, manager *activeRequestManager) (int64, int64, context.Context) {
		t.Helper()
		requestID := manager.Register(time.Now(), "m", "1.2.3.4", true)
		attemptCtx, cancelAttempt := context.WithCancelCause(context.Background())
		t.Cleanup(func() { cancelAttempt(nil) })
		attemptID, ok := manager.BeginAttempt(requestID, cancelAttempt)
		if !ok {
			t.Fatal("BeginAttempt() failed")
		}
		return requestID, attemptID, attemptCtx
	}

	call := func(t *testing.T, srv *Server, requestID string, body string) int {
		t.Helper()
		req := newRequest(http.MethodPost, "/admin/active-requests/"+requestID+"/skip", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		c, w := newTestContext(t, req)
		c.Params = gin.Params{{Key: "request_id", Value: requestID}}
		srv.HandleSkipActiveRequest(c)
		return w.Code
	}

	t.Run("accepted", func(t *testing.T) {
		manager := newActiveRequestManager()
		srv := &Server{activeRequests: manager}
		requestID, attemptID, attemptCtx := newAttempt(t, manager)

		if status := call(t, srv, strconv.FormatInt(requestID, 10), `{"attempt_id":`+strconv.FormatInt(attemptID, 10)+`}`); status != http.StatusAccepted {
			t.Fatalf("status=%d, want %d", status, http.StatusAccepted)
		}
		select {
		case <-attemptCtx.Done():
			if context.Cause(attemptCtx) != errManualChannelSkip {
				t.Fatalf("attempt cause = %v, want manual skip", context.Cause(attemptCtx))
			}
		case <-time.After(time.Second):
			t.Fatal("skip handler did not cancel attempt")
		}
	})

	t.Run("bad request", func(t *testing.T) {
		srv := &Server{activeRequests: newActiveRequestManager()}
		if status := call(t, srv, "not-a-number", `{"attempt_id":1}`); status != http.StatusBadRequest {
			t.Fatalf("invalid request ID status=%d, want %d", status, http.StatusBadRequest)
		}
		if status := call(t, srv, "1", `{}`); status != http.StatusBadRequest {
			t.Fatalf("missing attempt ID status=%d, want %d", status, http.StatusBadRequest)
		}
	})

	t.Run("not found", func(t *testing.T) {
		srv := &Server{activeRequests: newActiveRequestManager()}
		if status := call(t, srv, "99", `{"attempt_id":1}`); status != http.StatusNotFound {
			t.Fatalf("status=%d, want %d", status, http.StatusNotFound)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		manager := newActiveRequestManager()
		srv := &Server{activeRequests: manager}
		requestID, attemptID, _ := newAttempt(t, manager)
		requestIDText := strconv.FormatInt(requestID, 10)

		if status := call(t, srv, requestIDText, `{"attempt_id":`+strconv.FormatInt(attemptID+1, 10)+`}`); status != http.StatusConflict {
			t.Fatalf("stale attempt status=%d, want %d", status, http.StatusConflict)
		}
		if err := manager.PrepareResponseCommit(requestID, attemptID); err != nil {
			t.Fatalf("PrepareResponseCommit() error = %v", err)
		}
		if status := call(t, srv, requestIDText, `{"attempt_id":`+strconv.FormatInt(attemptID, 10)+`}`); status != http.StatusConflict {
			t.Fatalf("already committed status=%d, want %d", status, http.StatusConflict)
		}
	})
}
