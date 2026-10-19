package provider

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/redact"
	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/sse"
	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/translate"
	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/transport"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const (
	copilotUserAgent     = "GitHubCopilotChat/0.35.0"
	copilotEditorVersion = "vscode/1.107.0"
	copilotPluginVersion = "copilot-chat/0.35.0"
	copilotIntegrationID = "vscode-chat"
	copilotAPIVersion    = "2025-04-01"

	anthropicVersionHeader           = "2023-06-01"
	anthropicBetaInterleavedThinking = "interleaved-thinking-2025-05-14"
)

type ExecuteRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type HTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (s *Service) Execute(ctx context.Context, req ExecuteRequest) (pluginapi.ExecutorResponse, error) {
	sourceFormat := normalizeRequestFormat(firstNonEmpty(req.SourceFormat, req.Format))
	if sourceFormat == "" {
		return pluginapi.ExecutorResponse{}, statusError("unsupported_format", fmt.Sprintf("unsupported request format %q", firstNonEmpty(req.SourceFormat, req.Format)), http.StatusUnprocessableEntity)
	}
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return pluginapi.ExecutorResponse{}, errParse
	}
	endpoint, model, token, errEndpoint := s.endpointForModel(ctx, req.HostCallbackID, req.AuthID, storage, req.Model, sourceFormat)
	if errEndpoint != nil {
		return pluginapi.ExecutorResponse{}, errEndpoint
	}
	payload := req.Payload
	if len(bytes.TrimSpace(payload)) == 0 {
		payload = req.OriginalRequest
	}
	requestBody, errTranslate := translate.RequestForEndpointFrom(sourceFormat, req.Model, payload, false, endpoint)
	if errTranslate != nil {
		return pluginapi.ExecutorResponse{}, statusError("translation_error", errTranslate.Error(), http.StatusUnprocessableEntity)
	}
	requestBody = prepareMessagesThinking(requestBody, requestedEffort(sourceFormat, payload), model)
	resp, token, errDo := s.doModelRequest(ctx, req.HostCallbackID, req.AuthID, storage, token, endpoint, requestBody, false)
	if errDo != nil {
		return pluginapi.ExecutorResponse{}, errDo
	}
	body, errResponse := translate.ResponseFromEndpoint(ctx, endpoint, sourceFormat, req.Model, req.OriginalRequest, requestBody, resp.Body)
	if errResponse != nil {
		return pluginapi.ExecutorResponse{}, statusError("translation_error", errResponse.Error(), http.StatusBadGateway)
	}
	return pluginapi.ExecutorResponse{
		Payload: body,
		Headers: filterResponseHeaders(resp.Headers),
		Metadata: map[string]any{
			"copilot_endpoint": endpoint,
			"token_expires_at": token.ExpiresAt.UTC().Format(http.TimeFormat),
		},
	}, nil
}

func (s *Service) ExecuteStream(ctx context.Context, req ExecuteRequest) (http.Header, error) {
	if strings.TrimSpace(req.StreamID) == "" {
		return nil, statusError("invalid_request", "stream_id is required", http.StatusBadRequest)
	}
	sourceFormat := normalizeRequestFormat(firstNonEmpty(req.SourceFormat, req.Format))
	if sourceFormat == "" {
		return nil, statusError("unsupported_format", fmt.Sprintf("unsupported request format %q", firstNonEmpty(req.SourceFormat, req.Format)), http.StatusUnprocessableEntity)
	}
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return nil, errParse
	}
	endpoint, model, token, errEndpoint := s.endpointForModel(ctx, req.HostCallbackID, req.AuthID, storage, req.Model, sourceFormat)
	if errEndpoint != nil {
		return nil, errEndpoint
	}
	payload := req.Payload
	if len(bytes.TrimSpace(payload)) == 0 {
		payload = req.OriginalRequest
	}
	requestBody, errTranslate := translate.RequestForEndpointFrom(sourceFormat, req.Model, payload, true, endpoint)
	if errTranslate != nil {
		return nil, statusError("translation_error", errTranslate.Error(), http.StatusUnprocessableEntity)
	}
	requestBody = prepareMessagesThinking(requestBody, requestedEffort(sourceFormat, payload), model)
	upstream, token, errOpen := s.openModelStream(ctx, req.HostCallbackID, req.AuthID, storage, token, endpoint, requestBody)
	if errOpen != nil {
		return nil, errOpen
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		body, errCollect := s.collectStreamError(ctx, upstream)
		if errCollect != nil {
			return nil, errCollect
		}
		return nil, upstreamStatusError(upstream.StatusCode, redact.ErrorBody(body, token.Token, storage.GitHubAccessToken))
	}
	go s.pumpStream(req.StreamID, endpoint, sourceFormat, req.Model, req.OriginalRequest, requestBody, upstream)
	headers := filterResponseHeaders(upstream.Headers)
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	return headers, nil
}

