package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// --- detectAPIType ---

func TestDetectAPIType(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v1/messages", "anthropic"},
		{"/v1/chat/completions", "openai"},
		{"/v1/responses", "openai"},
		{"/v1beta/models/gemini-2.0-flash:generateContent", "gemini"},
		{"/v1beta/models/gemini-2.0-flash:streamGenerateContent", "gemini"},
		{"/v1/embeddings", "unknown"},
		{"/something/else", "unknown"},
	}
	for _, tt := range tests {
		got := detectAPIType(tt.path, nil)
		if got != tt.want {
			t.Errorf("detectAPIType(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// --- injectAPIKey ---

func newInjectTestCfg(transparent bool) *Config {
	return &Config{
		Transparent: transparent,
		ListenAddr:  "127.0.0.1:8765",
		APIKeys: map[string]string{
			"anthropic": "sk-real-ant-key",
			"openai":    "sk-real-oai-key",
			"gemini":    "real-gem-key",
		},
	}
}

func TestInjectAPIKeyTransparentSelfReference(t *testing.T) {
	cfg := newInjectTestCfg(true)
	upstream, _ := http.NewRequest("POST", "https://api.z.ai/api/anthropic/v1/messages", nil)
	upstream.Header.Set("x-api-key", "dummy-agent-key")
	upstream.Header.Set("Authorization", "Bearer dummy-agent-token")
	upstream.Header.Set("x-goog-api-key", "stray-agent-key")

	// Agent pointed its BASE_URL at the proxy — always inject, even in transparent mode
	incoming, _ := http.NewRequest("POST", "http://localhost:8765/v1/messages", nil)
	cfg.injectAPIKey(upstream, "anthropic", incoming)

	if got := upstream.Header.Get("x-api-key"); got != "sk-real-ant-key" {
		t.Errorf("x-api-key = %q, want injected real key", got)
	}
	if got := upstream.Header.Get("Authorization"); got != "Bearer sk-real-ant-key" {
		t.Errorf("Authorization = %q, want Bearer with real key", got)
	}
	if got := upstream.Header.Get("x-goog-api-key"); got != "" {
		t.Errorf("x-goog-api-key = %q, want stripped", got)
	}
}

func TestInjectAPIKeyTransparentExternalPassThrough(t *testing.T) {
	cfg := newInjectTestCfg(true)
	upstream, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	upstream.Header.Set("x-api-key", "agent-own-key")

	// External destination — agent's own auth passes through untouched
	incoming, _ := http.NewRequest("POST", "http://api.anthropic.com/v1/messages", nil)
	incoming.Host = "api.anthropic.com"
	cfg.injectAPIKey(upstream, "anthropic", incoming)

	if got := upstream.Header.Get("x-api-key"); got != "agent-own-key" {
		t.Errorf("x-api-key = %q, want agent's own key (no injection)", got)
	}
	if got := upstream.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty (no injection)", got)
	}
}

func TestInjectAPIKeyNonTransparentAlwaysInjects(t *testing.T) {
	cfg := newInjectTestCfg(false)
	upstream, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	upstream.Header.Set("x-api-key", "agent-own-key")

	// Non-transparent: external destination still gets injection (legacy behavior)
	incoming, _ := http.NewRequest("POST", "http://api.anthropic.com/v1/messages", nil)
	incoming.Host = "api.anthropic.com"
	cfg.injectAPIKey(upstream, "anthropic", incoming)

	if got := upstream.Header.Get("x-api-key"); got != "sk-real-ant-key" {
		t.Errorf("x-api-key = %q, want injected real key", got)
	}
	if got := upstream.Header.Get("Authorization"); got != "Bearer sk-real-ant-key" {
		t.Errorf("Authorization = %q, want Bearer with real key", got)
	}
}

func TestInjectAPIKeyOpenAISelfReference(t *testing.T) {
	cfg := newInjectTestCfg(true)
	upstream, _ := http.NewRequest("POST", "https://api.example.com/v1/chat/completions", nil)
	upstream.Header.Set("Authorization", "Bearer dummy-agent-token")

	incoming, _ := http.NewRequest("POST", "http://127.0.0.1:8765/v1/chat/completions", nil)
	cfg.injectAPIKey(upstream, "openai", incoming)

	if got := upstream.Header.Get("Authorization"); got != "Bearer sk-real-oai-key" {
		t.Errorf("Authorization = %q, want Bearer with real key", got)
	}
}

func TestInjectAPIKeyGeminiSelfReference(t *testing.T) {
	cfg := newInjectTestCfg(true)
	upstream, _ := http.NewRequest("GET", "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent", nil)

	incoming, _ := http.NewRequest("GET", "http://localhost:8765/v1beta/models/gemini-2.0-flash:generateContent", nil)
	cfg.injectAPIKey(upstream, "gemini", incoming)

	if got := upstream.Header.Get("x-goog-api-key"); got != "real-gem-key" {
		t.Errorf("x-goog-api-key = %q, want injected real key", got)
	}
	if got := upstream.URL.Query().Get("key"); got != "real-gem-key" {
		t.Errorf("key query param = %q, want injected real key", got)
	}
}

func TestInjectAPIKeyNoKeyConfigured(t *testing.T) {
	cfg := newInjectTestCfg(true)
	cfg.APIKeys = map[string]string{}
	upstream, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	upstream.Header.Set("x-api-key", "agent-own-key")

	incoming, _ := http.NewRequest("POST", "http://localhost:8765/v1/messages", nil)
	cfg.injectAPIKey(upstream, "anthropic", incoming)

	// Nothing to inject — agent's headers pass through as-is
	if got := upstream.Header.Get("x-api-key"); got != "agent-own-key" {
		t.Errorf("x-api-key = %q, want agent's own key (nothing to inject)", got)
	}
}

// --- MODEL_MAP ---

func TestParseModelMap(t *testing.T) {
	rules := parseModelMap("claude-opus-*=glm-5.3, claude-sonnet-5=glm-5.1, ,bogus-line,claude-haiku-*=glm-4.6")
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3: %+v", len(rules), rules)
	}
	if !rules[0].prefix || rules[0].pattern != "claude-opus-" || rules[0].replacement != "glm-5.3" {
		t.Errorf("rule[0] = %+v, want prefix claude-opus- -> glm-5.3", rules[0])
	}
	if rules[1].prefix || rules[1].pattern != "claude-sonnet-5" || rules[1].replacement != "glm-5.1" {
		t.Errorf("rule[1] = %+v, want exact claude-sonnet-5 -> glm-5.1", rules[1])
	}
	if !rules[2].prefix || rules[2].pattern != "claude-haiku-" {
		t.Errorf("rule[2] = %+v, want prefix claude-haiku-", rules[2])
	}
	if parseModelMap("") != nil {
		t.Error("empty MODEL_MAP should yield no rules")
	}
}

func TestMapModel(t *testing.T) {
	rules := parseModelMap("claude-opus-*=glm-5.3,claude-sonnet-*=glm-5.3,claude-3-5-haiku-*=glm-4.6,gpt-4o=glm-5.3")
	tests := []struct {
		model string
		want  string
	}{
		{"claude-opus-5", "glm-5.3"},   // prefix match
		{"claude-sonnet-5", "glm-5.3"}, // prefix match
		{"claude-3-5-haiku-20241022", "glm-4.6"},
		{"gpt-4o", "glm-5.3"}, // exact match
		{"gpt-4o-mini", ""},   // exact rule must not prefix-match
		{"claude-sonnet-4-20250514", "glm-5.3"},
		{"some-other-model", ""}, // no match -> passthrough
		{"", ""},                 // empty model -> no match
	}
	for _, tt := range tests {
		if got := mapModel(rules, tt.model); got != tt.want {
			t.Errorf("mapModel(%q) = %q, want %q", tt.model, got, tt.want)
		}
	}
}

func TestMapModelFirstMatchWins(t *testing.T) {
	rules := parseModelMap("claude-*=glm-4.6,claude-sonnet-*=glm-5.3")
	if got := mapModel(rules, "claude-sonnet-5"); got != "glm-4.6" {
		t.Errorf("mapModel = %q, want glm-4.6 (first matching rule must win)", got)
	}
}

func TestRewriteModelInBody(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	rewritten, err := rewriteModelInBody(body, "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rewritten, &m); err != nil {
		t.Fatal(err)
	}
	if m.Model != "glm-5.3" {
		t.Errorf("model = %q, want glm-5.3", m.Model)
	}
	if m.MaxTokens != 32 || len(m.Messages) != 1 || m.Messages[0].Content != "hi" {
		t.Errorf("other fields not preserved: %+v", m)
	}

	// Invalid JSON is an error, not a silent passthrough
	if _, err := rewriteModelInBody([]byte(`not json`), "glm-5.3"); err == nil {
		t.Error("expected error for invalid JSON body")
	}
}

func TestRewriteGeminiModelPath(t *testing.T) {
	tests := []struct {
		path     string
		oldModel string
		want     string
	}{
		{"/v1beta/models/gemini-2.0-flash:generateContent", "gemini-2.0-flash", "/v1beta/models/glm-5.3:generateContent"},
		{"/v1beta/models/gemini-2.0-flash:streamGenerateContent", "gemini-2.0-flash", "/v1beta/models/glm-5.3:streamGenerateContent"},
		{"/v1beta/models/gemini-2.0-flash", "gemini-2.0-flash", "/v1beta/models/glm-5.3"},
		// A partial old-model name must NOT match a longer model in the path
		{"/v1beta/models/gemini-2.0-flash-1.5:generateContent", "gemini-2.0-flash", "/v1beta/models/gemini-2.0-flash-1.5:generateContent"},
		{"/v1/messages", "gemini-2.0-flash", "/v1/messages"}, // no model segment
	}
	for _, tt := range tests {
		if got := rewriteGeminiModelPath(tt.path, tt.oldModel, "glm-5.3"); got != tt.want {
			t.Errorf("rewriteGeminiModelPath(%q, %q) = %q, want %q", tt.path, tt.oldModel, got, tt.want)
		}
	}
}

// --- extractModel ---

func TestExtractModel(t *testing.T) {
	// Anthropic / OpenAI: model in body
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[]}`)
	got := extractModel(body, "/v1/messages")
	if got != "claude-sonnet-4-20250514" {
		t.Errorf("extractModel anthropic = %q, want claude-sonnet-4-20250514", got)
	}

	// Gemini: model in URL path
	got = extractModel(nil, "/v1beta/models/gemini-2.0-flash:generateContent")
	if got != "gemini-2.0-flash" {
		t.Errorf("extractModel gemini = %q, want gemini-2.0-flash", got)
	}

	// Gemini streaming
	got = extractModel(nil, "/v1beta/models/gemini-2.5-pro:streamGenerateContent")
	if got != "gemini-2.5-pro" {
		t.Errorf("extractModel gemini-stream = %q, want gemini-2.5-pro", got)
	}

	// Empty
	got = extractModel([]byte(`{}`), "/v1/something")
	if got != "" {
		t.Errorf("extractModel empty = %q, want empty", got)
	}
}

// --- extractSystemPrompt ---

func TestExtractSystemPromptAnthropic(t *testing.T) {
	// String system
	body := []byte(`{"model":"claude-sonnet-4-20250514","system":"You are helpful","messages":[]}`)
	got := extractSystemPrompt(body)
	if got != "You are helpful" {
		t.Errorf("anthropic string system = %q", got)
	}

	// Content block array
	body = []byte(`{"system":[{"type":"text","text":"Part 1"},{"type":"text","text":"Part 2"}]}`)
	got = extractSystemPrompt(body)
	if got != "Part 1\nPart 2" {
		t.Errorf("anthropic blocks system = %q", got)
	}
}

func TestExtractSystemPromptOpenAIChat(t *testing.T) {
	body := []byte(`{"model":"gpt-5","messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"hi"}]}`)
	got := extractSystemPrompt(body)
	if got != "Be concise" {
		t.Errorf("openai chat system = %q", got)
	}

	// Array content
	body = []byte(`{"messages":[{"role":"system","content":[{"type":"text","text":"Multi"},{"type":"text","text":"Part"}]}]}`)
	got = extractSystemPrompt(body)
	if got != "Multi\nPart" {
		t.Errorf("openai chat array system = %q", got)
	}
}

func TestExtractSystemPromptResponsesAPI(t *testing.T) {
	// Top-level instructions
	body := []byte(`{"model":"gpt-5","instructions":"Talk like a pirate","input":"hello"}`)
	got := extractSystemPrompt(body)
	if got != "Talk like a pirate" {
		t.Errorf("responses instructions = %q", got)
	}

	// Developer role in input array
	body = []byte(`{"model":"gpt-5","input":[{"role":"developer","content":"Be helpful"},{"role":"user","content":"hi"}]}`)
	got = extractSystemPrompt(body)
	if got != "Be helpful" {
		t.Errorf("responses developer role = %q", got)
	}

	// Developer role with array content
	body = []byte(`{"model":"gpt-5","input":[{"role":"developer","content":[{"type":"input_text","text":"Be"},{"type":"input_text","text":"helpful"}]}]}`)
	got = extractSystemPrompt(body)
	if got != "Be\nhelpful" {
		t.Errorf("responses developer array = %q", got)
	}
}

func TestExtractSystemPromptGemini(t *testing.T) {
	body := []byte(`{"contents":[],"systemInstruction":{"parts":[{"text":"You are a helpful assistant"}]}}`)
	got := extractSystemPrompt(body)
	if got != "You are a helpful assistant" {
		t.Errorf("gemini systemInstruction = %q", got)
	}

	// Multi-part
	body = []byte(`{"systemInstruction":{"parts":[{"text":"Part 1"},{"text":"Part 2"}]}}`)
	got = extractSystemPrompt(body)
	if got != "Part 1\nPart 2" {
		t.Errorf("gemini systemInstruction multi = %q", got)
	}
}

func TestExtractSystemPromptCodex(t *testing.T) {
	body := []byte(`{"instructions":"Codex system prompt","model":"gpt-5"}`)
	got := extractSystemPrompt(body)
	if got != "Codex system prompt" {
		t.Errorf("codex instructions = %q", got)
	}
}

// --- extractMessages ---

func TestExtractMessagesOpenAIChat(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}]}`)
	msgs := extractMessages(body, "openai")
	if len(msgs) != 2 {
		t.Fatalf("openai chat msgs = %d, want 2", len(msgs))
	}
	if msgs[0] != "**user**: hello" {
		t.Errorf("openai msg[0] = %q", msgs[0])
	}
	if msgs[1] != "**assistant**: hi" {
		t.Errorf("openai msg[1] = %q", msgs[1])
	}
}

func TestExtractMessagesResponsesAPI(t *testing.T) {
	// input as array
	body := []byte(`{"input":[{"role":"developer","content":"sys"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}]}`)
	msgs := extractMessages(body, "openai")
	if len(msgs) != 2 {
		t.Fatalf("responses array msgs = %d, want 2", len(msgs))
	}
	if msgs[0] != "**user**: hello" {
		t.Errorf("responses msg[0] = %q", msgs[0])
	}

	// input as string
	body = []byte(`{"input":"just a string","model":"gpt-5"}`)
	msgs = extractMessages(body, "openai")
	if len(msgs) != 1 || msgs[0] != "**user**: just a string" {
		t.Errorf("responses string input = %v", msgs)
	}
}

func TestExtractMessagesGemini(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"text":"hi there"}]}]}`)
	msgs := extractMessages(body, "gemini")
	if len(msgs) != 2 {
		t.Fatalf("gemini msgs = %d, want 2", len(msgs))
	}
	if msgs[0] != "**user**: hello" {
		t.Errorf("gemini msg[0] = %q", msgs[0])
	}
	if msgs[1] != "**assistant**: hi there" {
		t.Errorf("gemini msg[1] = %q", msgs[1])
	}
}

