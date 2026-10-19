package translate

import (
	"encoding/json"
	"strings"
)

// Copilot's Chat Completions gateway returns Claude reasoning in
// `reasoning_text` (visible summary) and `reasoning_opaque` (signature)
// fields. The protocol translators only read `reasoning_content`, so without
// renaming, thinking is silently dropped. `reasoning_opaque` is ciphertext
// with no protocol equivalent; it is left in place and ignored downstream.

// NormalizeChatCompletionsResponse renames reasoning fields inside a
// non-stream Chat Completions response body.
func NormalizeChatCompletionsResponse(body []byte) ([]byte, error) {
	root, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	if renameChatReasoning(root) {
		return json.Marshal(root)
	}
	return body, nil
}

// NormalizeChatCompletionsFrame renames reasoning fields inside one Chat
// Completions SSE frame. Event lines are preserved verbatim; only `data:`
// JSON payloads are rewritten. Frames without JSON payloads pass through.
func NormalizeChatCompletionsFrame(frame []byte) []byte {
	normalized := strings.ReplaceAll(string(frame), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	changed := false
	for i, line := range lines {
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload := strings.TrimSpace(data)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		root, err := decodeObject([]byte(payload))
		if err != nil {
			continue
		}
		if !renameChatReasoning(root) {
			continue
		}
		encoded, errMarshal := json.Marshal(root)
		if errMarshal != nil {
			continue
		}
		lines[i] = "data: " + string(encoded)
		changed = true
	}
	if !changed {
		return frame
	}
	return []byte(strings.Join(lines, "\n"))
}

// renameChatReasoning moves choices[].delta/message reasoning_text into
// reasoning_content when the latter is absent or empty. It reports whether
// the object changed.
func renameChatReasoning(root map[string]any) bool {
	changed := false
	for _, rawChoice := range arrayValue(root["choices"]) {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"delta", "message"} {
			part, ok := choice[key].(map[string]any)
			if !ok {
				continue
			}
			text, hasText := part["reasoning_text"].(string)
			if !hasText || text == "" {
				continue
			}
			if existing, _ := part["reasoning_content"].(string); existing == "" {
				part["reasoning_content"] = text
			}
			delete(part, "reasoning_text")
			changed = true
		}
	}
	return changed
}

// MessagesThinkingOptions describes a model's thinking controls as advertised
// by the Copilot model catalog.
type MessagesThinkingOptions struct {
	AdaptiveThinking bool
	ReasoningEfforts []string
}

// PrepareMessagesThinking converts budget-style thinking into adaptive
// thinking for models whose catalog reports adaptive_thinking. Copilot's
// Anthropic Messages endpoint rejects thinking.type=enabled for those models
// and requires thinking.type=adaptive with output_config.effort instead.
// requestedEffort carries the level the client asked for in its native
// protocol; when empty it is derived from the translated budget_tokens.
func PrepareMessagesThinking(body []byte, requestedEffort string, opts MessagesThinkingOptions) []byte {
	if !opts.AdaptiveThinking {
		return body
	}
	root, err := decodeObject(body)
	if err != nil {
		return body
	}
	thinking, ok := root["thinking"].(map[string]any)
	if !ok || stringValue(thinking["type"]) != "enabled" {
		return body
	}
	budget := int64Value(thinking["budget_tokens"])
	thinking["type"] = "adaptive"
	delete(thinking, "budget_tokens")
	root["thinking"] = thinking
	effort := firstNonEmptyString(
		stringValue(objectValue(root["output_config"])["effort"]),
		requestedEffort,
		budgetToEffort(budget),
	)
	if effort = clampEffort(effort, opts.ReasoningEfforts); effort != "" {
		outputConfig := objectValue(root["output_config"])
		outputConfig["effort"] = effort
		root["output_config"] = outputConfig
	}
	out, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return body
	}
	return out
}

// NormalizeResponsesStringInput converts a Responses request whose `input` is
// a plain string into the message-array form. The protocol translators only
// walk input arrays, so a string input would otherwise translate into a
// request without messages and fail upstream.
func NormalizeResponsesStringInput(body []byte) []byte {
	root, err := decodeObject(body)
	if err != nil {
		return body
	}
	text, ok := root["input"].(string)
	if !ok || text == "" {
		return body
	}
	root["input"] = []any{map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{map[string]any{
			"type": "input_text",
			"text": text,
		}},
	}}
	out, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return body
	}
	return out
}

func budgetToEffort(budget int64) string {
	switch {
	case budget <= 0:
		return ""
	case budget <= 2048:
		return "low"
	case budget <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// clampEffort maps the requested level onto the model's supported efforts.
// Without catalog levels every effort passes through; otherwise client-only
// names collapse ("minimal" to "low", "auto" to the model default) and
// unknown levels clamp to the highest supported one.
func clampEffort(effort string, levels []string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return ""
	}
	if len(levels) == 0 {
		return effort
	}
	for _, level := range levels {
		if strings.EqualFold(strings.TrimSpace(level), effort) {
			return effort
		}
	}
	switch effort {
	case "minimal":
		return clampEffort("low", levels)
	case "auto":
		return ""
	default:
		return strings.ToLower(strings.TrimSpace(levels[len(levels)-1]))
	}
}
