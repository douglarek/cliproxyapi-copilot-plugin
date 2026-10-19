package provider

import (
	"context"
	"net/http"
	"testing"
	"time"

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
	request transport.Request
}

func (h *mockExecutorHost) Do(_ context.Context, _ string, req transport.Request) (transport.Response, error) {
	h.request = req
	return transport.Response{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body: []byte(`{
			"id":"resp-1",
			"status":"completed",
			"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]
		}`),
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
