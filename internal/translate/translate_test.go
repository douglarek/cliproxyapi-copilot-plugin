package translate

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeRequestToResponsesPreservesCoreContent(t *testing.T) {
	t.Parallel()

	claudeRequest := []byte(`{
		"model":"ignored",
		"max_tokens":321,
		"system":"Be concise.",
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"describe this"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}
			]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"inspect first","signature":"sig"},
				{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"found"}]}
		],
		"tools":[{"name":"lookup","description":"Look up data","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]
	}`)

	out, err := RequestForEndpointFrom("claude", "gpt-5.6-sol", claudeRequest, false, EndpointResponses)
	if err != nil {
		t.Fatalf("translate Claude request: %v", err)
	}
	data := gjson.ParseBytes(out)
	if got := data.Get("model").String(); got != "gpt-5.6-sol" {
		t.Fatalf("model = %q", got)
	}
	if got := data.Get("max_output_tokens").Int(); got != 321 {
		t.Fatalf("max_output_tokens = %d", got)
	}
	text := string(out)
	for _, needle := range []string{
		"Be concise.",
		"describe this",
		"data:image/png;base64,YWJj",
		"inspect first",
		`"type":"function_call"`,
		`"call_id":"call_1"`,
		`"type":"function_call_output"`,
		`"name":"lookup"`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("translated request omits %q: %s", needle, out)
		}
	}
}

func TestClaudeStructuredOutputAddsResponsesSchemaName(t *testing.T) {
	t.Parallel()

	original := []byte(`{
		"model":"gpt-5.6-terra",
		"messages":[{"role":"user","content":"Create a title."}],
		"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}
	}`)
	out, err := RequestForEndpointFrom("claude", "gpt-5.6-terra", original, true, EndpointResponses)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	if got := gjson.GetBytes(out, "text.format.name").String(); got != "claude_structured_output" {
		t.Fatalf("text.format.name = %q; request=%s", got, out)
	}
}

func TestChatNonStreamResponseToResponses(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"gpt-test","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	translated, err := RequestForEndpoint("gpt-test", original, false, EndpointChatCompletions)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	upstream := []byte(`{
		"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"gpt-test",
		"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
	}`)
	out, err := ResponseToResponses(context.Background(), EndpointChatCompletions, "gpt-test", original, translated, upstream)
	if err != nil {
		t.Fatalf("translate response: %v", err)
	}
	data := gjson.ParseBytes(out)
	if got := data.Get("status").String(); got != "completed" {
		t.Fatalf("status = %q; response=%s", got, out)
	}
	if got := data.Get("output.0.content.0.text").String(); got != "pong" {
		t.Fatalf("output text = %q; response=%s", got, out)
	}
	if got := data.Get("usage.total_tokens").Int(); got != 3 {
		t.Fatalf("total tokens = %d; response=%s", got, out)
	}
}

func TestResponsesNonStreamResponseToClaude(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"gpt-5.6-sol","max_tokens":20,"messages":[{"role":"user","content":"ping"}]}`)
	translated, err := RequestForEndpointFrom("claude", "gpt-5.6-sol", original, false, EndpointResponses)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	upstream := []byte(`{
		"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-5.6-sol",
		"output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"pong","annotations":[]}]}],
		"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}
	}`)
	out, err := ResponseFromEndpoint(context.Background(), EndpointResponses, "claude", "gpt-5.6-sol", original, translated, upstream)
	if err != nil {
		t.Fatalf("translate response: %v", err)
	}
	data := gjson.ParseBytes(out)
	if got := data.Get("type").String(); got != "message" {
		t.Fatalf("Claude response type = %q; response=%s", got, out)
	}
	if got := data.Get("content.0.text").String(); got != "pong" {
		t.Fatalf("Claude response text = %q; response=%s", got, out)
	}
	if got := data.Get("usage.input_tokens").Int(); got != 2 {
		t.Fatalf("Claude input tokens = %d; response=%s", got, out)
	}
}

func TestChatSSEToResponses(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"gpt-test","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	translated, err := RequestForEndpoint("gpt-test", original, true, EndpointChatCompletions)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	chunks := [][]byte{
		[]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"po"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"ng"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`),
		[]byte(`data: [DONE]`),
	}
	var state any
	var output strings.Builder
	for _, chunk := range chunks {
		frames, errTranslate := StreamToResponses(context.Background(), EndpointChatCompletions, "gpt-test", original, translated, chunk, &state)
		if errTranslate != nil {
			t.Fatalf("translate SSE: %v", errTranslate)
		}
		for _, frame := range frames {
			output.Write(frame)
		}
	}
	text := output.String()
	for _, needle := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		`"delta":"po"`,
		`"delta":"ng"`,
		"event: response.completed",
		`"total_tokens":3`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("translated SSE omits %q:\n%s", needle, text)
		}
	}
}

func TestResponsesSSEToClaude(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"gpt-5.6-sol","max_tokens":20,"messages":[{"role":"user","content":"ping"}]}`)
	translated, err := RequestForEndpointFrom("claude", "gpt-5.6-sol", original, true, EndpointResponses)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	chunks := [][]byte{
		[]byte(`event: response.created
data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","model":"gpt-5.6-sol","output":[],"usage":{"input_tokens":2,"output_tokens":0}}}

`),
		[]byte(`event: response.content_part.added
data: {"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

`),
		[]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"one"}

