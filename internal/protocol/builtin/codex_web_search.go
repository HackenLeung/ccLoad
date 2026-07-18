package builtin

import (
	"fmt"
	"strings"
)

func codexWebSearchHistoryText(item map[string]any) string {
	call := codexWebSearchCallFromItem(item)
	if call.Action != "" && call.Action != "search" {
		return codexWebSearchNonSearchHistoryText(call)
	}
	if len(call.Queries) == 0 {
		return "[web search performed]"
	}
	return fmt.Sprintf("[web search performed: %s]", strings.Join(call.Queries, "; "))
}

func codexWebSearchNonSearchHistoryText(call codexWebSearchCall) string {
	if call.Action == "" {
		return ""
	}
	switch call.Action {
	case "open_page", "find_in_page":
		return fmt.Sprintf("[web search %s performed]", call.Action)
	default:
		return fmt.Sprintf("[web search action %s performed]", call.Action)
	}
}

type codexWebSearchCall struct {
	ID        string
	Action    string
	Queries   []string
	Sources   []codexWebSearchSource
	Completed bool
}

type codexWebSearchSource struct {
	URL   string
	Title string
}

func codexWebSearchCallFromItem(item map[string]any) codexWebSearchCall {
	call := codexWebSearchCall{
		ID:        firstNonEmptyString(item, "id", "call_id"),
		Completed: stringValue(item["status"]) != "failed",
	}
	if call.ID == "" {
		call.ID = "web_search"
	}
	action, _ := item["action"].(map[string]any)
	if action != nil {
		call.Action = normalizeRole(stringValue(action["type"]))
		call.Queries = appendWebSearchQueries(call.Queries, action["queries"])
		if query := strings.TrimSpace(stringValue(action["query"])); query != "" {
			call.Queries = append(call.Queries, query)
		}
		call.Sources = appendWebSearchSources(call.Sources, action["sources"])
	}
	call.Queries = appendWebSearchQueries(call.Queries, item["queries"])
	call.Queries = uniqueNonEmptyStrings(call.Queries)
	call.Sources = appendWebSearchSources(call.Sources, item["sources"])
	call.Sources = uniqueWebSearchSources(call.Sources)

	return call
}

func appendWebSearchQueries(dst []string, raw any) []string {
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if query := strings.TrimSpace(stringValue(item)); query != "" {
				dst = append(dst, query)
			}
		}
	case []string:
		for _, item := range v {
			if query := strings.TrimSpace(item); query != "" {
				dst = append(dst, query)
			}
		}
	}
	return dst
}

func appendWebSearchSources(dst []codexWebSearchSource, raw any) []codexWebSearchSource {
	items, ok := raw.([]any)
	if !ok {
		return dst
	}
	for _, rawItem := range items {
		source, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		url := strings.TrimSpace(firstNonEmptyString(source, "url", "uri"))
		if url == "" {
			continue
		}
		dst = append(dst, codexWebSearchSource{
			URL:   url,
			Title: strings.TrimSpace(firstNonEmptyString(source, "title", "text")),
		})
	}
	return dst
}

func uniqueNonEmptyStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func uniqueWebSearchSources(values []codexWebSearchSource) []codexWebSearchSource {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]codexWebSearchSource, 0, len(values))
	for _, value := range values {
		url := strings.TrimSpace(value.URL)
		if url == "" {
			continue
		}
		if _, ok := seen[url]; ok {
			continue
		}
		seen[url] = struct{}{}
		value.URL = url
		value.Title = strings.TrimSpace(value.Title)
		out = append(out, value)
	}
	return out
}

func anthropicWebSearchBlocksFromCodexItem(item map[string]any) ([]map[string]any, bool, bool) {
	call := codexWebSearchCallFromItem(item)
	if call.Action != "" && call.Action != "search" {
		return nil, false, false
	}
	input := map[string]any{}
	if len(call.Queries) > 1 {
		input["queries"] = call.Queries
	} else if len(call.Queries) == 1 {
		input["query"] = call.Queries[0]
	} else {
		input["query"] = ""
	}

	var resultContent any
	if call.Completed {
		hits := make([]map[string]any, 0, len(call.Sources))
		for _, source := range call.Sources {
			hits = append(hits, map[string]any{
				"type":  "web_search_result",
				"title": source.Title,
				"url":   source.URL,
			})
		}
		resultContent = hits
	} else {
		resultContent = map[string]any{
			"type":       "web_search_tool_result_error",
			"error_code": "unavailable",
		}
	}

	return []map[string]any{
		{
			"type":  "server_tool_use",
			"id":    call.ID,
			"name":  "web_search",
			"input": input,
		},
		{
			"type":        "web_search_tool_result",
			"tool_use_id": call.ID,
			"content":     resultContent,
		},
	}, call.Completed, true
}

func anthropicWebSearchBlocksFromCodexOutput(output any) ([]map[string]any, int64) {
	items, _ := output.([]any)
	if len(items) == 0 {
		return nil, 0
	}
	blocks := make([]map[string]any, 0)
	var completedCount int64
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || normalizeRole(stringValue(item["type"])) != "web_search_call" {
			continue
		}
		searchBlocks, completed, convertible := anthropicWebSearchBlocksFromCodexItem(item)
		if !convertible {
			continue
		}
		blocks = append(blocks, searchBlocks...)
		if completed {
			completedCount++
		}
	}
	return blocks, completedCount
}

func anthropicWebSearchServerToolHistoryText(block map[string]any) string {
	input, _ := block["input"].(map[string]any)
	queries := appendWebSearchQueries(nil, input["queries"])
	if query := strings.TrimSpace(stringValue(input["query"])); query != "" {
		queries = append(queries, query)
	}
	queries = uniqueNonEmptyStrings(queries)
	if len(queries) == 0 {
		return "[web search requested]"
	}
	return fmt.Sprintf("[web search requested: %s]", strings.Join(queries, "; "))
}

func anthropicWebSearchToolResultHistoryText(block map[string]any) string {
	switch content := block["content"].(type) {
	case []any:
		lines := make([]string, 0, len(content))
		for _, raw := range content {
			item, ok := raw.(map[string]any)
			if !ok || normalizeRole(stringValue(item["type"])) != "web_search_result" {
				continue
			}
			if text := anthropicWebSearchResultHistoryText(item); text != "" {
				lines = append(lines, text)
			}
		}
		if len(lines) == 0 {
			return "[web search completed]"
		}
		return "[web search results]\n" + strings.Join(lines, "\n")
	case map[string]any:
		if normalizeRole(stringValue(content["type"])) == "web_search_tool_result_error" {
			return "[web search failed]"
		}
	}
	return "[web search completed]"
}

func anthropicWebSearchResultHistoryText(block map[string]any) string {
	url := strings.TrimSpace(stringValue(block["url"]))
	title := strings.TrimSpace(stringValue(block["title"]))
	switch {
	case title != "" && url != "":
		return fmt.Sprintf("- %s: %s", title, url)
	case url != "":
		return "- " + url
	case title != "":
		return "- " + title
	default:
		return ""
	}
}