func (s *Service) doModelRequest(ctx context.Context, callbackID, authID string, storage authStorage, token copilotTokenEntry, endpoint string, body []byte, stream bool) (transport.Response, copilotTokenEntry, error) {
	request := transport.Request{
		Method:  http.MethodPost,
		URL:     token.APIBaseURL + endpoint,
		Headers: copilotRequestHeaders(token.Token, stream, endpoint, body),
		Body:    body,
	}
	resp, errDo := s.host.Do(ctx, callbackID, request)
	if errDo != nil {
		return transport.Response{}, token, fmt.Errorf("call Copilot model endpoint: %w", errDo)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		s.invalidateAuth(authID)
		refreshed, errToken := s.copilotToken(ctx, callbackID, authID, storage)
		if errToken != nil {
			return transport.Response{}, token, errToken
		}
		token = refreshed
		request.URL = token.APIBaseURL + endpoint
		request.Headers = copilotRequestHeaders(token.Token, stream, endpoint, body)
		resp, errDo = s.host.Do(ctx, callbackID, request)
		if errDo != nil {
			return transport.Response{}, token, fmt.Errorf("call Copilot model endpoint after token refresh: %w", errDo)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, token, upstreamStatusError(resp.StatusCode, redact.ErrorBody(resp.Body, token.Token, storage.GitHubAccessToken))
	}
	return resp, token, nil
}

func (s *Service) openModelStream(ctx context.Context, callbackID, authID string, storage authStorage, token copilotTokenEntry, endpoint string, body []byte) (transport.Stream, copilotTokenEntry, error) {
	request := transport.Request{
		Method:  http.MethodPost,
		URL:     token.APIBaseURL + endpoint,
		Headers: copilotRequestHeaders(token.Token, true, endpoint, body),
		Body:    body,
	}
	stream, errOpen := s.host.OpenStream(ctx, callbackID, request)
	if errOpen != nil {
		return transport.Stream{}, token, fmt.Errorf("open Copilot model stream: %w", errOpen)
	}
	if stream.StatusCode == http.StatusUnauthorized {
		_ = s.host.CloseStream(ctx, stream.ID)
		s.invalidateAuth(authID)
		refreshed, errToken := s.copilotToken(ctx, callbackID, authID, storage)
		if errToken != nil {
			return transport.Stream{}, token, errToken
		}
		token = refreshed
		request.URL = token.APIBaseURL + endpoint
		request.Headers = copilotRequestHeaders(token.Token, true, endpoint, body)
		stream, errOpen = s.host.OpenStream(ctx, callbackID, request)
		if errOpen != nil {
			return transport.Stream{}, token, fmt.Errorf("open Copilot model stream after token refresh: %w", errOpen)
		}
	}
	return stream, token, nil
}

func (s *Service) collectStreamError(ctx context.Context, stream transport.Stream) ([]byte, error) {
	defer func() { _ = s.host.CloseStream(context.Background(), stream.ID) }()
	var body []byte
	for {
		chunk, errRead := s.host.ReadStream(ctx, stream.ID)
		if errRead != nil {
			return nil, fmt.Errorf("read Copilot error stream: %w", errRead)
		}
		body = append(body, chunk.Payload...)
		if chunk.Error != "" {
			return nil, fmt.Errorf("read Copilot error stream: %s", redact.Text(chunk.Error))
		}
		if chunk.Done {
			return body, nil
		}
	}
}

func (s *Service) pumpStream(outputID, endpoint, destination, model string, original, translated []byte, upstream transport.Stream) {
	ctx := context.Background()
	var terminalErr error
	defer func() {
		_ = s.host.CloseStream(ctx, upstream.ID)
		message := ""
		if terminalErr != nil {
			message = redact.Text(terminalErr.Error())
		}
		s.host.CloseOutput(ctx, outputID, message)
	}()

	decoder := &sse.Decoder{}
	var state any
	emit := func(frame []byte) error {
		frames, errTranslate := translate.StreamFromEndpoint(ctx, endpoint, destination, model, original, translated, frame, &state)
		if errTranslate != nil {
			return errTranslate
		}
		for _, output := range frames {
			if len(output) == 0 {
				continue
			}
			if errEmit := s.host.Emit(ctx, outputID, output); errEmit != nil {
				return errEmit
			}
		}
		return nil
	}

	for {
		chunk, errRead := s.host.ReadStream(ctx, upstream.ID)
		if errRead != nil {
			terminalErr = fmt.Errorf("read Copilot stream: %w", errRead)
			return
		}
		if chunk.Error != "" {
			terminalErr = fmt.Errorf("Copilot stream error: %s", redact.Text(chunk.Error))
			return
		}
		for _, frame := range decoder.Feed(chunk.Payload) {
			if errEmit := emit(frame); errEmit != nil {
				terminalErr = fmt.Errorf("translate Copilot stream: %w", errEmit)
				return
			}
		}
		if chunk.Done {
			if trailing := decoder.Flush(); len(trailing) > 0 {
				if errEmit := emit(trailing); errEmit != nil {
					terminalErr = fmt.Errorf("translate final Copilot stream frame: %w", errEmit)
				}
			}
			return
		}
	}
}

func normalizeRequestFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "responses", "openai-response", "openai-responses":
		return "openai-response"
	case "claude", "anthropic":
		return "claude"
	case "openai", "chat", "chat-completions", "chat_completions":
		return "openai"
	default:
		return ""
	}
}

