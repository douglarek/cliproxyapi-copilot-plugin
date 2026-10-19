package provider

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/translate"
	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/transport"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestCopilotHeadersUseRecognizedIntegration(t *testing.T) {
	headers := copilotHeaders("test-token", false)

	expected := map[string]string{
		"Copilot-Integration-Id": "vscode-chat",
		"Editor-Plugin-Version":  "copilot-chat/0.35.0",
		"Editor-Version":         "vscode/1.107.0",
		"OpenAI-Intent":          "conversation-edits",
		"User-Agent":             "GitHubCopilotChat/0.35.0",
		"X-GitHub-Api-Version":   "2025-04-01",
	}

	for name, want := range expected {
		if got := headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestCopilotRequestHeadersForMessagesEndpoint(t *testing.T) {
	t.Parallel()

	thinkingBody := []byte(`{"model":"claude-opus-5","thinking":{"type":"enabled","budget_tokens":4096},"messages":[]}`)
	headers := copilotRequestHeaders("test-token", true, translate.EndpointMessages, thinkingBody)
	if got := headers.Get("anthropic-version"); got != anthropicVersionHeader {
		t.Errorf("anthropic-version = %q, want %q", got, anthropicVersionHeader)
	}
	if got := headers.Get("anthropic-beta"); got != anthropicBetaInterleavedThinking {
		t.Errorf("anthropic-beta = %q, want %q", got, anthropicBetaInterleavedThinking)
	}

	adaptiveBody := []byte(`{"model":"claude-opus-5","thinking":{"type":"adaptive"},"messages":[]}`)
	if got := copilotRequestHeaders("test-token", true, translate.EndpointMessages, adaptiveBody).Get("anthropic-beta"); got != "" {
		t.Errorf("adaptive thinking set anthropic-beta = %q, want empty", got)
	}

	plainBody := []byte(`{"model":"claude-opus-5","messages":[]}`)
	headers = copilotRequestHeaders("test-token", false, translate.EndpointMessages, plainBody)
	if got := headers.Get("anthropic-version"); got != anthropicVersionHeader {
		t.Errorf("anthropic-version = %q, want %q", got, anthropicVersionHeader)
	}
	if got := headers.Get("anthropic-beta"); got != "" {
		t.Errorf("thinking-free request set anthropic-beta = %q, want empty", got)
	}

	headers = copilotRequestHeaders("test-token", false, translate.EndpointResponses, []byte(`{"model":"gpt-5.6-sol"}`))
	if headers.Get("anthropic-version") != "" || headers.Get("anthropic-beta") != "" {
		t.Fatalf("responses endpoint must not carry anthropic headers: %#v", headers)
	}
}

func TestExecuteSendsAnthropicHeadersForMessagesEndpoint(t *testing.T) {
	t.Parallel()

	host := &mockExecutorHost{}
	svc := New(host)
	fp := tokenFingerprint("gho_test")
	svc.tokenEntries["auth-1"] = copilotTokenEntry{
		Token:       "copilot-test-token",
		APIBaseURL:  "https://api.githubcopilot.com",
		ExpiresAt:   time.Now().Add(time.Hour),
		Fingerprint: fp,
	}
	svc.modelEntries["auth-1"] = modelCacheEntry{
		Fingerprint: fp,
		APIBaseURL:  "https://api.githubcopilot.com",
		ExpiresAt:   time.Now().Add(time.Hour),
		Models: []upstreamModel{
			{ID: "claude-opus-5", Vendor: "Anthropic", SupportedEndpoints: []string{"/v1/messages"}},
		},
	}

	_, err := svc.Execute(t.Context(), ExecuteRequest{
		HostCallbackID: "callback-1",
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "auth-1",
			Model:        "claude-opus-5",
			SourceFormat: "claude",
			Payload:      []byte(`{"model":"claude-opus-5","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":4096},"messages":[{"role":"user","content":"hello"}]}`),
			StorageJSON:  []byte(`{"type":"copilot","github_access_token":"gho_test"}`),
		},
	})
	if err != nil {
		t.Fatalf("execute claude-opus-5: %v", err)
	}
	if host.request.URL != "https://api.githubcopilot.com/v1/messages" {
		t.Fatalf("request URL = %q, want the native messages endpoint", host.request.URL)
	}
	if got := host.request.Headers.Get("anthropic-version"); got != anthropicVersionHeader {
		t.Errorf("anthropic-version = %q, want %q", got, anthropicVersionHeader)
	}
	if got := host.request.Headers.Get("anthropic-beta"); got != anthropicBetaInterleavedThinking {
		t.Errorf("anthropic-beta = %q, want %q", got, anthropicBetaInterleavedThinking)
	}
}

