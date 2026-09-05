package app

import (
	"bytes"
	"encoding/json"
	"log"
)

func (p *jsonUsageParser) Feed(data []byte) error {
	if len(data) > 0 {
		p.hasBody = true
	}
	p.scanJSONUsage(data)

	if p.truncated {
		return nil
	}
	if p.buffer.Len()+len(data) > maxUsageBodySize {
		p.truncated = true
		p.buffer = bytes.Buffer{}
		log.Printf("[WARN] usage 响应体超过最大长度（%d 字节），切换到流式 usage 提取", maxUsageBodySize)
		return nil
	}
	_, err := p.buffer.Write(data)
	return err
}

func (p *jsonUsageParser) scanJSONUsage(data []byte) {
	for _, b := range data {
		if p.scanCaptureKey != "" {
			p.scanJSONCaptureByte(b)
			continue
		}
		if p.scanInString {
			p.scanJSONStringByte(b)
			continue
		}
		if p.scanExpectValue {
			if isJSONWhitespace(b) {
				continue
			}
			switch p.scanPendingKey {
			case "usage", "usageMetadata", "usage_metadata", "tool_usage":
				if b == '{' {
					p.startJSONValueCapture(b)
					continue
				}
			case "service_tier":
				if b == '"' {
					p.startJSONValueCapture(b)
					continue
				}
			}
			p.clearJSONPendingKey()
		}
		if p.scanHaveToken {
			if isJSONWhitespace(b) {
				continue
			}
			if b == ':' {
				p.scanPendingKey = p.scanStringToken
				p.scanExpectValue = true
				p.scanHaveToken = false
				p.scanStringToken = ""
				continue
			}
			p.scanHaveToken = false
			p.scanStringToken = ""
		}
		if b == '"' {
			p.scanInString = true
			p.scanEscape = false
			p.scanStringBuf = p.scanStringBuf[:0]
			p.scanStringTooLong = false
		}
	}
}

func (p *jsonUsageParser) scanJSONStringByte(b byte) {
	if p.scanEscape {
		p.scanEscape = false
		p.appendJSONKeyByte(b)
		return
	}
	switch b {
	case '\\':
		p.scanEscape = true
	case '"':
		p.scanInString = false
		if !p.scanStringTooLong {
			p.scanHaveToken = true
			p.scanStringToken = string(p.scanStringBuf)
		}
	default:
		p.appendJSONKeyByte(b)
	}
}

func (p *jsonUsageParser) appendJSONKeyByte(b byte) {
	if p.scanStringTooLong {
		return
	}
	if len(p.scanStringBuf) >= maxJSONKeySize {
		p.scanStringTooLong = true
		p.scanStringBuf = p.scanStringBuf[:0]
		return
	}
	p.scanStringBuf = append(p.scanStringBuf, b)
}

func (p *jsonUsageParser) startJSONValueCapture(first byte) {
	p.scanCaptureKey = p.scanPendingKey
	p.scanCaptureBuf = p.scanCaptureBuf[:0]
	p.scanCaptureDepth = 0
	p.scanCaptureString = false
	p.scanCaptureEscape = false
	p.scanCaptureDiscard = false
	p.clearJSONPendingKey()
	p.scanJSONCaptureByte(first)
}

func (p *jsonUsageParser) scanJSONCaptureByte(b byte) {
	if !p.scanCaptureDiscard {
		if len(p.scanCaptureBuf) >= maxJSONUsageFragmentSize {
			p.scanCaptureDiscard = true
			p.scanCaptureBuf = p.scanCaptureBuf[:0]
		} else {
			p.scanCaptureBuf = append(p.scanCaptureBuf, b)
		}
	}

	if p.scanCaptureString {
		if p.scanCaptureEscape {
			p.scanCaptureEscape = false
			return
		}
		switch b {
		case '\\':
			p.scanCaptureEscape = true
		case '"':
			p.scanCaptureString = false
			if p.scanCaptureDepth == 0 {
				p.finishJSONValueCapture()
			}
		}
		return
	}

	switch b {
	case '"':
		p.scanCaptureString = true
	case '{':
		p.scanCaptureDepth++
	case '}':
		if p.scanCaptureDepth > 0 {
			p.scanCaptureDepth--
		}
		if p.scanCaptureDepth == 0 {
			p.finishJSONValueCapture()
		}
	}
}

