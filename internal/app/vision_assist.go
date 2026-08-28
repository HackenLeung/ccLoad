package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	protocolbuiltin "ccLoad/internal/protocol/builtin"

	"github.com/bytedance/sonic"
)

func (s *Server) prepareVisionAssistForChannel(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext) error {
	// 每个主请求只执行一次完整的视觉池尝试，避免主请求切换渠道后从头重复。
	// visionPrepared 表示图片已改写/移除；visionAttempted 表示视觉池已完整执行完毕。
	if reqCtx == nil || cfg == nil || len(reqCtx.body) == 0 || reqCtx.visionPrepared || reqCtx.visionAttempted {
		return nil
	}
	target := cfg.FindModelEntry(reqCtx.originalModel, string(reqCtx.clientProtocol))
	if target == nil || !target.VisionAssistEnabled {
		return nil
	}

	_, hasImages, err := protocolbuiltin.BuildVisionAssistRequest(reqCtx.clientProtocol, reqCtx.body, "vision-probe")
	if err != nil {
		return fmt.Errorf("inspect vision assist request: %w", err)
	}
	if !hasImages {
		return nil
	}
	cacheKey, cacheable, err := protocolbuiltin.VisionAssistCacheKey(reqCtx.clientProtocol, reqCtx.body)
	if err != nil {
		return fmt.Errorf("build vision assist cache key: %w", err)
	}
	cacheKey = visionAssistCacheTokenKey(reqCtx.tokenHash, cacheKey)
	if cacheable && s.visionAssistCache != nil {
		if description, ok := s.visionAssistCache.Get(cacheKey); ok {
			rewritten, rewriteErr := protocolbuiltin.RewriteImagesAsText(reqCtx.clientProtocol, reqCtx.body, description)
			if rewriteErr != nil {
				return fmt.Errorf("rewrite request from cached vision assist: %w", rewriteErr)
			}
			replaceVisionRequestBody(reqCtx, rewritten)
			return nil
		}
	}
	pool, err := s.visionPoolForChannel(ctx, cfg, reqCtx)
	if err != nil {
		return err
	}
	if len(pool) == 0 {
		rewritten, rewriteErr := protocolbuiltin.RemoveImages(reqCtx.clientProtocol, reqCtx.body)
		if rewriteErr != nil {
			return fmt.Errorf("remove images without a vision model: %w", rewriteErr)
		}
		replaceVisionRequestBody(reqCtx, rewritten)
		return nil
	}
	// 从这里开始，这一次主请求会遍历完整视觉池；即使所有候选失败，后续主渠道
	// 切换时也不应再从头调用视觉模型，而是复用下面的纯文本降级结果。
	reqCtx.visionAttempted = true

	var lastErr error
	for _, candidate := range pool {
		visionBody, _, err := protocolbuiltin.BuildVisionAssistRequest(reqCtx.clientProtocol, reqCtx.body, candidate.entry.Model)
		if err != nil {
			return fmt.Errorf("prepare vision assist request: %w", err)
		}

		description, err := s.callVisionModel(ctx, candidate.channel, candidate.entry.Model, visionBody, reqCtx)
		if err != nil {
			lastErr = err
			continue
		}
		rewritten, err := protocolbuiltin.RewriteImagesAsText(reqCtx.clientProtocol, reqCtx.body, description)
		if err != nil {
			return fmt.Errorf("rewrite request after vision assist: %w", err)
		}
		if cacheable && s.visionAssistCache != nil {
			s.visionAssistCache.Put(cacheKey, description)
		}
		replaceVisionRequestBody(reqCtx, rewritten)
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no vision model was attempted")
	}
	// 所有视觉候选均失败：降级为移除图片继续主请求，不再重复请求视觉模型。
	// （与「未配置视觉池」时的降级策略一致，保证主请求仍有机会成功。）
	rewritten, rewriteErr := protocolbuiltin.RemoveImages(reqCtx.clientProtocol, reqCtx.body)
	if rewriteErr != nil {
		return fmt.Errorf("remove images after vision assist failure: %w", rewriteErr)
	}
	replaceVisionRequestBody(reqCtx, rewritten)
	log.Printf("[WARN] vision assist failed on channel %s, degraded to text-only: %v", cfg.Name, lastErr)
	return nil
}

type visionAssistCandidate struct {
	channel *model.Config
	entry   model.ModelEntry
}

