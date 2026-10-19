package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

const (
	EndpointResponses       = "/responses"
	EndpointChatCompletions = "/chat/completions"
	EndpointMessages        = "/v1/messages"
)

var registry = builtin.Registry()

func ClaudeToResponses(model string, body []byte, stream bool) ([]byte, error) {
	return request(sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse, model, body, stream)
}

func ResponsesToClaude(ctx context.Context, model string, original, translated, body []byte) ([]byte, error) {
	return response(ctx, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, model, original, translated, body)
}

func RequestForEndpoint(model string, body []byte, stream bool, endpoint string) ([]byte, error) {
	return RequestForEndpointFrom(sdktranslator.FormatOpenAIResponse.String(), model, body, stream, endpoint)
}

func RequestForEndpointFrom(source, model string, body []byte, stream bool, endpoint string) ([]byte, error) {
	from := sdktranslator.FromString(source)
	to, err := endpointFormat(endpoint)
	if err != nil {
		return nil, err
	}
	if from == sdktranslator.FormatOpenAIResponse && to != sdktranslator.FormatOpenAIResponse {
		body = NormalizeResponsesStringInput(body)
	}
	var out []byte
	if from == to {
		out = append([]byte(nil), body...)
	} else {
		out, err = request(from, to, model, body, stream)
	}
	if err != nil {
		return nil, err
	}
	return setModelAndStream(out, model, stream)
}

func ResponseToResponses(ctx context.Context, endpoint, model string, original, translated, body []byte) ([]byte, error) {
	return ResponseFromEndpoint(ctx, endpoint, sdktranslator.FormatOpenAIResponse.String(), model, original, translated, body)
}

func ResponseFromEndpoint(ctx context.Context, endpoint, destination, model string, original, translated, body []byte) ([]byte, error) {
	from, err := endpointFormat(endpoint)
	if err != nil {
		return nil, err
	}
	if from == sdktranslator.FormatOpenAI {
		body, err = NormalizeChatCompletionsResponse(body)
		if err != nil {
			return nil, fmt.Errorf("normalize Chat Completions response: %w", err)
		}
	}
	to := sdktranslator.FromString(destination)
	if from == to {
		if !json.Valid(body) {
			return nil, fmt.Errorf("Copilot API returned invalid JSON")
		}
		return append([]byte(nil), body...), nil
	}
	return response(ctx, from, to, model, original, translated, body)
}

func StreamToResponses(ctx context.Context, endpoint, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	return StreamFromEndpoint(ctx, endpoint, sdktranslator.FormatOpenAIResponse.String(), model, original, translated, frame, state)
}

func StreamFromEndpoint(ctx context.Context, endpoint, destination, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	from, err := endpointFormat(endpoint)
	if err != nil {
		return nil, err
	}
	if from == sdktranslator.FormatOpenAI {
		frame = NormalizeChatCompletionsFrame(frame)
	}
	to := sdktranslator.FromString(destination)
	if from == to {
		if from == sdktranslator.FormatOpenAI {
			// Chat Completions clients receive chunks verbatim from the host
			// handler, which adds its own `data: ...` framing; hand over bare
			// JSON payloads and drop the upstream terminator.
			return chatCompletionChunk(frame)
		}
		return [][]byte{append([]byte(nil), frame...)}, nil
	}
	return stream(ctx, from, to, model, original, translated, frame, state)
}

// chatCompletionChunk converts one Chat Completions SSE frame into the bare
// JSON payload the host expects as an executor chunk. The `data: [DONE]`
// terminator is dropped; the host emits its own when the stream closes.
func chatCompletionChunk(frame []byte) ([][]byte, error) {
	_, data, done, err := parseSSEFrame(frame)
	if err != nil {
		return nil, err
	}
	if done || len(data) == 0 {
		return nil, nil
	}
	return [][]byte{append([]byte(nil), data...)}, nil
}

func ResponsesSSEToClaude(ctx context.Context, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	return stream(ctx, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, model, original, translated, frame, state)
}

func request(from, to sdktranslator.Format, model string, body []byte, stream bool) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("request body is not valid JSON")
	}
	if from == sdktranslator.FormatClaude && to == sdktranslator.FormatOpenAIResponse {
		return claudeRequestToResponses(model, body, stream)
	}
	if from == sdktranslator.FormatOpenAI && to == sdktranslator.FormatOpenAIResponse {
		// Chat Completions client against a /responses-only model. The
		// registry has no direct chat↔responses pair, so route through the
		// Claude schema: builtin Chat→Claude request translation, then the
		// plugin's Claude→Responses converter.
		claudeBody := registry.TranslateRequest(sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, model, body, stream)
		return claudeRequestToResponses(model, claudeBody, stream)
	}
	if from != to && !registry.HasRequestTransformer(from, to) {
		intermediate, ok := intermediateFormat(from, to)
		if !ok {
			return nil, fmt.Errorf("official translator has no request route from %s to %s", from, to)
		}
		first := registry.TranslateRequest(from, intermediate, model, body, stream)
		out := registry.TranslateRequest(intermediate, to, model, first, stream)
		if len(out) == 0 || !json.Valid(out) {
			return nil, fmt.Errorf("official two-hop request translation from %s to %s failed", from, to)
		}
		return out, nil
	}
	out := registry.TranslateRequest(from, to, model, body, stream)
	if len(out) == 0 || !json.Valid(out) {
		return nil, fmt.Errorf("official request translation from %s to %s failed", from, to)
	}
	return out, nil
}

