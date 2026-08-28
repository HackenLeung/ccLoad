package app

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// HandleActiveRequests 返回当前进行中的请求列表（内存状态，不持久化）
func (s *Server) HandleActiveRequests(c *gin.Context) {
	var requests []*ActiveRequest
	if s.activeRequests != nil {
		requests = s.activeRequests.List()
	}
	RespondJSONWithCount(c, http.StatusOK, requests, len(requests))
}

// HandleGetActiveRequestDebugLog 返回运行中请求的调试日志快照。
// GET /admin/active-requests/:request_id/debug-log
func (s *Server) HandleGetActiveRequestDebugLog(c *gin.Context) {
	requestIDStr := c.Param("request_id")
	requestID, err := strconv.ParseInt(requestIDStr, 10, 64)
	if err != nil || requestID <= 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request_id")
		return
	}

	if s.activeRequests == nil {
		RespondErrorWithData(c, http.StatusNotFound, "debug log unavailable", s.buildDebugLogUnavailableInfo(c.Request.Context()))
		return
	}

	entry, ok := s.activeRequests.GetDebugLogSnapshot(requestID)
	if !ok || entry == nil {
		RespondErrorWithData(c, http.StatusNotFound, "debug log unavailable", s.buildDebugLogUnavailableInfo(c.Request.Context()))
		return
	}

	RespondJSON(c, http.StatusOK, debugLogResponse(entry))
}

// HandleSkipActiveRequest 取消当前上游尝试并让代理循环继续下一个渠道。
// POST /admin/active-requests/:request_id/skip
func (s *Server) HandleSkipActiveRequest(c *gin.Context) {
	requestID, err := strconv.ParseInt(c.Param("request_id"), 10, 64)
	if err != nil || requestID <= 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request_id")
		return
	}

	var req struct {
		AttemptID int64 `json:"attempt_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.AttemptID <= 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "valid attempt_id is required")
		return
	}

	if s.activeRequests == nil {
		RespondErrorMsg(c, http.StatusNotFound, "active request not found")
		return
	}

	err = s.activeRequests.RequestChannelSkip(requestID, req.AttemptID)
	if err != nil {
		switch {
		case errors.Is(err, errActiveRequestNotFound):
			RespondErrorMsg(c, http.StatusNotFound, err.Error())
		case errors.Is(err, errActiveRequestAttemptMismatch), errors.Is(err, errActiveRequestSkipNotAvailable):
			RespondErrorMsg(c, http.StatusConflict, err.Error())
		default:
			RespondErrorMsg(c, http.StatusInternalServerError, "failed to skip active request")
		}
		return
	}

	RespondJSON(c, http.StatusAccepted, gin.H{
		"request_id": requestID,
		"attempt_id": req.AttemptID,
		"status":     "switching_channel",
	})
}

// HandleCancelActiveRequest 取消整个进行中的请求（不再尝试其他渠道）。
// POST /admin/active-requests/:request_id/cancel
//
// 与 skip 的区别：skip 只取消当前上游尝试、代理循环会继续下一个候选渠道，且响应一旦
// 提交给客户端就不再可用；cancel 掐断请求级 context，流式响应提交后仍然有效——这正是
// “渠道挂死几千秒”场景唯一能自救的入口。
//
// 只需 request_id：请求 ID 由进程内单调递增分配、不复用，不存在 skip 那种“当前尝试会漂移”
// 的问题。为防止进程重启后 ID 重新从 1 开始、旧页面误杀新请求，客户端需带上 start_time 校验。
func (s *Server) HandleCancelActiveRequest(c *gin.Context) {
	requestID, err := strconv.ParseInt(c.Param("request_id"), 10, 64)
	if err != nil || requestID <= 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request_id")
		return
	}

	var req struct {
		StartTime int64 `json:"start_time"`
	}
	// body 可选：省略 start_time 时跳过重启防护校验
	_ = c.ShouldBindJSON(&req)

	if s.activeRequests == nil {
		RespondErrorMsg(c, http.StatusNotFound, "active request not found")
		return
	}

	if req.StartTime > 0 && !s.activeRequests.MatchesStartTime(requestID, req.StartTime) {
		RespondErrorMsg(c, http.StatusConflict, "active request changed, refresh and retry")
		return
	}

	if err := s.activeRequests.CancelRequest(requestID); err != nil {
		switch {
		case errors.Is(err, errActiveRequestNotFound):
			RespondErrorMsg(c, http.StatusNotFound, err.Error())
		case errors.Is(err, errActiveRequestAlreadyCanceled):
			// 幂等：已在取消中，重复点击不算失败
			RespondJSON(c, http.StatusAccepted, gin.H{
				"request_id": requestID,
				"status":     "canceling",
			})
		case errors.Is(err, errActiveRequestReadOnly):
			// 只读子请求（如视觉转文字辅助）：随宿主主请求生命周期起止，不能单独取消
			RespondErrorMsg(c, http.StatusConflict, err.Error())
		default:
			RespondErrorMsg(c, http.StatusInternalServerError, "failed to cancel active request")
		}
		return
	}

	log.Printf("[INFO] 管理端取消进行中请求 ID=%d（来源IP=%s）", requestID, c.ClientIP())

	RespondJSON(c, http.StatusAccepted, gin.H{
		"request_id": requestID,
		"status":     "canceling",
	})
}
