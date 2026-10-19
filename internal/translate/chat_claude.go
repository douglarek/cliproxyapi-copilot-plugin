package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// claudeMessageToChatCompletion converts a complete Claude message response
// into a Chat Completions response body. The builtin non-stream Claude→Chat
// translator only consumes recorded `data:` event lines, so Responses-only
// models served to Chat Completions clients need this direct mapping.
func claudeMessageToChatCompletion(model string, body []byte) ([]byte, error) {
	root, err := decodeObject(body)
	if err != nil {
		return nil, fmt.Errorf("decode Claude message: %w", err)
	}
	if model == "" {
		model = stringValue(root["model"])
	}
	message := map[string]any{"role": "assistant"}
	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls []any
	for _, rawBlock := range arrayValue(root["content"]) {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(block["type"]) {
		case "text":
			content.WriteString(stringValue(block["text"]))
		case "thinking":
			reasoning.WriteString(stringValue(block["thinking"]))
		case "redacted_thinking":
			// Encrypted thinking has no Chat Completions representation.
		case "tool_use":
			arguments, errArguments := json.Marshal(block["input"])
			if errArguments != nil {
				arguments = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   stringValue(block["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      stringValue(block["name"]),
					"arguments": string(arguments),
				},
			})
		}
	}
	message["content"] = content.String()
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	finishReason := claudeStopToFinishReason(stringValue(root["stop_reason"]))
	usage := objectValue(root["usage"])
	promptTokens := int64Value(usage["input_tokens"]) + int64Value(usage["cache_read_input_tokens"]) + int64Value(usage["cache_creation_input_tokens"])
	completionTokens := int64Value(usage["output_tokens"])
	out := map[string]any{
		"id":      stringValue(root["id"]),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
	return json.Marshal(out)
}

// claudeStopToFinishReason maps Claude stop reasons onto Chat Completions
// finish reasons.
func claudeStopToFinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}