func (s *Server) visionPoolForChannel(ctx context.Context, preferred *model.Config, reqCtx *proxyRequestContext) ([]visionAssistCandidate, error) {
	if !reqCtx.visionPoolLoaded {
		configs, err := s.store.ListConfigs(ctx)
		if err != nil {
			return nil, fmt.Errorf("load vision pool: %w", err)
		}
		for _, channel := range configs {
			if channel == nil || !channel.Enabled {
				continue
			}
			for _, entry := range channel.ModelEntries {
				if entry.VisionPoolEnabled {
					reqCtx.visionPool = append(reqCtx.visionPool, visionAssistCandidate{channel: channel, entry: entry})
				}
			}
		}
		reqCtx.visionPoolLoaded = true
	}

	pool := append([]visionAssistCandidate(nil), reqCtx.visionPool...)
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].entry.VisionPriority != pool[j].entry.VisionPriority {
			return pool[i].entry.VisionPriority > pool[j].entry.VisionPriority
		}
		if pool[i].channel.Priority != pool[j].channel.Priority {
			return pool[i].channel.Priority > pool[j].channel.Priority
		}
		if pool[i].channel.ID != pool[j].channel.ID {
			return pool[i].channel.ID < pool[j].channel.ID
		}
		return strings.ToLower(pool[i].entry.Model) < strings.ToLower(pool[j].entry.Model)
	})
	return pool, nil
}

func replaceVisionRequestBody(reqCtx *proxyRequestContext, body []byte) {
	reqCtx.body = body
	reqCtx.translatedBody = body
	reqCtx.visionPrepared = true
}

func sameChannelVisionPool(cfg *model.Config) []model.ModelEntry {
	pool := make([]model.ModelEntry, 0)
	for _, entry := range cfg.ModelEntries {
		if entry.VisionPoolEnabled {
			pool = append(pool, entry)
		}
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].VisionPriority != pool[j].VisionPriority {
			return pool[i].VisionPriority > pool[j].VisionPriority
		}
		return strings.ToLower(pool[i].Model) < strings.ToLower(pool[j].Model)
	})
	return pool
}

func (s *Server) callVisionModel(
	ctx context.Context,
	cfg *model.Config,
	visionModel string,
	body []byte,
	parent *proxyRequestContext,
) (string, error) {
	header := parent.header.Clone()
	header.Set("Content-Type", "application/json")
	header.Del("Content-Length")
	visionCtx := &proxyRequestContext{
		originalModel:      visionModel,
		clientProtocol:     protocol.OpenAI,
		requestMethod:      http.MethodPost,
		requestPath:        "/v1/chat/completions",
		body:               body,
		translatedBody:     body,
		header:             header,
		isStreaming:        false,
		tokenHash:          parent.tokenHash,
		tokenID:            parent.tokenID,
		clientIP:           parent.clientIP,
		startTime:          time.Now(),
		logSource:          model.LogSourceVisionAssist,
		isolatedSubrequest: true,
	}
	// 视觉转文字子请求独立注册为只读“进行中”行：匹配到视觉模型的那一刻日志页即可看到，
	// 不必等子请求结束落库；随本子请求生命周期起止，不可被单独跳过/取消。
	if s.activeRequests != nil {
		activeID := s.activeRequests.RegisterSub(time.Now(), visionModel, parent.clientIP, model.LogSourceVisionAssist, false)
		s.activeRequests.SetSubrequestChannel(activeID, cfg.ID, cfg.Name, cfg.GetChannelType(), cfg.ResolveUpstreamProtocol(string(protocol.OpenAI)), cfg.CostMultiplier)
		defer s.activeRequests.Remove(activeID)
	}
	recorder := httptest.NewRecorder()
	result, err := s.tryChannelWithKeys(ctx, cfg, visionCtx, recorder)
	if err != nil {
		return "", err
	}
	if result == nil || !result.succeeded {
		status := recorder.Code
		if result != nil && result.status != 0 {
			status = result.status
		}
		return "", fmt.Errorf("model %s returned status %d", visionModel, status)
	}
	description, err := protocolbuiltin.ExtractVisionAssistText(recorder.Body.Bytes())
	if err != nil {
		return "", fmt.Errorf("parse model %s response: %w", visionModel, err)
	}
	return description, nil
}

func visionAssistFailureResult(err error) *proxyResult {
	body, marshalErr := sonic.Marshal(map[string]any{"error": map[string]any{
		"message": err.Error(),
		"type":    "vision_assist_error",
		"code":    "vision_assist_failed",
	}})
	if marshalErr != nil {
		body = []byte(`{"error":"vision assist failed"}`)
	}
	return &proxyResult{
		status:     http.StatusBadGateway,
		header:     http.Header{"Content-Type": []string{"application/json"}},
		body:       body,
		succeeded:  false,
		nextAction: 0,
	}
}
