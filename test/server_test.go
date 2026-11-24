package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestNormalizeServeAddr(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "127.0.0.1:8080", false},
		{"8080", "127.0.0.1:8080", false},
		{":8080", ":8080", false},
		{"127.0.0.1:9090", "127.0.0.1:9090", false},
		{"0.0.0.0:8080", "0.0.0.0:8080", false},
		{"127.0.0.1:0", "127.0.0.1:0", false},
		{"0", "", true},
		{"70000", "", true},
		{"not-a-port", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeServeAddr(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeServeAddr(%q) expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeServeAddr(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeServeAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseStealthArgsServe(t *testing.T) {
	opts, err := parseStealthArgs([]string{"--serve", "9090", "--serve-key", "secret"})
	if err != nil {
		t.Fatalf("parseStealthArgs: %v", err)
	}
	if !opts.serveMode || opts.serveAddr != "9090" || opts.serveKey != "secret" {
		t.Fatalf("unexpected opts: %+v", opts)
	}

	opts, err = parseStealthArgs([]string{"--serve", "--token", "tok"})
	if err != nil {
		t.Fatalf("parseStealthArgs default addr: %v", err)
	}
	if !opts.serveMode || opts.serveAddr != "8080" || opts.token != "tok" {
		t.Fatalf("unexpected default serve opts: %+v", opts)
	}

	opts, err = parseStealthArgs([]string{"--serve=127.0.0.1:7777"})
	if err != nil {
		t.Fatalf("parseStealthArgs equals: %v", err)
	}
	if !opts.serveMode || opts.serveAddr != "127.0.0.1:7777" {
		t.Fatalf("unexpected equals serve opts: %+v", opts)
	}

	if _, err := parseStealthArgs([]string{"--serve="}); err == nil {
		t.Fatal("expected error for empty --serve=")
	}
}

func TestResolveServeModel(t *testing.T) {
	chat := resolveServeModel("deepseek-chat", stealthOptions{})
	if chat.Thinking || chat.Search {
		t.Fatalf("deepseek-chat = %+v", chat)
	}
	reasoner := resolveServeModel("deepseek/deepseek-reasoner", stealthOptions{})
	if !reasoner.Thinking || reasoner.Search {
		t.Fatalf("deepseek-reasoner = %+v", reasoner)
	}
	searchChat := resolveServeModel("deepseek-chat-search", stealthOptions{})
	if searchChat.Thinking || !searchChat.Search {
		t.Fatalf("deepseek-chat-search = %+v", searchChat)
	}
	custom := resolveServeModel("gpt-4o", stealthOptions{think: true, noSearch: true})
	if !custom.Thinking || custom.Search {
		t.Fatalf("custom fallback = %+v", custom)
	}
}

func TestOAITextContentUnmarshal(t *testing.T) {
	var msg oaiMessage
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hello"}`), &msg); err != nil {
		t.Fatalf("string content: %v", err)
	}
	if msg.Content != "hello" {
		t.Fatalf("got %q, want hello", msg.Content)
	}

	if err := json.Unmarshal([]byte(`{"role":"assistant","content":null}`), &msg); err != nil {
		t.Fatalf("null content: %v", err)
	}
	if msg.Content != "" {
		t.Fatalf("got %q, want empty", msg.Content)
	}

	partsJSON := `{"role":"user","content":[{"type":"text","text":"line 1"},{"type":"input_text","text":"line 2"},{"type":"image_url","image_url":{"url":"https://example.invalid/a.png"}}]}`
	if err := json.Unmarshal([]byte(partsJSON), &msg); err != nil {
		t.Fatalf("parts content: %v", err)
	}
	if string(msg.Content) != "line 1\nline 2\n[image omitted]" {
		t.Fatalf("parts = %q", msg.Content)
	}
}

func TestToolStreamParserSplitChunksAndMultipleCalls(t *testing.T) {
	var textOut strings.Builder
	var calls []oaiToolCall

	p := newToolStreamParser(true, nil, func(s string) {
		textOut.WriteString(s)
	}, func(tc oaiToolCall) {
		calls = append(calls, tc)
	})

	chunks := []string{
		"Checking files <",
		"tool_call>\n{\"name\":\"read_file\",\"arguments\":{\"path\":",
		"\"main.go\"}}\n</tool_",
		"call>\nNext <to",
		"ol_call>{\"name\":\"list_dir\",\"arguments\":\"{\\\"dir\\\":\\\".\\\"}\"}</tool_call> done.",
	}
	for _, c := range chunks {
		p.Feed(c)
	}
	p.Finish()

	if got := textOut.String(); got != "Checking files \nNext  done." {
		t.Fatalf("prose = %q", got)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0].Function.Name != "read_file" || calls[0].Function.Arguments != `{"path":"main.go"}` {
		t.Fatalf("call 0 = %+v", calls[0])
	}
	if calls[1].Function.Name != "list_dir" || calls[1].Function.Arguments != `{"dir":"."}` {
		t.Fatalf("call 1 = %+v", calls[1])
	}
	if !strings.HasPrefix(calls[0].ID, "call_") {
		t.Fatalf("unexpected call ID: %q", calls[0].ID)
	}
}

func TestToolStreamParserInvalidJSONAndStopSequence(t *testing.T) {
	var textOut strings.Builder
	var calls []oaiToolCall
	p := newToolStreamParser(true, []string{"HALT"}, func(s string) {
		textOut.WriteString(s)
	}, func(tc oaiToolCall) {
		calls = append(calls, tc)
	})

	p.Feed("Literal <tool_call>not json</tool_call> and HA")
	p.Feed("LT trailing ignored")
	p.Finish()

	if len(calls) != 0 {
		t.Fatalf("expected 0 valid calls, got %d", len(calls))
	}
	if !p.stopped {
		t.Fatal("expected stop sequence to trigger")
	}
	if got := textOut.String(); got != "Literal <tool_call>not json</tool_call> and " {
		t.Fatalf("prose = %q", got)
	}
}

func TestToolStreamParserDSMLFormat(t *testing.T) {
	var textOut strings.Builder
	var calls []oaiToolCall

	p := newToolStreamParser(true, nil, func(s string) {
		textOut.WriteString(s)
	}, func(tc oaiToolCall) {
		calls = append(calls, tc)
	})

	chunks := []string{
		"Reading file <",
		"｜DSML｜ calls>\n<｜DSML｜ invoke name=\"read\">\n",
		"<｜DSML｜ parameter name=\"filePath\" string=\"true\">hello.txt</｜DSML｜ parameter>\n",
		"<｜DSML｜ parameter name=\"limit\" string=\"false\">25</｜DSML｜ parameter>\n",
		"</｜DSML｜ invoke>\n</｜DSML｜ calls> ok",
	}
	for _, c := range chunks {
		p.Feed(c)
	}
	p.Finish()

	if got := textOut.String(); got != "Reading file  ok" {
		t.Fatalf("prose = %q", got)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Function.Name != "read" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("bad args JSON %q: %v", calls[0].Function.Arguments, err)
	}
	if args["filePath"] != "hello.txt" || args["limit"] != float64(25) {
		t.Fatalf("args = %+v", args)
	}

	// Also verify double fullwidth bar DSML variant (<｜｜DSML｜｜ ...>) observed from live DeepSeek.
	textOut.Reset()
	calls = nil
	p2 := newToolStreamParser(true, nil, func(s string) {
		textOut.WriteString(s)
	}, func(tc oaiToolCall) {
		calls = append(calls, tc)
	})
	p2.Feed("<｜｜DSML｜｜ calls>\n<｜｜DSML｜｜ invoke name=\"read\">\n")
	p2.Feed("<｜｜DSML｜｜ parameter name=\"path\" string=\"true\">hello.txt</｜｜DSML｜｜ parameter>\n")
	p2.Feed("</｜｜DSML｜｜ invoke>\n</｜｜DSML｜｜ calls>")
	p2.Finish()
	if len(calls) != 1 || calls[0].Function.Name != "read" {
		t.Fatalf("double-bar DSML calls = %+v", calls)
	}
}

func newMockDeepSeekUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, step int)) *httptest.Server {
	t.Helper()
	const (
		salt     = "mock-salt"
		expireAt = int64(1700000000000)
	)
	digest := deepSeekHashV1([]byte(fmt.Sprintf("%s_%d_0", salt, expireAt)))
	challenge := hex.EncodeToString(digest[:])

	var mu sync.Mutex
	completionCount := 0

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat_session/create":
			fmt.Fprint(w, `{"code":0,"data":{"biz_data":{"id":"sess-serve-1"}}}`)
		case "/chat/create_pow_challenge":
			fmt.Fprintf(w, `{"code":0,"data":{"biz_data":{"algorithm":"DeepSeekHashV1","challenge":%q,"salt":%q,"expire_at":%d,"difficulty":1,"signature":"sig","target_path":"/api/v0/chat/completion"}}}`,
				challenge, salt, expireAt)
		case "/chat/completion":
			mu.Lock()
			completionCount++
			step := completionCount
			mu.Unlock()
			handler(w, r, step)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestServeModelsAndAuth(t *testing.T) {
	srv := newServeServer(NewAPIClient("tok"), stealthOptions{serveKey: "my-local-secret"}, nil)
	h := srv.Handler()

	// Health requires no auth
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/health status = %d", rec.Code)
	}

	// /v1/models rejects missing auth when serveKey is set
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/models status = %d, want 401", rec.Code)
	}

	// /v1/models accepts Bearer key
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer my-local-secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated /v1/models status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var modelsResp struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &modelsResp); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if modelsResp.Object != "list" || len(modelsResp.Data) < 4 {
		t.Fatalf("unexpected models response: %+v", modelsResp)
	}
}