// --- extractStreamedText ---

func TestExtractStreamedTextAnthropic(t *testing.T) {
	data := []byte("data: " + marshal(map[string]interface{}{
		"type":  "content_block_delta",
		"delta": map[string]string{"text": "Hello"},
	}) + "\n" +
		"data: " + marshal(map[string]interface{}{
		"type":  "content_block_delta",
		"delta": map[string]string{"text": " world"},
	}) + "\n")
	got := extractStreamedText(data, "anthropic")
	if got != "Hello world" {
		t.Errorf("anthropic stream = %q", got)
	}
}

func TestExtractStreamedTextOpenAIChat(t *testing.T) {
	data := []byte("data: " + marshal(map[string]interface{}{
		"choices": []map[string]interface{}{
			{"delta": map[string]string{"content": "Hi"}},
		},
	}) + "\n" +
		"data: " + marshal(map[string]interface{}{
		"choices": []map[string]interface{}{
			{"delta": map[string]string{"content": " there"}},
		},
	}) + "\n" +
		"data: [DONE]\n")
	got := extractStreamedText(data, "openai")
	if got != "Hi there" {
		t.Errorf("openai chat stream = %q", got)
	}
}

func TestExtractStreamedTextResponsesAPI(t *testing.T) {
	data := []byte("data: " + marshal(map[string]interface{}{
		"type":  "response.output_text.delta",
		"delta": "Hello",
	}) + "\n" +
		"data: " + marshal(map[string]interface{}{
		"type":  "response.output_text.delta",
		"delta": " from Responses",
	}) + "\n" +
		"data: " + marshal(map[string]interface{}{
		"type": "response.output_text.done",
	}) + "\n")
	got := extractStreamedText(data, "openai")
	if got != "Hello from Responses" {
		t.Errorf("responses stream = %q", got)
	}
}

