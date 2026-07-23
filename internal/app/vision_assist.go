package app

import (
	"context"
	"fmt"
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
	if reqCtx == nil || cfg == nil || len(reqCtx.body) == 0 || reqCtx.visionPrepared {
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
		replaceVisionRequestBody(reqCtx, rewritten)
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no vision model was attempted")
	}
	return fmt.Errorf("vision assist failed on channel %s: %w", cfg.Name, lastErr)
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
		iPreferred := pool[i].channel.ID == preferred.ID
		jPreferred := pool[j].channel.ID == preferred.ID
		if iPreferred != jPreferred {
			return iPreferred
		}
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