func (s *Service) HTTP(ctx context.Context, req HTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return pluginapi.ExecutorHTTPResponse{}, errParse
	}
	token, errToken := s.copilotToken(ctx, req.HostCallbackID, req.AuthID, storage)
	if errToken != nil {
		return pluginapi.ExecutorHTTPResponse{}, errToken
	}
	target, errURL := url.Parse(strings.TrimSpace(req.URL))
	if errURL != nil || target.Hostname() == "" {
		return pluginapi.ExecutorHTTPResponse{}, statusError("invalid_request", "executor HTTP URL is invalid", http.StatusBadRequest)
	}
	base, _ := url.Parse(token.APIBaseURL)
	if !strings.EqualFold(target.Scheme, base.Scheme) || !strings.EqualFold(target.Host, base.Host) {
		return pluginapi.ExecutorHTTPResponse{}, statusError("forbidden_url", "executor HTTP URL is outside the authenticated Copilot API origin", http.StatusForbidden)
	}
	headers := cloneHeader(req.Headers)
	headers.Set("Authorization", "Bearer "+token.Token)
	headers.Set("User-Agent", copilotUserAgent)
	headers.Set("X-GitHub-Api-Version", copilotAPIVersion)
	resp, errDo := s.host.Do(ctx, req.HostCallbackID, transport.Request{
		Method:  req.Method,
		URL:     target.String(),
		Headers: headers,
		Body:    append([]byte(nil), req.Body...),
	})
	if errDo != nil {
		return pluginapi.ExecutorHTTPResponse{}, errDo
	}
	return pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    filterResponseHeaders(resp.Headers),
		Body:       resp.Body,
	}, nil
}

