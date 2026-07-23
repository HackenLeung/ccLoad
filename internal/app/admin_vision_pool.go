package app

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

type visionPoolItemRequest struct {
	ChannelID int64  `json:"channel_id"`
	Model     string `json:"model"`
	Priority  int    `json:"priority"`
}

type visionPoolUpdateRequest struct {
	Items []visionPoolItemRequest `json:"items"`
}

type visionPoolModelResponse struct {
	ChannelID           int64  `json:"channel_id"`
	ChannelName         string `json:"channel_name"`
	ChannelType         string `json:"channel_type"`
	ChannelEnabled      bool   `json:"channel_enabled"`
	Model               string `json:"model"`
	VisionAssistEnabled bool   `json:"vision_assist_enabled"`
	VisionPoolEnabled   bool   `json:"vision_pool_enabled"`
	VisionPriority      int    `json:"vision_priority"`
}

// HandleVisionPool lists all channel models and replaces visual-model pool membership.
func (s *Server) HandleVisionPool(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet:
		s.handleListVisionPool(c)
	case http.MethodPut:
		s.handleUpdateVisionPool(c)
	default:
		RespondErrorMsg(c, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleListVisionPool(c *gin.Context) {
	configs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"models": buildVisionPoolModelResponses(configs)})
}

func buildVisionPoolModelResponses(configs []*model.Config) []visionPoolModelResponse {
	items := make([]visionPoolModelResponse, 0)
	for _, cfg := range configs {
		if cfg == nil {
			continue
		}
		for _, entry := range cfg.ModelEntries {
			items = append(items, visionPoolModelResponse{
				ChannelID:           cfg.ID,
				ChannelName:         cfg.Name,
				ChannelType:         cfg.GetChannelType(),
				ChannelEnabled:      cfg.Enabled,
				Model:               entry.Model,
				VisionAssistEnabled: entry.VisionAssistEnabled,
				VisionPoolEnabled:   entry.VisionPoolEnabled,
				VisionPriority:      entry.VisionPriority,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].VisionPoolEnabled != items[j].VisionPoolEnabled {
			return items[i].VisionPoolEnabled
		}
		if items[i].VisionPriority != items[j].VisionPriority {
			return items[i].VisionPriority > items[j].VisionPriority
		}
		if items[i].ChannelID != items[j].ChannelID {
			return items[i].ChannelID < items[j].ChannelID
		}
		return strings.ToLower(items[i].Model) < strings.ToLower(items[j].Model)
	})
	return items
}

func (s *Server) handleUpdateVisionPool(c *gin.Context) {
	var req visionPoolUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	configs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	byID := make(map[int64]*model.Config, len(configs))
	for _, cfg := range configs {
		byID[cfg.ID] = cfg
	}

	seen := make(map[string]struct{}, len(req.Items))
	updates := make([]model.VisionPoolUpdate, 0, len(req.Items))
	for i := range req.Items {
		item := &req.Items[i]
		item.Model = strings.TrimSpace(item.Model)
		if item.ChannelID <= 0 || item.Model == "" || item.Priority < 0 {
			RespondErrorMsg(c, http.StatusBadRequest, "invalid vision pool item")
			return
		}
		cfg := byID[item.ChannelID]
		if cfg == nil {
			RespondErrorMsg(c, http.StatusBadRequest, "vision pool model does not exist")
			return
		}
		entry := cfg.FindModelEntry(item.Model, "")
		if entry == nil {
			RespondErrorMsg(c, http.StatusBadRequest, "vision pool model does not exist")
			return
		}
		key := fmt.Sprintf("%d\x00%s", item.ChannelID, strings.ToLower(item.Model))
		if _, exists := seen[key]; exists {
			RespondErrorMsg(c, http.StatusBadRequest, "duplicate vision pool item")
			return
		}
		seen[key] = struct{}{}
		updates = append(updates, model.VisionPoolUpdate{
			ChannelID: item.ChannelID,
			Model:     entry.Model,
			Priority:  item.Priority,
		})
	}

	if err := s.store.ReplaceVisionPool(c.Request.Context(), updates); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	s.InvalidateChannelListCache()
	s.handleListVisionPool(c)
}