`),
		[]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" two"}

`),
		[]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" three"}

`),
		[]byte(`event: response.content_part.done
data: {"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"one two three"}}

`),
		[]byte(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-5.6-sol","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"one two three"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}

`),
	}
	var state any
	var output strings.Builder
	for _, chunk := range chunks {
		frames, errTranslate := StreamFromEndpoint(context.Background(), EndpointResponses, "claude", "gpt-5.6-sol", original, translated, chunk, &state)
		if errTranslate != nil {
			t.Fatalf("translate SSE: %v", errTranslate)
		}
		for _, frame := range frames {
			output.Write(frame)
		}
	}
	text := output.String()
	for _, needle := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text"`,
		`"text":"one"`,
		`"text":" two"`,
		`"text":" three"`,
		`"stop_reason":"end_turn"`,
		`"output_tokens":3`,
		"event: message_stop",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("Claude SSE omits %q:\n%s", needle, text)
		}
	}
}

func TestIsGPTModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model string
		want  bool
	}{
		{model: "gpt-6-astra", want: true},
		{model: "GPT-6-ASTRA", want: true},
		{model: "gpt-5.6-sol", want: true},
		{model: "gpt-5.6-terra", want: true},
		{model: "gpt-4o", want: true},
		{model: "gpt-4o-mini", want: true},
		{model: "gpt-4", want: true},
		{model: "gpt-3.5-turbo", want: true},
		{model: "chatgpt-4o-latest", want: true},
		{model: "openai/gpt-6-astra", want: true},
		{model: "copilot/gpt-6-astra", want: true},
		{model: "copilot-gpt-4o", want: true},
		{model: "claude-3-7-sonnet", want: false},
		{model: "claude-opus-5", want: false},
		{model: "gemini-2.0-flash", want: false},
		{model: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			if got := IsGPTModel(tc.model); got != tc.want {
				t.Fatalf("IsGPTModel(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestRequestStripsTemperatureForGPTModels(t *testing.T) {
	t.Parallel()

	claudePayload := []byte(`{
		"model":"ignored",
		"temperature":0.7,
		"messages":[{"role":"user","content":"hello"}]
	}`)

	out, err := RequestForEndpointFrom("claude", "gpt-6-astra", claudePayload, false, EndpointResponses)
	if err != nil {
		t.Fatalf("RequestForEndpointFrom claude: %v", err)
	}
	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("expected temperature to be stripped for gpt-6-astra: %s", out)
	}

	claudeOut, errClaude := ClaudeToResponses("gpt-6-astra", claudePayload, false)
	if errClaude != nil {
		t.Fatalf("ClaudeToResponses: %v", errClaude)
	}
	if gjson.GetBytes(claudeOut, "temperature").Exists() {
		t.Fatalf("expected temperature to be stripped in ClaudeToResponses for gpt-6-astra: %s", claudeOut)
	}

	responsesPayload := []byte(`{
		"model":"gpt-6-astra",
		"temperature":0.7,
		"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]
	}`)

	responsesOut, errResponses := RequestForEndpointFrom("openai-response", "gpt-6-astra", responsesPayload, false, EndpointResponses)
	if errResponses != nil {
		t.Fatalf("RequestForEndpointFrom responses: %v", errResponses)
	}
	if gjson.GetBytes(responsesOut, "temperature").Exists() {
		t.Fatalf("expected temperature to be stripped for gpt-6-astra in responses: %s", responsesOut)
	}

	chatCompletionsOut, errCompletions := RequestForEndpoint("gpt-6-astra", responsesPayload, false, EndpointChatCompletions)
	if errCompletions != nil {
		t.Fatalf("RequestForEndpoint chat completions: %v", errCompletions)
	}
	if gjson.GetBytes(chatCompletionsOut, "temperature").Exists() {
		t.Fatalf("expected temperature to be stripped for gpt-6-astra in chat completions: %s", chatCompletionsOut)
	}
}

func TestRequestPreservesTemperatureForNonGPTModels(t *testing.T) {
	t.Parallel()

	claudePayload := []byte(`{
		"model":"ignored",
		"temperature":0.7,
		"messages":[{"role":"user","content":"hello"}]
	}`)

	out, err := RequestForEndpointFrom("claude", "claude-3-7-sonnet", claudePayload, false, EndpointResponses)
	if err != nil {
		t.Fatalf("RequestForEndpointFrom claude: %v", err)
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 0.7 {
		t.Fatalf("expected temperature to be preserved for non-GPT model, got %v: %s", got, out)
	}
}

func TestChatPassthroughEmitsBareJSONChunks(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"claude-opus-5.5","messages":[{"role":"user","content":"Say OK"}]}`)
	translated, err := RequestForEndpointFrom("openai", "claude-opus-5.5", original, true, EndpointChatCompletions)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	if gjson.GetBytes(translated, "stream").Bool() != true {
		t.Fatalf("translated request lost stream=true: %s", translated)
	}
	frames := [][]byte{
		[]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"),
		[]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_text\":\"hmm\"}}]}\n\n"),
		[]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"}}]}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}
	var state any
	var output strings.Builder
	for _, frame := range frames {
		chunks, errTranslate := StreamFromEndpoint(context.Background(), EndpointChatCompletions, "openai", "claude-opus-5.5", original, translated, frame, &state)
		if errTranslate != nil {
			t.Fatalf("translate frame %q: %v", frame, errTranslate)
		}
		for _, chunk := range chunks {
			if strings.HasPrefix(string(chunk), "data:") || strings.HasPrefix(string(chunk), "event:") {
				t.Fatalf("chunk keeps SSE framing, want bare JSON: %q", chunk)
			}
			output.Write(chunk)
			output.WriteString("\n")
		}
	}
	text := output.String()
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("upstream terminator leaked to the client: %s", text)
	}
	if !strings.Contains(text, `"reasoning_content":"hmm"`) || strings.Contains(text, `"reasoning_text"`) {
		t.Fatalf("reasoning fields were not mapped for the chat client: %s", text)
	}
	if !strings.Contains(text, `"content":"OK"`) {
		t.Fatalf("text delta missing: %s", text)
	}
}

func TestResponsesSSEToChatChunks(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"gpt-6-sol","messages":[{"role":"user","content":"ping"}]}`)
	translated, err := RequestForEndpointFrom("openai", "gpt-6-sol", original, true, EndpointResponses)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	if !strings.Contains(string(translated), `"input"`) {
		t.Fatalf("chat request was not converted to Responses input: %s", translated)
	}
	chunks := [][]byte{
		[]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-6-sol\",\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n"),
		[]byte("event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"r_1\",\"output_index\":0,\"delta\":\"thinking hard\"}\n\n"),
		[]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":1,\"content_index\":0,\"delta\":\"hello\"}\n\n"),
		[]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"gpt-6-sol\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}}\n\n"),
	}
	var state any
	var output strings.Builder
	for _, chunk := range chunks {
		frames, errTranslate := StreamFromEndpoint(context.Background(), EndpointResponses, "openai", "gpt-6-sol", original, translated, chunk, &state)
		if errTranslate != nil {
			t.Fatalf("translate Responses SSE: %v", errTranslate)
		}
		for _, frame := range frames {
			output.Write(frame)
		}
	}
	text := output.String()
	for _, needle := range []string{
		`"object":"chat.completion.chunk"`,
		`"reasoning_content":"thinking hard"`,
		`"content":"hello"`,
		`"finish_reason":"stop"`,
		`"completion_tokens":3`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("translated chat stream omits %q:\n%s", needle, text)
		}
	}
}

func TestResponsesNonStreamToChat(t *testing.T) {
	t.Parallel()

	body := []byte(`{"id":"resp_1","object":"response","model":"gpt-6-sol","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`)
	out, err := ResponseFromEndpoint(context.Background(), EndpointResponses, "openai", "gpt-6-sol", []byte(`{}`), []byte(`{}`), body)
	if err != nil {
		t.Fatalf("translate non-stream response: %v", err)
	}
	if got := gjson.GetBytes(out, "object").String(); got != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion: %s", got, out)
	}
	if got := gjson.GetBytes(out, "choices.0.message.content").String(); got != "answer" {
		t.Fatalf("message content = %q: %s", got, out)
	}
}

func TestOpenAIRequestForResponsesEndpointCarriesReasoning(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"gpt-6-sol","messages":[{"role":"user","content":"deep question"}],"reasoning_effort":"high","stream":false}`)
	out, err := RequestForEndpointFrom("openai", "gpt-6-sol", original, false, EndpointResponses)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high: %s", got, out)
	}
	if gjson.GetBytes(out, "messages").Exists() {
		t.Fatalf("chat messages leaked into the Responses request: %s", out)
	}
}
