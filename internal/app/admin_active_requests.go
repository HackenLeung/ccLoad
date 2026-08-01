package app

import (
	"errors"
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
