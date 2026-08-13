package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// newCancelableRequest 注册一个绑定了请求级 cancel 的活跃请求。
func newCancelableRequest(t *testing.T, manager *activeRequestManager) (int64, context.Context) {
	t.Helper()
	requestID := manager.Register(time.Now(), "m", "1.2.3.4", true)
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	manager.BindRequestCancel(requestID, cancel)
	return requestID, ctx
}

func TestCancelRequestCancelsWholeRequest(t *testing.T) {
	t.Parallel()

	manager := newActiveRequestManager()
	requestID, ctx := newCancelableRequest(t, manager)

	if err := manager.CancelRequest(requestID); err != nil {
		t.Fatalf("CancelRequest() error = %v, want nil", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("请求 context 未被取消")
	}

	// 必须是专属 cause，否则会被误当成「跳过渠道」而继续重试下一个渠道
	if !isManualRequestCancel(ctx) {
		t.Fatalf("context cause = %v, want errManualRequestCancel", context.Cause(ctx))
	}
	if isManualChannelSkip(ctx) {
		t.Fatal("取消不应被识别为手动跳过渠道")
	}
	// 底层仍是 context.Canceled，保证分类为 499（不重试/不冷却）
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}

	list := manager.List()
	if len(list) != 1 || !list[0].CancelRequested {
		t.Fatalf("cancel_requested 未在列表中暴露: %+v", list)
	}
}

func TestCancelRequestIsIdempotent(t *testing.T) {
	t.Parallel()

	manager := newActiveRequestManager()
	requestID, _ := newCancelableRequest(t, manager)

	if err := manager.CancelRequest(requestID); err != nil {
		t.Fatalf("first CancelRequest() error = %v", err)
	}
	if err := manager.CancelRequest(requestID); !errors.Is(err, errActiveRequestAlreadyCanceled) {
		t.Fatalf("second CancelRequest() error = %v, want errActiveRequestAlreadyCanceled", err)
	}
}

func TestCancelRequestUnknownID(t *testing.T) {
	t.Parallel()

	manager := newActiveRequestManager()
	if err := manager.CancelRequest(999); !errors.Is(err, errActiveRequestNotFound) {
		t.Fatalf("CancelRequest(unknown) error = %v, want errActiveRequestNotFound", err)
	}

	// 已注册但未绑定 cancel（理论上不该出现）也应视为未找到，而不是 panic
	id := manager.Register(time.Now(), "m", "1.2.3.4", true)
	if err := manager.CancelRequest(id); !errors.Is(err, errActiveRequestNotFound) {
		t.Fatalf("CancelRequest(unbound) error = %v, want errActiveRequestNotFound", err)
	}
}

// 取消对已提交响应的请求同样有效：这正是 skip 覆盖不到的场景。
func TestCancelRequestWorksAfterResponseCommitted(t *testing.T) {
	t.Parallel()

	manager := newActiveRequestManager()
	requestID, ctx := newCancelableRequest(t, manager)

	attemptCtx, cancelAttempt := context.WithCancelCause(ctx)
	t.Cleanup(func() { cancelAttempt(nil) })
	attemptID, ok := manager.BeginAttempt(requestID, cancelAttempt)
	if !ok {
		t.Fatal("BeginAttempt() failed")
	}
	// 模拟响应已提交给客户端：skip 从此不可用
	if err := manager.PrepareResponseCommit(requestID, attemptID); err != nil {
		t.Fatalf("PrepareResponseCommit() error = %v", err)
	}
	if err := manager.RequestChannelSkip(requestID, attemptID); !errors.Is(err, errActiveRequestSkipNotAvailable) {
		t.Fatalf("RequestChannelSkip() error = %v, want errActiveRequestSkipNotAvailable", err)
	}

	// 取消仍然有效，且 cause 会传播到当前尝试的 context
	if err := manager.CancelRequest(requestID); err != nil {
		t.Fatalf("CancelRequest() error = %v", err)
	}
	select {
	case <-attemptCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("尝试 context 未随请求取消")
	}
	if !isManualRequestCancel(attemptCtx) {
		t.Fatalf("attempt cause = %v, want errManualRequestCancel", context.Cause(attemptCtx))
	}
}