func TestServeNonStreamingToolCallingAndSessionCache(t *testing.T) {
	var captured []map[string]any
	var mu sync.Mutex

	upstream := newMockDeepSeekUpstream(t, func(w http.ResponseWriter, r *http.Request, step int) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		captured = append(captured, payload)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if step == 1 {
			fmt.Fprint(w, "event: ready\ndata: {\"response_message_id\":101}\n\n")
			fmt.Fprint(w, "data: {\"v\":{\"response\":{\"fragments\":[{\"id\":1,\"type\":\"THINK\",\"content\":\"need to read file\"},{\"id\":2,\"type\":\"RESPONSE\",\"content\":\"<tool_call>{\\\"name\\\":\\\"read_file\\\",\\\"arguments\\\":{\\\"path\\\":\\\"go.mod\\\"}}</tool_call>\"}]}}}\n\n")
			fmt.Fprint(w, "event: close\ndata: {}\n\n")
			return
		}
		fmt.Fprint(w, "event: ready\ndata: {\"response_message_id\":102}\n\n")
		fmt.Fprint(w, "data: {\"p\":\"response/content\",\"o\":\"APPEND\",\"v\":\"The module is deepseek-tui.\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	defer upstream.Close()

	client := NewAPIClient("tok")
	client.baseURL = upstream.URL
	srv := newServeServer(client, stealthOptions{}, nil)
	h := srv.Handler()

	// Turn 1: agent sends system + user + tool schema
	turn1Req := `{
		"model": "deepseek-reasoner",
		"stream": false,
		"tools": [{
			"type": "function",
			"function": {
				"name": "read_file",
				"description": "Read a file",
				"parameters": {"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
			}
		}],
		"messages": [
			{"role": "system", "content": "You are a coding agent."},
			{"role": "user", "content": "Check go.mod"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(turn1Req))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn 1 status = %d, body: %s", rec.Code, rec.Body.String())
	}

	var resp1 oaiChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("decode turn 1: %v", err)
	}
	if resp1.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("turn 1 finish_reason = %q, want tool_calls", resp1.Choices[0].FinishReason)
	}
	if len(resp1.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("turn 1 tool_calls = %+v", resp1.Choices[0].Message.ToolCalls)
	}
	tc := resp1.Choices[0].Message.ToolCalls[0]
	if tc.Function.Name != "read_file" || tc.Function.Arguments != `{"path":"go.mod"}` {
		t.Fatalf("unexpected tool call: %+v", tc)
	}
	if resp1.Choices[0].Message.ReasoningContent != "need to read file" {
		t.Fatalf("reasoning = %q", resp1.Choices[0].Message.ReasoningContent)
	}

	// Turn 2: agent sends assistant tool_call + tool result
	turn2Obj := map[string]any{
		"model":  "deepseek-reasoner",
		"stream": false,
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "read_file",
					"description": "Read a file",
					"parameters":  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}},
				},
			},
		},
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a coding agent."},
			map[string]any{"role": "user", "content": "Check go.mod"},
			map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{
					map[string]any{
						"id":   tc.ID,
						"type": "function",
						"function": map[string]any{
							"name":      "read_file",
							"arguments": tc.Function.Arguments,
						},
					},
				},
			},
			map[string]any{
				"role":         "tool",
				"tool_call_id": tc.ID,
				"name":         "read_file",
				"content":      "module deepseek-tui\ngo 1.24.2",
			},
		},
	}
	turn2Bytes, _ := json.Marshal(turn2Obj)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(turn2Bytes))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("turn 2 status = %d, body: %s", rec2.Code, rec2.Body.String())
	}

	var resp2 oaiChatResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode turn 2: %v", err)
	}
	if resp2.Choices[0].FinishReason != "stop" {
		t.Fatalf("turn 2 finish_reason = %q, want stop", resp2.Choices[0].FinishReason)
	}
	if resp2.Choices[0].Message.Content == nil || *resp2.Choices[0].Message.Content != "The module is deepseek-tui." {
		t.Fatalf("turn 2 content = %v", resp2.Choices[0].Message.Content)
	}

	// Verify Turn 2 hit session cache and chained parent_message_id = 101 with only the tool_result tail!
	if len(captured) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(captured))
	}
	if captured[1]["parent_message_id"] != float64(101) {
		t.Fatalf("turn 2 parent_message_id = %v, want 101", captured[1]["parent_message_id"])
	}
	prompt2, _ := captured[1]["prompt"].(string)
	if !strings.Contains(prompt2, "<tool_result") || strings.Contains(prompt2, "### Available Tools") {
		t.Fatalf("turn 2 should send only incremental tool_result tail, got:\n%s", prompt2)
	}
}

func TestServeStreamingToolCallsAndUsage(t *testing.T) {
	upstream := newMockDeepSeekUpstream(t, func(w http.ResponseWriter, r *http.Request, step int) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "event: ready\ndata: {\"response_message_id\":202}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"v\":{\"response\":{\"fragments\":[{\"id\":1,\"type\":\"THINK\",\"content\":\"thinking...\"},{\"id\":2,\"type\":\"RESPONSE\",\"content\":\"Calling <tool_\"}]}}}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"p\":\"response/fragments/-1/content\",\"o\":\"APPEND\",\"v\":\"call>{\\\"name\\\":\\\"bash\\\",\\\"arguments\\\":{\\\"cmd\\\":\\\"ls\\\"}}</tool_call>\"}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "event: close\ndata: {}\n\n")
		flusher.Flush()
	})
	defer upstream.Close()

	client := NewAPIClient("tok")
	client.baseURL = upstream.URL
	srv := newServeServer(client, stealthOptions{}, nil)

	body := `{
		"model": "deepseek-reasoner",
		"stream": true,
		"stream_options": {"include_usage": true},
		"tools": [{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}],
		"messages": [{"role":"user","content":"list files"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	var gotRole, gotReasoning, gotProse, gotName, gotArgs, gotFinish string
	var gotDone, gotUsage bool

	scanner := bufio.NewScanner(rec.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			gotDone = true
			continue
		}
		var chunk oaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad SSE chunk %q: %v", data, err)
		}
		if chunk.Usage != nil {
			gotUsage = true
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Role != "" {
				gotRole = ch.Delta.Role
			}
			gotReasoning += ch.Delta.ReasoningContent
			if ch.Delta.Content != nil {
				gotProse += *ch.Delta.Content
			}
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Function.Name != "" {
					gotName = tc.Function.Name
				}
				gotArgs += tc.Function.Arguments
			}
			if ch.FinishReason != nil {
				gotFinish = *ch.FinishReason
			}
		}
	}

	if !gotDone || !gotUsage {
		t.Fatalf("done=%v usage=%v", gotDone, gotUsage)
	}
	if gotRole != "assistant" || gotReasoning != "thinking..." || gotProse != "Calling " {
		t.Fatalf("role=%q reasoning=%q prose=%q", gotRole, gotReasoning, gotProse)
	}
	if gotName != "bash" || gotArgs != `{"cmd":"ls"}` || gotFinish != "tool_calls" {
		t.Fatalf("tool name=%q args=%q finish=%q", gotName, gotArgs, gotFinish)
	}
}

func TestServeResponsesAPIAndPreviousResponseID(t *testing.T) {
	var captured []map[string]any
	var mu sync.Mutex

	upstream := newMockDeepSeekUpstream(t, func(w http.ResponseWriter, r *http.Request, step int) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		captured = append(captured, payload)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if step == 1 {
			fmt.Fprint(w, "event: ready\ndata: {\"response_message_id\":301}\n\n")
			fmt.Fprint(w, "data: {\"p\":\"response/content\",\"o\":\"APPEND\",\"v\":\"<｜DSML｜ calls><｜DSML｜ invoke name=\\\"shell\\\"><｜DSML｜ parameter name=\\\"cmd\\\" string=\\\"true\\\">pwd</｜DSML｜ parameter></｜DSML｜ invoke></｜DSML｜ calls>\"}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "event: ready\ndata: {\"response_message_id\":302}\n\n")
		fmt.Fprint(w, "data: {\"p\":\"response/content\",\"o\":\"APPEND\",\"v\":\"Current directory is /app.\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	defer upstream.Close()

	client := NewAPIClient("tok")
	client.baseURL = upstream.URL
	srv := newServeServer(client, stealthOptions{}, nil)
	h := srv.Handler()

	req1Body := `{
		"model": "deepseek-chat",
		"instructions": "You are Codex.",
		"tools": [{
			"type": "function",
			"name": "shell",
			"description": "Run shell command",
			"parameters": {"type":"object","properties":{"cmd":{"type":"string"}}}
		}],
		"input": "Print working dir"
	}`
	req1 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(req1Body))
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("responses turn 1 status = %d, body: %s", rec1.Code, rec1.Body.String())
	}

	var resp1 struct {
		ID     string `json:"id"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("decode resp1: %v", err)
	}
	if len(resp1.Output) != 1 || resp1.Output[0].Type != "function_call" || resp1.Output[0].Name != "shell" {
		t.Fatalf("unexpected resp1 output: %+v", resp1.Output)
	}
	callID := resp1.Output[0].CallID

	req2Body, _ := json.Marshal(map[string]any{
		"model":                "deepseek-chat",
		"instructions":         "You are Codex.",
		"previous_response_id": resp1.ID,
		"tools": []any{
			map[string]any{
				"type":        "function",
				"name":        "shell",
				"description": "Run shell command",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}},
			},
		},
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  "/app",
			},
		},
	})
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(req2Body))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("responses turn 2 status = %d, body: %s", rec2.Code, rec2.Body.String())
	}
	var resp2 struct {
		OutputText string `json:"output_text"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode resp2: %v", err)
	}
	if resp2.OutputText != "Current directory is /app." {
		t.Fatalf("output_text = %q", resp2.OutputText)
	}
	if len(captured) != 2 || captured[1]["parent_message_id"] != float64(301) {
		t.Fatalf("expected same session parent_message_id=301 on turn 2, captured=%+v", captured)
	}
}

func TestServeResponsesStreaming(t *testing.T) {
	upstream := newMockDeepSeekUpstream(t, func(w http.ResponseWriter, r *http.Request, step int) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ready\ndata: {\"response_message_id\":401}\n\n")
		fmt.Fprint(w, "data: {\"p\":\"response/content\",\"o\":\"APPEND\",\"v\":\"Hi <tool_call>{\\\"name\\\":\\\"ping\\\",\\\"arguments\\\":{}}</tool_call>\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	defer upstream.Close()

	client := NewAPIClient("tok")
	client.baseURL = upstream.URL
	srv := newServeServer(client, stealthOptions{}, nil)

	body := `{
		"model": "deepseek-chat",
		"stream": true,
		"tools": [{"type":"function","name":"ping","parameters":{"type":"object"}}],
		"input": [{"role":"user","content":"ping"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		"event: response.function_call_arguments.delta",
		"event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestServeAnthropicMessagesAPIAndStreaming(t *testing.T) {
	var captured []map[string]any
	var mu sync.Mutex

	upstream := newMockDeepSeekUpstream(t, func(w http.ResponseWriter, r *http.Request, step int) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		captured = append(captured, payload)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if step == 1 {
			fmt.Fprint(w, "event: ready\ndata: {\"response_message_id\":501}\n\n")
			fmt.Fprint(w, "data: {\"v\":{\"response\":{\"fragments\":[{\"id\":1,\"type\":\"THINK\",\"content\":\"check file\"},{\"id\":2,\"type\":\"RESPONSE\",\"content\":\"<tool_call>{\\\"name\\\":\\\"Read\\\",\\\"arguments\\\":{\\\"file_path\\\":\\\"README.md\\\"}}</tool_call>\"}]}}}\n\n")
			fmt.Fprint(w, "event: close\ndata: {}\n\n")
			return
		}
		fmt.Fprint(w, "event: ready\ndata: {\"response_message_id\":502}\n\n")
		fmt.Fprint(w, "data: {\"p\":\"response/content\",\"o\":\"APPEND\",\"v\":\"Done reading.\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	defer upstream.Close()

	client := NewAPIClient("tok")
	client.baseURL = upstream.URL
	srv := newServeServer(client, stealthOptions{serveKey: "claude-key"}, nil)
	h := srv.Handler()

	// Turn 1: non-streaming Anthropic Messages with x-api-key
	req1Body := `{
		"model": "deepseek-reasoner",
		"system": [{"type":"text","text":"You are Claude Code."}],
		"tools": [{
			"name": "Read",
			"description": "Read file",
			"input_schema": {"type":"object","properties":{"file_path":{"type":"string"}}}
		}],
		"messages": [{"role":"user","content":"Read README.md"}]
	}`
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(req1Body))
	req1.Header.Set("x-api-key", "claude-key")
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("anthropic turn 1 status = %d, body: %s", rec1.Code, rec1.Body.String())
	}
	var resp1 struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string         `json:"type"`
			Thinking string         `json:"thinking"`
			ID       string         `json:"id"`
			Name     string         `json:"name"`
			Input    map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("decode anthropic resp1: %v", err)
	}
	if resp1.StopReason != "tool_use" || len(resp1.Content) != 2 {
		t.Fatalf("unexpected resp1: %+v", resp1)
	}
	if resp1.Content[0].Type != "thinking" || resp1.Content[1].Type != "tool_use" || resp1.Content[1].Input["file_path"] != "README.md" {
		t.Fatalf("unexpected blocks: %+v", resp1.Content)
	}
	toolUseID := resp1.Content[1].ID

	// Turn 2: streaming Anthropic Messages with tool_result -> must reuse same session!
	req2Body, _ := json.Marshal(map[string]any{
		"model":  "deepseek-reasoner",
		"stream": true,
		"system": []any{map[string]any{"type": "text", "text": "You are Claude Code."}},
		"tools": []any{
			map[string]any{
				"name":         "Read",
				"description":  "Read file",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}},
			},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "Read README.md"},
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": toolUseID, "name": "Read", "input": map[string]any{"file_path": "README.md"}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": toolUseID, "content": "# deepseek-tui"},
				},
			},
		},
	})
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(req2Body))
	req2.Header.Set("x-api-key", "claude-key")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("anthropic turn 2 status = %d, body: %s", rec2.Code, rec2.Body.String())
	}
	streamOut := rec2.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "Done reading.", "event: message_stop"} {
		if !strings.Contains(streamOut, want) {
			t.Fatalf("missing %q in stream:\n%s", want, streamOut)
		}
	}
	if len(captured) != 2 || captured[1]["parent_message_id"] != float64(501) {
		t.Fatalf("expected same session parent_message_id=501 on turn 2, captured=%+v", captured)
	}
}

func TestServeSurfacesHTTP200JSONChallengeError(t *testing.T) {
	upstream := newMockDeepSeekUpstream(t, func(w http.ResponseWriter, r *http.Request, step int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"code":40303,"msg":"CHALLENGE_REQUIRED","data":{"provider":"hcaptcha"}}`)
	})
	defer upstream.Close()

	client := NewAPIClient("tok")
	client.baseURL = upstream.URL
	srv := newServeServer(client, stealthOptions{}, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "CHALLENGE_REQUIRED") {
		t.Fatalf("expected CHALLENGE_REQUIRED in error body: %s", rec.Body.String())
	}
}