func TestExecuteAdaptsThinkingForMessagesEndpoint(t *testing.T) {
	t.Parallel()

	host := &mockExecutorHost{}
	svc := New(host)
	fp := tokenFingerprint("gho_test")
	svc.tokenEntries["auth-1"] = copilotTokenEntry{
		Token:       "copilot-test-token",
		APIBaseURL:  "https://api.githubcopilot.com",
		ExpiresAt:   time.Now().Add(time.Hour),
		Fingerprint: fp,
	}
	svc.modelEntries["auth-1"] = modelCacheEntry{
		Fingerprint: fp,
		APIBaseURL:  "https://api.githubcopilot.com",
		ExpiresAt:   time.Now().Add(time.Hour),
		Models: []upstreamModel{
			{
				ID:                 "claude-opus-5.5",
				Vendor:             "Anthropic",
				SupportedEndpoints: []string{"/v1/messages"},
				Capabilities: modelCapabilities{
					Supports: modelSupports{AdaptiveThinking: true, ReasoningEffort: []string{"low", "medium", "high", "xhigh", "max"}},
				},
			},
		},
	}

	_, err := svc.Execute(t.Context(), ExecuteRequest{
		HostCallbackID: "callback-1",
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "auth-1",
			Model:        "claude-opus-5.5",
			SourceFormat: "claude",
			Payload:      []byte(`{"model":"claude-opus-5.5","max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":4096},"messages":[{"role":"user","content":"hello"}]}`),
			StorageJSON:  []byte(`{"type":"copilot","github_access_token":"gho_test"}`),
		},
	})
	if err != nil {
		t.Fatalf("execute claude-opus-5.5: %v", err)
	}
	if got := gjson.GetBytes(host.request.Body, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive: %s", got, host.request.Body)
	}
	if gjson.GetBytes(host.request.Body, "thinking.budget_tokens").Exists() {
		t.Fatalf("budget_tokens survived the adaptive rewrite: %s", host.request.Body)
	}
	if got := gjson.GetBytes(host.request.Body, "output_config.effort").String(); got != "medium" {
		t.Fatalf("output_config.effort = %q, want medium (4096 budget): %s", got, host.request.Body)
	}
	if got := host.request.Headers.Get("anthropic-beta"); got != "" {
		t.Errorf("anthropic-beta = %q, want empty for adaptive thinking", got)
	}
}

func TestExecuteMapsReasoningTextForChatEndpoint(t *testing.T) {
	t.Parallel()

	host := &mockExecutorHost{
		responseBody: []byte(`{"choices":[{"message":{"role":"assistant","content":"3599 = 59 x 61","reasoning_text":"3599 factors as 59 times 61."}}],"usage":{"completion_tokens":10,"prompt_tokens":5,"total_tokens":15}}`),
	}
	svc := New(host)
	fp := tokenFingerprint("gho_test")
	svc.tokenEntries["auth-1"] = copilotTokenEntry{
		Token:       "copilot-test-token",
		APIBaseURL:  "https://api.githubcopilot.com",
		ExpiresAt:   time.Now().Add(time.Hour),
		Fingerprint: fp,
	}
	svc.modelEntries["auth-1"] = modelCacheEntry{
		Fingerprint: fp,
		APIBaseURL:  "https://api.githubcopilot.com",
		ExpiresAt:   time.Now().Add(time.Hour),
		Models: []upstreamModel{
			{ID: "claude-sonnet-5", Vendor: "Anthropic", SupportedEndpoints: []string{"/v1/messages", "/chat/completions"}},
		},
	}

	resp, err := svc.Execute(t.Context(), ExecuteRequest{
		HostCallbackID: "callback-1",
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "auth-1",
			Model:        "claude-sonnet-5",
			SourceFormat: "claude",
			Payload:      []byte(`{"model":"claude-sonnet-5","max_tokens":512,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"Factor 3599."}]}`),
			StorageJSON:  []byte(`{"type":"copilot","github_access_token":"gho_test"}`),
		},
	})
	if err != nil {
		t.Fatalf("execute claude-sonnet-5: %v", err)
	}
	if host.request.URL != "https://api.githubcopilot.com/chat/completions" {
		t.Fatalf("request URL = %q, want the chat completions endpoint", host.request.URL)
	}
	if got := gjson.GetBytes(host.request.Body, "reasoning_effort").String(); got != "medium" {
		t.Errorf("reasoning_effort = %q, want medium (2048 budget): %s", got, host.request.Body)
	}
	blocks := gjson.GetBytes(resp.Payload, "content").Array()
	if len(blocks) == 0 || blocks[0].Get("type").String() != "thinking" {
		t.Fatalf("expected a leading thinking block, got: %s", resp.Payload)
	}
	if got := blocks[0].Get("thinking").String(); got != "3599 factors as 59 times 61." {
		t.Fatalf("thinking text = %q, want the mapped reasoning_text", got)
	}
	if got := blocks[1].Get("text").String(); got != "3599 = 59 x 61" {
		t.Fatalf("text block = %q, unexpected response translation", got)
	}
}

