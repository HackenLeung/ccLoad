package app

import (
	"context"
	"errors"
	"log"
	"net/http"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
)

// runProxyAttemptLoop 按优先级遍历候选渠道。
// 返回最后一次结果（可能 nil），调用方据此决定是否兜底响应。
// succeeded 时内部已写响应，调用方应停止后续 writeFinal 步骤。
func (s *Server) runProxyAttemptLoop(
	ctx context.Context,
	cands []*model.Config,
	reqCtx *proxyRequestContext,
	w http.ResponseWriter,
) (lastResult *proxyResult, succeeded bool) {
	var lastVisionAssistErr error
	for _, cfg := range cands {
		if cfg == nil {
			continue
		}
		enabled, err := s.isChannelEnabledForAttempt(ctx, cfg)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return buildCtxDoneResult(cfg, ctxErr), false
			}
			log.Printf("[WARN] 检查渠道 %s (ID=%d) 启用状态失败，跳过本次尝试: %v", cfg.Name, cfg.ID, err)
			continue
		}
		if !enabled {
			log.Printf("[INFO] 渠道 %s (ID=%d) 已禁用，跳过本次尝试", cfg.Name, cfg.ID)
			continue
		}

		if err := s.prepareVisionAssistForChannel(ctx, cfg, reqCtx); err != nil {
			lastVisionAssistErr = err
			continue
		}
		result, err := s.tryChannelWithKeys(ctx, cfg, reqCtx, w)
		// 记录路由决策，不记录 API Key。
		if result != nil {
			log.Printf("[PROXY_ATTEMPT] request=%d channel=%d name=%q status=%d action=%v succeeded=%t duration=%.3fs base_url=%q",
				reqCtx.activeReqID, cfg.ID, cfg.Name, result.status, result.nextAction, result.succeeded,
				result.duration, reqCtx.baseURL)
		} else if err != nil {
			log.Printf("[PROXY_ATTEMPT] request=%d channel=%d name=%q error=%v base_url=%q",
				reqCtx.activeReqID, cfg.ID, cfg.Name, err, reqCtx.baseURL)
		}
		// 所有Key冷却：触发渠道级冷却(503)，防止后续请求重复尝试
		// 使用 cooldownManager.HandleError 统一处理（DRY原则）
		if err != nil && errors.Is(err, ErrAllKeysUnavailable) {
			// 统一走 applyCooldownDecision：断开取消链+按决策执行缓存失效
			s.applyCooldownDecision(ctx, cfg, httpErrorInputFromParts(cfg.ID, cooldown.NoKeyIndex, 503, nil, nil))
			continue
		}

		// [WARN] 所有Key验证失败，尝试下一个渠道
		if err != nil && errors.Is(err, ErrAllKeysExhausted) {
			log.Printf("[WARN] 渠道 %s (ID=%d) 所有Key验证失败，跳过该渠道", cfg.Name, cfg.ID)
			continue
		}

		if err != nil && errors.Is(err, ErrChannelRPMExceeded) {
			log.Printf("[INFO] 渠道 %s (ID=%d) 已达到RPM限制，跳过该渠道", cfg.Name, cfg.ID)
			continue
		}

		if err != nil && errors.Is(err, ErrChannelConcurrencyExceeded) {
			log.Printf("[INFO] 渠道 %s (ID=%d) 已达到并发限制，跳过该渠道", cfg.Name, cfg.ID)
			continue
		}

		if result != nil {
			if result.channelDisabled {
				log.Printf("[INFO] 渠道 %s (ID=%d) 在请求重试期间被禁用，跳过后续尝试", cfg.Name, cfg.ID)
				continue
			}
			if result.succeeded {
				return nil, true
			}

			lastResult = result
			if result.manualChannelSkip {
				log.Printf("[INFO] 手动跳过渠道 %s (ID=%d)，继续下一个候选渠道", cfg.Name, cfg.ID)
				continue
			}

			// 客户端已取消：别再浪费资源“重试”了。
			if result.isClientCanceled {
				break
			}

			if shouldStopTryingChannels(result) && !reqCtx.requireNativeGPT {
				break
			}
		}
	}
	if lastResult == nil && lastVisionAssistErr != nil {
		return visionAssistFailureResult(lastVisionAssistErr), false
	}

	return lastResult, false
}
