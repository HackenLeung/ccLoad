package app

import (
	"net/http"
	"strings"
)

// 客户端软件标识（持久化到 logs.client_name，同时作为日志页筛选值）。
// 取值保持稳定，避免历史日志因改名而无法按同一维度聚合。
const (
	ClientNameClaudeCode   = "claude-code"   // Anthropic 官方 CLI
	ClientNameCodexCLI     = "codex-cli"     // OpenAI Codex CLI
	ClientNameGeminiCLI    = "gemini-cli"    // Google Gemini CLI
	ClientNameAnthropicSDK = "anthropic-sdk" // Anthropic 官方 SDK（Python/JS/Go 等）
	ClientNameOpenAISDK    = "openai-sdk"    // OpenAI 官方 SDK
	ClientNameGeminiSDK    = "gemini-sdk"    // Google GenAI SDK
	ClientNameCursor       = "cursor"        // Cursor 编辑器
	ClientNameWindsurf     = "windsurf"      // Windsurf 编辑器
	ClientNameVSCode       = "vscode"        // VS Code 及其 AI 插件
	ClientNameCherryStudio = "cherry-studio" // Cherry Studio 桌面客户端
	ClientNameOther        = "other"         // 已识别为未知但存在 UA
)

// clientUAMaxStoreLen 与 logs.client_ua 列宽对齐（VARCHAR(191)）。
// 截断而非丢弃：前 191 字节已足够覆盖 UA 的产物名与版本号，足以校准归类规则。
const clientUAMaxStoreLen = 191

// classifyClient 从请求头推断调用方软件，返回归类名与截断后的原始 UA。
//
// 设计原则（YAGNI）：优先读客户端主动声明的特征头（originator/x-app），
// 这些是上游官方 CLI 自报的稳定标识；只有缺失时才回退到 UA 前缀匹配，
// 因为 UA 格式会随版本变化，而特征头语义明确。
func classifyClient(header http.Header) (name, ua string) {
	if header == nil {
		return "", ""
	}

	rawUA := strings.TrimSpace(header.Get("User-Agent"))
	ua = truncateClientUA(rawUA)

	// 1) 特征头优先：官方 CLI 会自报 originator，语义明确且不受 UA 格式漂移影响
	switch strings.ToLower(strings.TrimSpace(header.Get("Originator"))) {
	case "codex_cli_rs", "codex_cli", "codex":
		return ClientNameCodexCLI, ua
	}
	if app := strings.ToLower(strings.TrimSpace(header.Get("X-App"))); app != "" {
		if matched := matchClientByName(app); matched != "" {
			return matched, ua
		}
	}

	// 2) UA 匹配
	if matched := matchClientByUA(rawUA); matched != "" {
		return matched, ua
	}

	// 3) 有 UA 但无法归类：保留 other，便于后续用真实样本补规则
	if rawUA != "" {
		return ClientNameOther, ua
	}
	return "", ""
}

// matchClientByName 匹配客户端自报的软件名（x-app 等特征头的值）。
func matchClientByName(app string) string {
	switch {
	case strings.Contains(app, "claude"):
		return ClientNameClaudeCode
	case strings.Contains(app, "codex"):
		return ClientNameCodexCLI
	case strings.Contains(app, "gemini"):
		return ClientNameGeminiCLI
	case strings.Contains(app, "cursor"):
		return ClientNameCursor
	case strings.Contains(app, "windsurf"):
		return ClientNameWindsurf
	}
	return ""
}

// matchClientByUA 按 UA 特征归类。顺序敏感：先匹配更具体的标识，
// 避免 "claude-cli" 被 "claude" 泛匹配提前截胡。
func matchClientByUA(ua string) string {
	lower := strings.ToLower(ua)
	if lower == "" {
		return ""
	}

	switch {
	// 官方 CLI：UA 形如 claude-cli/2.x.x (external, cli)
	case strings.HasPrefix(lower, "claude-cli"):
		return ClientNameClaudeCode
	case strings.Contains(lower, "codex_cli") || strings.HasPrefix(lower, "codex"):
		return ClientNameCodexCLI
	case strings.HasPrefix(lower, "geminicli") || strings.Contains(lower, "gemini-cli"):
		return ClientNameGeminiCLI

	// 编辑器 / 桌面客户端
	case strings.Contains(lower, "cursor"):
		return ClientNameCursor
	case strings.Contains(lower, "windsurf"):
		return ClientNameWindsurf
	case strings.Contains(lower, "cherrystudio") || strings.Contains(lower, "cherry-studio"):
		return ClientNameCherryStudio
	case strings.Contains(lower, "vscode") || strings.Contains(lower, "visual studio code"):
		return ClientNameVSCode

	// 官方 SDK：UA 形如 Anthropic/Python 0.40.0 / OpenAI/NodeJS 4.20.0
	case strings.HasPrefix(lower, "anthropic/"):
		return ClientNameAnthropicSDK
	case strings.HasPrefix(lower, "openai/"):
		return ClientNameOpenAISDK
	case strings.Contains(lower, "google-genai") || strings.Contains(lower, "google.generativeai"):
		return ClientNameGeminiSDK
	}
	return ""
}

// truncateClientUA 按字节截断 UA，避免超长 UA 撑爆列宽。
func truncateClientUA(ua string) string {
	if len(ua) <= clientUAMaxStoreLen {
		return ua
	}
	return ua[:clientUAMaxStoreLen]
}