func copilotHeaders(token string, stream bool) http.Header {
	requestID, _ := randomIdentifier(16)
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	headers := http.Header{}
	headers.Set("Accept", accept)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Content-Type", "application/json")
	headers.Set("Copilot-Integration-Id", copilotIntegrationID)
	headers.Set("Editor-Plugin-Version", copilotPluginVersion)
	headers.Set("Editor-Version", copilotEditorVersion)
	headers.Set("OpenAI-Intent", "conversation-edits")
	headers.Set("User-Agent", copilotUserAgent)
	headers.Set("X-Agent-Task-Id", requestID)
	headers.Set("X-GitHub-Api-Version", copilotAPIVersion)
	headers.Set("X-Initiator", "user")
	headers.Set("X-Interaction-Type", "conversation-edits")
	headers.Set("X-Request-Id", requestID)
	return headers
}

// copilotRequestHeaders builds the headers for a model inference call. The
// Anthropic Messages endpoint additionally requires an API version header and
// the interleaved thinking beta whenever the request enables budget thinking.
func copilotRequestHeaders(token string, stream bool, endpoint string, body []byte) http.Header {
	headers := copilotHeaders(token, stream)
	if endpoint != translate.EndpointMessages {
		return headers
	}
	headers.Set("anthropic-version", anthropicVersionHeader)
	thinking := gjson.GetBytes(body, "thinking")
	if thinking.Get("budget_tokens").Int() > 0 && thinking.Get("type").String() != "adaptive" {
		headers.Set("anthropic-beta", anthropicBetaInterleavedThinking)
	}
	return headers
}

// prepareMessagesThinking adapts the translated request for Copilot's
// Anthropic Messages endpoint, whose newer Claude models reject budget-style
// thinking and require thinking.type=adaptive with output_config.effort.
func prepareMessagesThinking(body []byte, effort string, model upstreamModel) []byte {
	return translate.PrepareMessagesThinking(body, effort, translate.MessagesThinkingOptions{
		AdaptiveThinking: model.Capabilities.Supports.AdaptiveThinking,
		ReasoningEfforts: model.Capabilities.Supports.ReasoningEffort,
	})
}

// requestedEffort extracts the thinking level the client asked for, in its
// native protocol's vocabulary, so the adaptive rewrite can preserve it
// instead of round-tripping through budget token estimates.
func requestedEffort(sourceFormat string, payload []byte) string {
	root := gjson.ParseBytes(payload)
	switch sourceFormat {
	case "claude":
		if effort := root.Get("output_config.effort"); effort.Exists() {
			return effort.String()
		}
		if root.Get("thinking.type").String() != "enabled" {
			return ""
		}
		switch budget := root.Get("thinking.budget_tokens").Int(); {
		case budget <= 0:
			return ""
		case budget <= 2048:
			return "low"
		case budget <= 8192:
			return "medium"
		default:
			return "high"
		}
	case "openai-response":
		return root.Get("reasoning.effort").String()
	default:
		return root.Get("reasoning_effort").String()
	}
}

func filterResponseHeaders(headers http.Header) http.Header {
	out := http.Header{}
	for key, values := range headers {
		switch strings.ToLower(key) {
		case "content-type", "cache-control", "retry-after", "x-github-request-id", "x-request-id":
			out[key] = append([]string(nil), values...)
		}
	}
	return out
}

func cloneHeader(headers http.Header) http.Header {
	if len(headers) == 0 {
		return http.Header{}
	}
	return headers.Clone()
}