func response(ctx context.Context, from, to sdktranslator.Format, model string, original, translated, body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("response body is not valid JSON")
	}
	if from == sdktranslator.FormatOpenAIResponse && to == sdktranslator.FormatClaude {
		return responsesResponseToClaude(model, body)
	}
	if from == sdktranslator.FormatOpenAIResponse && to == sdktranslator.FormatOpenAI {
		// Non-stream counterpart of the /responses-only model route: convert
		// to a Claude message first, then map it onto a Chat Completions
		// response (the builtin translator expects recorded event lines).
		claudeBody, errClaude := responsesResponseToClaude(model, body)
		if errClaude != nil {
			return nil, errClaude
		}
		return claudeMessageToChatCompletion(model, claudeBody)
	}
	if from != to && !registry.HasNonStreamResponseTransformer(to, from) {
		intermediate, ok := intermediateFormat(to, from)
		if !ok {
			return nil, fmt.Errorf("official translator has no response route from %s to %s", from, to)
		}
		intermediateRequest := registry.TranslateRequest(to, intermediate, model, original, false)
		first := registry.TranslateNonStream(ctx, from, intermediate, model, intermediateRequest, translated, body, nil)
		out := registry.TranslateNonStream(ctx, intermediate, to, model, original, intermediateRequest, first, nil)
		if len(out) == 0 || !json.Valid(out) {
			return nil, fmt.Errorf("official two-hop response translation from %s to %s failed", from, to)
		}
		return out, nil
	}
	out := registry.TranslateNonStream(ctx, from, to, model, original, translated, body, nil)
	if len(out) == 0 || !json.Valid(out) {
		return nil, fmt.Errorf("official response translation from %s to %s failed", from, to)
	}
	return out, nil
}

func stream(ctx context.Context, from, to sdktranslator.Format, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	if from == sdktranslator.FormatOpenAIResponse && to == sdktranslator.FormatClaude {
		return responsesStreamToClaude(model, frame, state)
	}
	if from == sdktranslator.FormatOpenAIResponse && to == sdktranslator.FormatOpenAI {
		// Stream counterpart of the /responses-only model route: Responses
		// SSE → Claude events (plugin converter) → Chat Completions chunks
		// (builtin translator, which requires bare `data:` lines).
		return responsesStreamToOpenAI(ctx, model, original, translated, frame, state)
	}
	if from == sdktranslator.FormatClaude && to == sdktranslator.FormatOpenAI {
		// The builtin Claude→Chat stream translator only accepts bare
		// `data:` lines; strip event framing from full SSE frames first.
		data, ok := claudeFrameDataLine(frame)
		if !ok {
			return nil, nil
		}
		return registry.TranslateStream(ctx, from, to, model, original, translated, data, state), nil
	}
	if from != to && !registry.HasStreamResponseTransformer(to, from) {
		intermediate, ok := intermediateFormat(to, from)
		if !ok {
			return nil, fmt.Errorf("official translator has no stream route from %s to %s", from, to)
		}
		if state == nil {
			return nil, fmt.Errorf("two-hop stream translation requires state")
		}
		hopState, okState := (*state).(*twoHopStreamState)
		if !okState {
			hopState = &twoHopStreamState{}
			*state = hopState
		}
		intermediateRequest := registry.TranslateRequest(to, intermediate, model, original, true)
		first := registry.TranslateStream(ctx, from, intermediate, model, intermediateRequest, translated, frame, &hopState.First)
		var out [][]byte
		for _, firstFrame := range first {
			out = append(out, registry.TranslateStream(ctx, intermediate, to, model, original, intermediateRequest, firstFrame, &hopState.Second)...)
		}
		return out, nil
	}
	out := registry.TranslateStream(ctx, from, to, model, original, translated, frame, state)
	if len(out) == 1 && bytes.Equal(out[0], frame) && from != to {
		return nil, fmt.Errorf("official stream translation from %s to %s fell back unchanged", from, to)
	}
	return out, nil
}

type twoHopStreamState struct {
	First  any
	Second any
}

func intermediateFormat(from, to sdktranslator.Format) (sdktranslator.Format, bool) {
	for _, candidate := range []sdktranslator.Format{
		sdktranslator.FormatOpenAI,
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatClaude,
	} {
		if candidate != from && candidate != to &&
			registry.HasRequestTransformer(from, candidate) &&
			registry.HasRequestTransformer(candidate, to) {
			return candidate, true
		}
	}
	return "", false
}

func endpointFormat(endpoint string) (sdktranslator.Format, error) {
	switch endpoint {
	case EndpointResponses:
		return sdktranslator.FormatOpenAIResponse, nil
	case EndpointChatCompletions:
		return sdktranslator.FormatOpenAI, nil
	case EndpointMessages:
		return sdktranslator.FormatClaude, nil
	default:
		return "", fmt.Errorf("unsupported Copilot endpoint %q", endpoint)
	}
}

// IsGPTModel reports whether model represents an OpenAI GPT-family model.
func IsGPTModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	if strings.HasPrefix(name, "gpt") || strings.HasPrefix(name, "chatgpt") {
		return true
	}
	for _, part := range strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	}) {
		if part == "gpt" || strings.HasPrefix(part, "gpt") {
			return true
		}
	}
	return false
}

func setModelAndStream(body []byte, model string, streamEnabled bool) ([]byte, error) {
	var value map[string]any
	if errUnmarshal := json.Unmarshal(body, &value); errUnmarshal != nil {
		return nil, fmt.Errorf("decode translated request: %w", errUnmarshal)
	}
	targetModel := strings.TrimSpace(model)
	if targetModel == "" {
		if m, ok := value["model"].(string); ok {
			targetModel = strings.TrimSpace(m)
		}
	}
	if targetModel != "" {
		value["model"] = targetModel
	}
	value["stream"] = streamEnabled
	if IsGPTModel(targetModel) {
		delete(value, "temperature")
	}
	out, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode translated request: %w", errMarshal)
	}
	return out, nil
}