func TestExtractStreamedTextGemini(t *testing.T) {
	data := []byte(`[{"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"}}]},{"candidates":[{"content":{"parts":[{"text":" world"}],"role":"model"}}]}]`)
	got := extractStreamedText(data, "gemini")
	if got != "Hello world" {
		t.Errorf("gemini stream = %q", got)
	}
}

// --- modifySystemInBody ---

func TestModifySystemInBodyAnthropic(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","system":"old system","messages":[]}`)
	modified, err := modifySystemInBody(body, "new system")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	json.Unmarshal(modified, &m)
	if m["system"] != "new system" {
		t.Errorf("anthropic modify = %v", m["system"])
	}
}

func TestModifySystemInBodyOpenAIChat(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)
	modified, err := modifySystemInBody(body, "new system")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(modified, &m)
	for _, msg := range m.Messages {
		if msg.Role == "system" && msg.Content != "new system" {
			t.Errorf("openai modify = %q", msg.Content)
		}
	}
}

func TestModifySystemInBodyResponsesAPI(t *testing.T) {
	// Instructions field
	body := []byte(`{"model":"gpt-5","instructions":"old","input":"hello"}`)
	modified, err := modifySystemInBody(body, "new instructions")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	json.Unmarshal(modified, &m)
	if m["instructions"] != "new instructions" {
		t.Errorf("responses instructions modify = %v", m["instructions"])
	}

	// Developer role in input array
	body = []byte(`{"model":"gpt-5","input":[{"role":"developer","content":"old"},{"role":"user","content":"hi"}]}`)
	modified, err = modifySystemInBody(body, "new developer")
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(modified, &m)
	input := m["input"].([]interface{})
	devMsg := input[0].(map[string]interface{})
	if devMsg["content"] != "new developer" {
		t.Errorf("responses developer modify = %v", devMsg["content"])
	}
}

