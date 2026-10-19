package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeResponsesStringInput(t *testing.T) {
	t.Parallel()

	out := NormalizeResponsesStringInput([]byte(`{"model":"claude-sonnet-5","input":"Factor 3599.","stream":false}`))
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("decode normalized body: %v", err)
	}
	input, ok := root["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input was not wrapped into one message: %s", out)
	}
	message, _ := input[0].(map[string]any)
	if message["type"] != "message" || message["role"] != "user" {
		t.Fatalf("wrapped message shape = %#v", message)
	}
	content, _ := message["content"].([]any)
	first, _ := content[0].(map[string]any)
	if first["type"] != "input_text" || first["text"] != "Factor 3599." {
		t.Fatalf("wrapped content = %#v", first)
	}
	if root["model"] != "claude-sonnet-5" {
		t.Fatalf("model field lost: %s", out)
	}
}

func TestNormalizeResponsesStringInputLeavesArraysUntouched(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if got := NormalizeResponsesStringInput(original); string(got) != string(original) {
		t.Fatalf("array input was modified: %s", got)
	}
	original = []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if got := NormalizeResponsesStringInput(original); string(got) != string(original) {
		t.Fatalf("claude body was modified: %s", got)
	}
}

func TestNormalizeChatCompletionsResponseMapsReasoningText(t *testing.T) {
	t.Parallel()

	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"answer","reasoning_text":"why","reasoning_opaque":"sig"}}]}`)
	out, err := NormalizeChatCompletionsResponse(body)
	if err != nil {
		t.Fatalf("normalize response: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("decode normalized response: %v", err)
	}
	choice := root["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["reasoning_content"] != "why" {
		t.Fatalf("reasoning_content = %#v, want the mapped reasoning_text", message["reasoning_content"])
	}
	if _, exists := message["reasoning_text"]; exists {
		t.Fatalf("reasoning_text survived: %s", out)
	}
	if message["reasoning_opaque"] != "sig" {
		t.Fatalf("reasoning_opaque must be preserved: %s", out)
	}
	if message["content"] != "answer" {
		t.Fatalf("content lost: %s", out)
	}
}

func TestNormalizeChatCompletionsFrameMapsDeltaReasoning(t *testing.T) {
	t.Parallel()

	frame := []byte("data: {\"choices\":[{\"delta\":{\"reasoning_text\":\"step 1\"}}]}\n\n")
	out := NormalizeChatCompletionsFrame(frame)
	if !strings.HasPrefix(string(out), "data: ") || !strings.Contains(string(out), `"reasoning_content":"step 1"`) {
		t.Fatalf("frame not mapped: %q", out)
	}
	if strings.Contains(string(out), "reasoning_text") {
		t.Fatalf("reasoning_text survived in frame: %q", out)
	}

	untouched := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	if got := NormalizeChatCompletionsFrame(untouched); string(got) != string(untouched) {
		t.Fatalf("plain frame was rewritten: %q", got)
	}
	done := []byte("data: [DONE]\n\n")
	if got := NormalizeChatCompletionsFrame(done); string(got) != string(done) {
		t.Fatalf("terminal frame was rewritten: %q", got)
	}
	named := []byte("event: ping\ndata: {\"choices\":[{\"delta\":{\"reasoning_text\":\"x\"}}]}\n\n")
	got := string(NormalizeChatCompletionsFrame(named))
	if !strings.HasPrefix(got, "event: ping\ndata: ") {
		t.Fatalf("event line not preserved: %q", got)
	}
}

func TestPrepareMessagesThinkingRewritesBudgetThinking(t *testing.T) {
	t.Parallel()

	opts := MessagesThinkingOptions{AdaptiveThinking: true, ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}}
	body := []byte(`{"model":"m","thinking":{"type":"enabled","budget_tokens":4096},"messages":[]}`)
	out := PrepareMessagesThinking(body, "", opts)

	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("decode rewritten body: %v", err)
	}
	thinking := root["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Fatalf("thinking.type = %#v, want adaptive", thinking["type"])
	}
	if _, exists := thinking["budget_tokens"]; exists {
		t.Fatalf("budget_tokens survived: %s", out)
	}
	if root["output_config"].(map[string]any)["effort"] != "medium" {
		t.Fatalf("effort = %s, want medium for a 4096 budget", out)
	}
}

func TestPrepareMessagesThinkingPrefersRequestedEffort(t *testing.T) {
	t.Parallel()

	opts := MessagesThinkingOptions{AdaptiveThinking: true, ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}}
	body := []byte(`{"model":"m","thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`)
	out := PrepareMessagesThinking(body, "xhigh", opts)
	if !strings.Contains(string(out), `"effort":"xhigh"`) {
		t.Fatalf("requested effort not preserved: %s", out)
	}
}

func TestPrepareMessagesThinkingClampsUnknownEffort(t *testing.T) {
	t.Parallel()

	opts := MessagesThinkingOptions{AdaptiveThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}}
	body := []byte(`{"model":"m","thinking":{"type":"enabled","budget_tokens":4096},"messages":[]}`)
	out := PrepareMessagesThinking(body, "max", opts)
	if !strings.Contains(string(out), `"effort":"high"`) {
		t.Fatalf("max effort not clamped to the highest supported level: %s", out)
	}
}

func TestPrepareMessagesThinkingLeavesOtherStatesUntouched(t *testing.T) {
	t.Parallel()

	opts := MessagesThinkingOptions{AdaptiveThinking: true, ReasoningEfforts: []string{"low", "medium", "high"}}
	adaptive := []byte(`{"model":"m","thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[]}`)
	if got := PrepareMessagesThinking(adaptive, "", opts); string(got) != string(adaptive) {
		t.Fatalf("adaptive thinking was rewritten: %s", got)
	}
	disabled := []byte(`{"model":"m","thinking":{"type":"disabled"},"messages":[]}`)
	if got := PrepareMessagesThinking(disabled, "", opts); string(got) != string(disabled) {
		t.Fatalf("disabled thinking was rewritten: %s", got)
	}
	absent := []byte(`{"model":"m","messages":[]}`)
	if got := PrepareMessagesThinking(absent, "", opts); string(got) != string(absent) {
		t.Fatalf("thinking-free request was rewritten: %s", got)
	}
	legacy := []byte(`{"model":"m","thinking":{"type":"enabled","budget_tokens":4096},"messages":[]}`)
	if got := PrepareMessagesThinking(legacy, "", MessagesThinkingOptions{}); string(got) != string(legacy) {
		t.Fatalf("non-adaptive model was rewritten: %s", got)
	}
}