func TestCountTokensReturnsClaudeInputTokens(t *testing.T) {
	t.Parallel()

	resp, err := (&Service{}).CountTokens(ExecuteRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			SourceFormat:    "claude",
			OriginalRequest: []byte(`{"model":"gpt-5.6-sol","system":"Be concise.","messages":[{"role":"user","content":"hello world"}]}`),
		},
	})
	if err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got <= 0 {
		t.Fatalf("input_tokens = %d; response=%s", got, resp.Payload)
	}
}

type mockExecutorHost struct {
	request      transport.Request
	responseBody []byte
}

func (h *mockExecutorHost) Do(_ context.Context, _ string, req transport.Request) (transport.Response, error) {
	h.request = req
	body := h.responseBody
	if body == nil {
		body = []byte(`{
			"id":"resp-1",
			"status":"completed",
			"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]
		}`)
	}
	return transport.Response{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}, nil
}

func (h *mockExecutorHost) OpenStream(context.Context, string, transport.Request) (transport.Stream, error) {
	return transport.Stream{}, nil
}

func (h *mockExecutorHost) ReadStream(context.Context, string) (transport.StreamChunk, error) {
	return transport.StreamChunk{}, nil
}

func (h *mockExecutorHost) CloseStream(context.Context, string) error {
	return nil
}

func (h *mockExecutorHost) Emit(context.Context, string, []byte) error {
	return nil
}

func (h *mockExecutorHost) CloseOutput(context.Context, string, string) {}

func TestExecuteStripsTemperatureForGPTModel(t *testing.T) {
	t.Parallel()

	host := &mockExecutorHost{}
	svc := New(host)
	fp := tokenFingerprint("gho_test")
	svc.tokenEntries["auth-1"] = copilotTokenEntry{
		Token:       "copilot-test-token",
		APIBaseURL:  "https://api.githubcopilot.com",
		ExpiresAt:   time.Now().Add(time.Hour),
		Fingerprint: fp,
	}
	svc.modelEntries["auth-1"] = modelCacheEntry{
		Fingerprint: fp,
		APIBaseURL:  "https://api.githubcopilot.com",
		ExpiresAt:   time.Now().Add(time.Hour),
		Models: []upstreamModel{
			{ID: "gpt-6-astra", SupportedEndpoints: []string{"/responses"}},
			{ID: "claude-3-7-sonnet", SupportedEndpoints: []string{"/responses"}},
		},
	}

	_, err := svc.Execute(t.Context(), ExecuteRequest{
		HostCallbackID: "callback-1",
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "auth-1",
			Model:        "gpt-6-astra",
			SourceFormat: "claude",
			Payload:      []byte(`{"model":"ignored","temperature":0.7,"messages":[{"role":"user","content":"hello"}]}`),
			StorageJSON:  []byte(`{"type":"copilot","github_access_token":"gho_test"}`),
		},
	})
	if err != nil {
		t.Fatalf("execute gpt-6-astra: %v", err)
	}
	if gjson.GetBytes(host.request.Body, "temperature").Exists() {
		t.Fatalf("expected temperature to be stripped for gpt-6-astra: %s", host.request.Body)
	}

	_, errClaude := svc.Execute(t.Context(), ExecuteRequest{
		HostCallbackID: "callback-1",
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "auth-1",
			Model:        "claude-3-7-sonnet",
			SourceFormat: "claude",
			Payload:      []byte(`{"model":"ignored","temperature":0.7,"messages":[{"role":"user","content":"hello"}]}`),
			StorageJSON:  []byte(`{"type":"copilot","github_access_token":"gho_test"}`),
		},
	})
	if errClaude != nil {
		t.Fatalf("execute claude-3-7-sonnet: %v", errClaude)
	}
	if got := gjson.GetBytes(host.request.Body, "temperature").Float(); got != 0.7 {
		t.Fatalf("expected temperature to be preserved for claude-3-7-sonnet, got %v: %s", got, host.request.Body)
	}
}
