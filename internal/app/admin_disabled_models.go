package app

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

type globalDisabledModelStore interface {
	ListGlobalDisabledModels(ctx context.Context) ([]model.GlobalDisabledModel, error)
	UpsertGlobalDisabledModel(ctx context.Context, entry model.GlobalDisabledModel) error
	DeleteGlobalDisabledModel(ctx context.Context, modelName string) error
}

type globalDisabledModelRequest struct {
	Model string `json:"model"`
	Note  string `json:"note"`
}

func normalizeGlobalDisabledModelName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("模型名称不能为空")
	}
	if value == "*" {
		return "", fmt.Errorf("不能禁用通配模型 *")
	}
	if len(value) > 191 {
		return "", fmt.Errorf("模型名称不能超过191个字符")
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("模型名称包含非法字符")
	}
	return value, nil
}

func globalDisabledModelKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func (s *Server) disabledModelStore() (globalDisabledModelStore, error) {
	store, ok := s.store.(globalDisabledModelStore)
	if !ok {
		return nil, fmt.Errorf("global disabled model storage is unavailable")
	}
	return store, nil
}

func (s *Server) reloadGlobalDisabledModels(ctx context.Context) error {
	store, err := s.disabledModelStore()
	if err != nil {
		return err
	}
	entries, err := store.ListGlobalDisabledModels(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]model.GlobalDisabledModel, len(entries))
	for _, entry := range entries {
		next[globalDisabledModelKey(entry.Model)] = entry
	}
	s.globalDisabledModelsMu.Lock()
	s.globalDisabledModels = next
	s.globalDisabledModelsMu.Unlock()
	return nil
}

func (s *Server) isGlobalModelDisabled(modelName string) bool {
	key := globalDisabledModelKey(modelName)
	if key == "" {
		return false
	}
	s.globalDisabledModelsMu.RLock()
	_, disabled := s.globalDisabledModels[key]
	s.globalDisabledModelsMu.RUnlock()
	return disabled
}

func (s *Server) cacheGlobalDisabledModel(entry model.GlobalDisabledModel) {
	s.globalDisabledModelsMu.Lock()
	if s.globalDisabledModels == nil {
		s.globalDisabledModels = make(map[string]model.GlobalDisabledModel)
	}
	s.globalDisabledModels[globalDisabledModelKey(entry.Model)] = entry
	s.globalDisabledModelsMu.Unlock()
}

func (s *Server) removeCachedGlobalDisabledModel(modelName string) {
	s.globalDisabledModelsMu.Lock()
	delete(s.globalDisabledModels, globalDisabledModelKey(modelName))
	s.globalDisabledModelsMu.Unlock()
}

func (s *Server) listCachedGlobalDisabledModels() []model.GlobalDisabledModel {
	s.globalDisabledModelsMu.RLock()
	entries := make([]model.GlobalDisabledModel, 0, len(s.globalDisabledModels))
	for _, entry := range s.globalDisabledModels {
		entries = append(entries, entry)
	}
	s.globalDisabledModelsMu.RUnlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt != entries[j].CreatedAt {
			return entries[i].CreatedAt < entries[j].CreatedAt
		}
		return entries[i].Model < entries[j].Model
	})
	return entries
}

func (s *Server) HandleGlobalDisabledModels(c *gin.Context) {
	store, err := s.disabledModelStore()
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	switch c.Request.Method {
	case http.MethodGet:
		RespondJSON(c, http.StatusOK, s.listCachedGlobalDisabledModels())
	case http.MethodPost:
		var req globalDisabledModelRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			RespondError(c, http.StatusBadRequest, err)
			return
		}
		modelName, err := normalizeGlobalDisabledModelName(req.Model)
		if err != nil {
			RespondError(c, http.StatusBadRequest, err)
			return
		}
		note := strings.TrimSpace(req.Note)
		if len(note) > 512 || strings.ContainsRune(note, '\x00') {
			RespondErrorMsg(c, http.StatusBadRequest, "备注格式无效")
			return
		}
		// Upsert 在数据库侧保留原 created_at；缓存也必须复用已有时间，避免重复禁用不停刷新排序。
		createdAt := time.Now().Unix()
		s.globalDisabledModelsMu.RLock()
		if existing, ok := s.globalDisabledModels[globalDisabledModelKey(modelName)]; ok && existing.CreatedAt > 0 {
			createdAt = existing.CreatedAt
		}
		s.globalDisabledModelsMu.RUnlock()
		entry := model.GlobalDisabledModel{Model: modelName, Note: note, CreatedAt: createdAt}
		if err := store.UpsertGlobalDisabledModel(c.Request.Context(), entry); err != nil {
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
		s.cacheGlobalDisabledModel(entry)
		RespondJSON(c, http.StatusOK, gin.H{"model": modelName})
	default:
		RespondErrorMsg(c, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) HandleDeleteGlobalDisabledModel(c *gin.Context) {
	modelName, err := normalizeGlobalDisabledModelName(c.Query("model"))
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	store, err := s.disabledModelStore()
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if err := store.DeleteGlobalDisabledModel(c.Request.Context(), modelName); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	s.removeCachedGlobalDisabledModel(modelName)
	RespondJSON(c, http.StatusOK, gin.H{"model": modelName})
}
