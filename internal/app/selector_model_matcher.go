package app

import (
	modelpkg "ccLoad/internal/model"
)

// configSupportsModel 检查渠道是否支持指定模型
func (s *Server) configSupportsModel(cfg *modelpkg.Config, model string, clientProtocol ...string) bool {
	if model == "*" {
		return true
	}
	return cfg.SupportsModel(model, clientProtocol...)
}

// configSupportsModelWithFuzzyMatch 检查渠道是否支持指定模型（含模糊匹配）
//
// 匹配策略（按优先级）：
// 1. 精确匹配：cfg.SupportsModel(model)
// 2. 模糊匹配（需启用 model_fuzzy_match）：sonnet → claude-sonnet-4-5-20250929
//
// 模糊匹配会返回匹配到的完整模型名，在 prepareRequestBody 中用于替换请求体中的模型名。
func (s *Server) configSupportsModelWithFuzzyMatch(cfg *modelpkg.Config, model string, clientProtocol ...string) bool {
	if s.configSupportsModel(cfg, model, clientProtocol...) {
		return true
	}
	if model == "*" {
		return false
	}

	// 模糊匹配：sonnet -> claude-sonnet-4-5-20250929
	if s.modelFuzzyMatch {
		if _, ok := cfg.FuzzyMatchModel(model, clientProtocol...); ok {
			return true
		}
	}

	return false
}