func TestMatchesStartTime(t *testing.T) {
	t.Parallel()

	manager := newActiveRequestManager()
	// 用明确的过去时刻注册，避免与 Update 内部的 time.Now() 落在同一毫秒
	start := time.Now().Add(-time.Minute)
	id := manager.Register(start, "m", "1.2.3.4", true)

	if !manager.MatchesStartTime(id, start.UnixMilli()) {
		t.Fatal("MatchesStartTime() = false, want true")
	}
	if manager.MatchesStartTime(id, start.UnixMilli()+1) {
		t.Fatal("MatchesStartTime(mismatch) = true, want false")
	}
	if manager.MatchesStartTime(id+100, start.UnixMilli()) {
		t.Fatal("MatchesStartTime(unknown id) = true, want false")
	}

	// 切换渠道会重置 start_time，旧值不应再匹配（前端会带新值重试）
	manager.Update(id, 1, "ch", "openai", "openai", "sk-x", 0, 1) //nolint:gosec // 测试用假凭证
	if manager.MatchesStartTime(id, start.UnixMilli()) {
		t.Fatal("Update 后旧 start_time 仍匹配")
	}
}

func TestHandleCancelActiveRequest(t *testing.T) {
	call := func(t *testing.T, srv *Server, requestID, body string) int {
		t.Helper()
		req := newRequest(http.MethodPost, "/admin/active-requests/"+requestID+"/cancel", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		c, w := newTestContext(t, req)
		c.Params = gin.Params{{Key: "request_id", Value: requestID}}
		srv.HandleCancelActiveRequest(c)
		return w.Code
	}

	t.Run("invalid request id", func(t *testing.T) {
		srv := &Server{activeRequests: newActiveRequestManager()}
		if got := call(t, srv, "abc", "{}"); got != http.StatusBadRequest {
			t.Fatalf("status=%d, want %d", got, http.StatusBadRequest)
		}
	})

	t.Run("unknown request", func(t *testing.T) {
		srv := &Server{activeRequests: newActiveRequestManager()}
		if got := call(t, srv, "42", "{}"); got != http.StatusNotFound {
			t.Fatalf("status=%d, want %d", got, http.StatusNotFound)
		}
	})

	t.Run("cancel without start_time", func(t *testing.T) {
		manager := newActiveRequestManager()
		srv := &Server{activeRequests: manager}
		requestID, ctx := newCancelableRequest(t, manager)

		if got := call(t, srv, strconv.FormatInt(requestID, 10), "{}"); got != http.StatusAccepted {
			t.Fatalf("status=%d, want %d", got, http.StatusAccepted)
		}
		if ctx.Err() == nil {
			t.Fatal("请求 context 未被取消")
		}
	})

	t.Run("empty body is accepted", func(t *testing.T) {
		manager := newActiveRequestManager()
		srv := &Server{activeRequests: manager}
		requestID, ctx := newCancelableRequest(t, manager)

		if got := call(t, srv, strconv.FormatInt(requestID, 10), ""); got != http.StatusAccepted {
			t.Fatalf("status=%d, want %d", got, http.StatusAccepted)
		}
		if ctx.Err() == nil {
			t.Fatal("请求 context 未被取消")
		}
	})

	t.Run("start_time mismatch is rejected", func(t *testing.T) {
		manager := newActiveRequestManager()
		srv := &Server{activeRequests: manager}
		requestID, ctx := newCancelableRequest(t, manager)

		body := `{"start_time":1}`
		if got := call(t, srv, strconv.FormatInt(requestID, 10), body); got != http.StatusConflict {
			t.Fatalf("status=%d, want %d", got, http.StatusConflict)
		}
		if ctx.Err() != nil {
			t.Fatal("start_time 不匹配时不应取消请求")
		}
	})

	t.Run("repeat cancel stays accepted", func(t *testing.T) {
		manager := newActiveRequestManager()
		srv := &Server{activeRequests: manager}
		requestID, _ := newCancelableRequest(t, manager)

		idStr := strconv.FormatInt(requestID, 10)
		if got := call(t, srv, idStr, "{}"); got != http.StatusAccepted {
			t.Fatalf("first status=%d, want %d", got, http.StatusAccepted)
		}
		if got := call(t, srv, idStr, "{}"); got != http.StatusAccepted {
			t.Fatalf("second status=%d, want %d", got, http.StatusAccepted)
		}
	})

	t.Run("nil manager", func(t *testing.T) {
		srv := &Server{}
		if got := call(t, srv, "1", "{}"); got != http.StatusNotFound {
			t.Fatalf("status=%d, want %d", got, http.StatusNotFound)
		}
	})
}