func (p *jsonUsageParser) finishJSONValueCapture() {
	key := p.scanCaptureKey
	discard := p.scanCaptureDiscard
	if !discard && len(p.scanCaptureBuf) > 0 {
		switch key {
		case "usage", "usageMetadata", "usage_metadata":
			var usage map[string]any
			if err := json.Unmarshal(p.scanCaptureBuf, &usage); err == nil {
				p.applyUsageMap(usage)
			}
		case "tool_usage":
			var toolUsage map[string]any
			if err := json.Unmarshal(p.scanCaptureBuf, &toolUsage); err == nil {
				p.applyToolUsageMap(toolUsage, "")
			}
		case "service_tier":
			var tier string
			if err := json.Unmarshal(p.scanCaptureBuf, &tier); err == nil && tier != "" {
				p.ServiceTier = tier
			}
		}
	}
	p.scanCaptureKey = ""
	p.scanCaptureBuf = p.scanCaptureBuf[:0]
	p.scanCaptureDepth = 0
	p.scanCaptureString = false
	p.scanCaptureEscape = false
	p.scanCaptureDiscard = false
}

func (p *jsonUsageParser) applyUsageMap(usage map[string]any) {
	if usage == nil {
		return
	}
	if speed, ok := usage["speed"].(string); ok && speed == "fast" {
		p.ServiceTier = "fast"
	}
	p.applyUsage(usage, p.channelType)
}

func (p *jsonUsageParser) clearJSONPendingKey() {
	p.scanPendingKey = ""
	p.scanExpectValue = false
}

func isJSONWhitespace(b byte) bool {
	return b == ' ' || b == '\n' || b == '\r' || b == '\t'
}

func (p *jsonUsageParser) GetUsage() (inputTokens, outputTokens, cacheRead, cacheCreation int) {
	if p.truncated {
		return p.normalizedUsage(p.channelType)
	}
	if p.buffer.Len() == 0 {
		return p.normalizedUsage(p.channelType)
	}

	data := p.buffer.Bytes()

	// 兼容 text/plain SSE 回退：上游偶尔用 text/plain 发送 SSE 事件
	if looksLikeSSE(data) {
		sseParser := newSSEUsageParser(p.channelType)
		if err := sseParser.Feed(data); err != nil {
			log.Printf("[WARN] 类 SSE 格式的 usage 解析失败: %v", err)
		} else {
			p.ServiceTier = sseParser.ServiceTier
			p.ThinkingEffort = sseParser.GetThinkingEffort()
			p.ReasoningTokens = sseParser.GetReasoningTokens()
			p.ToolCostUSD = sseParser.GetToolCostUSD()
			return sseParser.GetUsage()
		}
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("[WARN] usage JSON 解析失败: %v", err)
		return 0, 0, 0, 0
	}

	usage := extractUsage(payload)
	// Anthropic fast mode: 从 usage.speed 推断计费层级
	p.applyUsageMap(usage)
	p.applyToolUsageFromPayload(payload)
	if effort := extractThinkingEffortFromPayload(payload); effort != "" {
		p.ThinkingEffort = effort
	}

	// 提取 service_tier（OpenAI Chat/Responses API 顶层字段）
	if tier, ok := payload["service_tier"].(string); ok && tier != "" {
		p.ServiceTier = tier
	} else if resp, ok := payload["response"].(map[string]any); ok {
		if tier, ok := resp["service_tier"].(string); ok && tier != "" {
			p.ServiceTier = tier
		}
	}

	return p.normalizedUsage(p.channelType)
}

// [INFO] GetLastError 返回nil（jsonUsageParser不处理SSE error事件）
func (p *jsonUsageParser) GetLastError() []byte {
	return nil // JSON解析器不处理SSE error事件
}

// [INFO] IsStreamComplete 返回false（非流式请求无结束标志概念）
func (p *jsonUsageParser) IsStreamComplete() bool {
	return false // JSON解析器不处理流结束标志
}

func (p *jsonUsageParser) HasStreamOutput() bool {
	return p.hasBody
}
