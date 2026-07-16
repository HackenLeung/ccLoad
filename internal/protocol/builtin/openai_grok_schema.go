package builtin

// normalizeGrokToolSchema handles the stricter JSON-schema subset accepted by
// Grok's Chat Completions tool endpoint. In particular, Grok rejects a nested
// root oneOf even when every leaf is an object schema.
func normalizeGrokToolSchema(schema any) any {
	root, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	rawOneOf, hasOneOf := root["oneOf"].([]any)
	if !hasOneOf {
		return schema
	}
	flat := make([]any, 0, len(rawOneOf))
	var appendBranch func(any) bool
	appendBranch = func(raw any) bool {
		branch, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		if nested, ok := branch["oneOf"].([]any); ok {
			for _, child := range nested {
				if !appendBranch(child) {
					return false
				}
			}
			return true
		}
		if typ, _ := branch["type"].(string); typ != "" && typ != "object" {
			return false
		}
		copy := cloneMapWithoutKeys(branch)
		if _, exists := copy["type"]; !exists {
			copy["type"] = "object"
		}
		flat = append(flat, copy)
		return true
	}
	for _, branch := range rawOneOf {
		if !appendBranch(branch) {
			return nil
		}
	}
	out := cloneMapWithoutKeys(root, "type", "oneOf")
	if out == nil {
		out = make(map[string]any)
	}
	out["oneOf"] = flat
	return out
}