func TestModifySystemInBodyGemini(t *testing.T) {
	body := []byte(`{"contents":[],"systemInstruction":{"parts":[{"text":"old"}]}}`)
	modified, err := modifySystemInBody(body, "new system")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	json.Unmarshal(modified, &m)
	si := m["systemInstruction"].(map[string]interface{})
	parts := si["parts"].([]interface{})
	part := parts[0].(map[string]interface{})
	if part["text"] != "new system" {
		t.Errorf("gemini modify = %v", part["text"])
	}
}

// --- isStreamingRequest ---

func TestIsStreamingRequest(t *testing.T) {
	// Body stream flag
	if !isStreamingRequest([]byte(`{"stream":true}`), "/v1/messages") {
		t.Error("stream:true should be streaming")
	}
	if isStreamingRequest([]byte(`{"stream":false}`), "/v1/messages") {
		t.Error("stream:false should not be streaming")
	}

	// Gemini path-based detection
	if !isStreamingRequest(nil, "/v1beta/models/gemini-2.0-flash:streamGenerateContent") {
		t.Error("Gemini streamGenerateContent should be streaming")
	}
	if isStreamingRequest(nil, "/v1beta/models/gemini-2.0-flash:generateContent") {
		t.Error("Gemini generateContent should not be streaming")
	}
}

// helper

func marshal(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
