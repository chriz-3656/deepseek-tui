package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// OpenAI Responses API (POST /v1/responses) — used by Codex CLI & OpenAI SDK
// -----------------------------------------------------------------------------

type responsesRequest struct {
	Model              string            `json:"model"`
	Instructions       string            `json:"instructions,omitempty"`
	Input              json.RawMessage   `json:"input"`
	Tools              []json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage   `json:"tool_choice,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Stream             bool              `json:"stream"`
	Reasoning          *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`
}

type responsesState struct {
	messages []oaiMessage
}

type responsesHistoryStore struct {
	mu      sync.Mutex
	entries map[string]responsesState
	order   []string
}

func newResponsesHistoryStore() *responsesHistoryStore {
	return &responsesHistoryStore{
		entries: make(map[string]responsesState),
	}
}

func (s *responsesHistoryStore) get(id string) (responsesState, bool) {
	if id == "" {
		return responsesState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.entries[id]
	return v, ok
}

func (s *responsesHistoryStore) put(id string, state responsesState) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[id]; !exists {
		if len(s.order) >= maxSessionCacheEntries {
			evict := s.order[0]
			s.order = s.order[1:]
			delete(s.entries, evict)
		}
		s.order = append(s.order, id)
	}
	copied := make([]oaiMessage, len(state.messages))
	copy(copied, state.messages)
	s.entries[id] = responsesState{messages: copied}
}

func (s *serveServer) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxServeRequestBytes+1))
	if err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body.")
		return
	}
	if len(body) > maxServeRequestBytes {
		writeOAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body exceeds 8MB limit.")
		return
	}

	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON request body: "+err.Error())
		return
	}

	tools, enableSearch := parseResponsesTools(req.Tools)
	toolChoice := normalizeResponsesToolChoice(req.ToolChoice)

	var msgs []oaiMessage
	if strings.TrimSpace(req.Instructions) != "" {
		msgs = append(msgs, oaiMessage{
			Role:    "system",
			Content: oaiTextContent(req.Instructions),
		})
	}
	if prev, ok := s.respHistory.get(req.PreviousResponseID); ok {
		for _, pm := range prev.messages {
			// Avoid duplicating top-level system prompt if Instructions was provided again.
			if strings.EqualFold(pm.Role, "system") && strings.TrimSpace(req.Instructions) != "" {
				continue
			}
			msgs = append(msgs, pm)
		}
	}

	inputMsgs, err := parseResponsesInput(req.Input)
	if err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid input: "+err.Error())
		return
	}
	msgs = append(msgs, inputMsgs...)
	if len(msgs) == 0 {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "input must not be empty.")
		return
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	}

	spec := resolveServeModel(req.Model, s.opts)
	if enableSearch {
		spec.Search = true
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" && !strings.EqualFold(req.Reasoning.Effort, "none") {
		spec.Thinking = true
	}

	oaiReq := oaiChatRequest{
		Model:      spec.ID,
		Messages:   msgs,
		Tools:      tools,
		ToolChoice: toolChoice,
		Stream:     req.Stream,
	}
	mode, _ := parseToolChoice(toolChoice)
	parseTools := len(tools) > 0 && mode != "none"

	respID := "resp_" + shortRandomHex(12)
	created := time.Now().Unix()

	if req.Stream {
		s.serveResponsesStreaming(w, r, oaiReq, spec, parseTools, respID, created)
		return
	}
	s.serveResponsesNonStreaming(w, r, oaiReq, spec, parseTools, respID, created)
}

func parseResponsesTools(rawTools []json.RawMessage) ([]oaiTool, bool) {
	var tools []oaiTool
	enableSearch := false
	for _, raw := range rawTools {
		var t struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Function    *oaiToolFunction `json:"function,omitempty"`
		}
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		if strings.HasPrefix(strings.ToLower(t.Type), "web_search") {
			enableSearch = true
			continue
		}
		fn := oaiToolFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		}
		if t.Function != nil && t.Function.Name != "" {
			fn = *t.Function
		}
		if fn.Name == "" {
			continue
		}
		tools = append(tools, oaiTool{Type: "function", Function: fn})
	}
	return tools, enableSearch
}

func normalizeResponsesToolChoice(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || strings.HasPrefix(trimmed, `"`) {
		return raw
	}
	var obj struct {
		Type     string           `json:"type"`
		Name     string           `json:"name"`
		Function *oaiToolFunction `json:"function,omitempty"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.Type == "function" && obj.Name != "" && obj.Function == nil {
			wrapped, _ := json.Marshal(map[string]any{
				"type":     "function",
				"function": map[string]string{"name": obj.Name},
			})
			return wrapped
		}
	}
	return raw
}

func parseResponsesInput(raw json.RawMessage) ([]oaiMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []oaiMessage{{Role: "user", Content: oaiTextContent(s)}}, nil
	}
	if !strings.HasPrefix(trimmed, "[") {
		return nil, fmt.Errorf("input must be a string or array of items")
	}

	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}

	var msgs []oaiMessage
	for _, itemRaw := range items {
		var item struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   oaiTextContent  `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Output    oaiTextContent  `json:"output"`
		}
		if err := json.Unmarshal(itemRaw, &item); err != nil {
			return nil, err
		}
		switch item.Type {
		case "function_call":
			tc := oaiToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: oaiCallFunction{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			}
			// Merge consecutive function_calls into the preceding assistant message if possible.
			if len(msgs) > 0 && msgs[len(msgs)-1].Role == "assistant" {
				msgs[len(msgs)-1].ToolCalls = append(msgs[len(msgs)-1].ToolCalls, tc)
			} else {
				msgs = append(msgs, oaiMessage{Role: "assistant", ToolCalls: []oaiToolCall{tc}})
			}
		case "function_call_output":
			msgs = append(msgs, oaiMessage{
				Role:       "tool",
				ToolCallID: item.CallID,
				Content:    item.Output,
			})
		default:
			role := strings.TrimSpace(item.Role)
			if role == "" {
				role = "user"
			}
			msgs = append(msgs, oaiMessage{
				Role:    role,
				Content: item.Content,
			})
		}
	}
	return msgs, nil
}

func buildResponsesOutputItems(msgID string, content string, thinking string, toolCalls []oaiToolCall) ([]any, string) {
	var output []any
	if thinking != "" {
		output = append(output, map[string]any{
			"id":      "rs_" + shortRandomHex(8),
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": thinking}},
		})
	}
	if content != "" || len(toolCalls) == 0 {
		output = append(output, map[string]any{
			"id":     msgID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        content,
				"annotations": []any{},
			}},
		})
	}
	for _, tc := range toolCalls {
		output = append(output, map[string]any{
			"id":        "fc_" + shortRandomHex(8),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
		})
	}
	return output, content
}

func (s *serveServer) saveResponseHistory(respID string, req oaiChatRequest, content string, toolCalls []oaiToolCall) {
	all := make([]oaiMessage, len(req.Messages)+1)
	copy(all, req.Messages)
	all[len(req.Messages)] = oaiMessage{
		Role:      "assistant",
		Content:   oaiTextContent(content),
		ToolCalls: toolCalls,
	}
	s.respHistory.put(respID, responsesState{messages: all})
}

func (s *serveServer) serveResponsesNonStreaming(
	w http.ResponseWriter,
	r *http.Request,
	req oaiChatRequest,
	spec serveModelSpec,
	parseTools bool,
	respID string,
	created int64,
) {
	var contentBuf, thinkBuf strings.Builder
	var toolCalls []oaiToolCall
	parser := newToolStreamParser(parseTools, nil, func(t string) {
		contentBuf.WriteString(t)
	}, func(tc oaiToolCall) {
		toolCalls = append(toolCalls, tc)
	})

	sessionID, msgID, err := s.runUpstreamCompletion(r.Context(), req, spec, nil, func(t string) {
		thinkBuf.WriteString(t)
	}, func(t string) {
		parser.Feed(t)
	})
	if err != nil {
		writeUpstreamOAIError(w, err)
		return
	}
	parser.Finish()

	contentStr := contentBuf.String()
	s.cacheTurn(req, spec, sessionID, msgID, contentStr, toolCalls)
	s.saveResponseHistory(respID, req, contentStr, toolCalls)

	outMsgID := "msg_" + shortRandomHex(10)
	output, outputText := buildResponsesOutputItems(outMsgID, contentStr, thinkBuf.String(), toolCalls)
	promptTok := max(1, len(buildFullPrompt(req))/4)
	compTok := max(1, (len(contentStr)+thinkBuf.Len())/4)

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          respID,
		"object":      "response",
		"created_at":  created,
		"status":      "completed",
		"model":       spec.ID,
		"output":      output,
		"output_text": outputText,
		"usage": map[string]int{
			"input_tokens":  promptTok,
			"output_tokens": compTok,
			"total_tokens":  promptTok + compTok,
		},
	})
}

func (s *serveServer) serveResponsesStreaming(
	w http.ResponseWriter,
	r *http.Request,
	req oaiChatRequest,
	spec serveModelSpec,
	parseTools bool,
	respID string,
	created int64,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOAIError(w, http.StatusInternalServerError, "server_error", "Streaming unsupported by response writer.")
		return
	}

	var headersSent bool
	seq := 0
	emitEvent := func(eventName string, payload map[string]any) {
		payload["type"] = eventName
		payload["sequence_number"] = seq
		seq++
		raw, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, raw)
		flusher.Flush()
	}

	ensureHeaders := func() {
		if headersSent {
			return
		}
		headersSent = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		baseResp := map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": created,
			"status":     "in_progress",
			"model":      spec.ID,
			"output":     []any{},
		}
		emitEvent("response.created", map[string]any{"response": baseResp})
		emitEvent("response.in_progress", map[string]any{"response": baseResp})
	}

	outIdx := 0
	msgOpen := false
	msgItemID := "msg_" + shortRandomHex(10)
	msgOutIdx := 0

	ensureMsgOpen := func() {
		if msgOpen {
			return
		}
		ensureHeaders()
		msgOpen = true
		msgOutIdx = outIdx
		outIdx++
		emitEvent("response.output_item.added", map[string]any{
			"output_index": msgOutIdx,
			"item": map[string]any{
				"id":      msgItemID,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		})
		emitEvent("response.content_part.added", map[string]any{
			"item_id":       msgItemID,
			"output_index":  msgOutIdx,
			"content_index": 0,
			"part": map[string]any{
				"type":        "output_text",
				"text":        "",
				"annotations": []any{},
			},
		})
	}

	var contentBuf, thinkBuf strings.Builder
	var toolCalls []oaiToolCall

	parser := newToolStreamParser(parseTools, nil, func(t string) {
		ensureMsgOpen()
		contentBuf.WriteString(t)
		emitEvent("response.output_text.delta", map[string]any{
			"item_id":       msgItemID,
			"output_index":  msgOutIdx,
			"content_index": 0,
			"delta":         t,
		})
	}, func(tc oaiToolCall) {
		ensureHeaders()
		if msgOpen {
			text := contentBuf.String()
			emitEvent("response.output_text.done", map[string]any{
				"item_id":       msgItemID,
				"output_index":  msgOutIdx,
				"content_index": 0,
				"text":          text,
			})
			emitEvent("response.output_item.done", map[string]any{
				"output_index": msgOutIdx,
				"item": map[string]any{
					"id":     msgItemID,
					"type":   "message",
					"status": "completed",
					"role":   "assistant",
					"content": []any{map[string]any{
						"type":        "output_text",
						"text":        text,
						"annotations": []any{},
					}},
				},
			})
			msgOpen = false
		}
		toolCalls = append(toolCalls, tc)
		fcID := "fc_" + shortRandomHex(8)
		idx := outIdx
		outIdx++
		emitEvent("response.output_item.added", map[string]any{
			"output_index": idx,
			"item": map[string]any{
				"id":        fcID,
				"type":      "function_call",
				"status":    "in_progress",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": "",
			},
		})
		emitEvent("response.function_call_arguments.delta", map[string]any{
			"item_id":      fcID,
			"output_index": idx,
			"delta":        tc.Function.Arguments,
		})
		emitEvent("response.function_call_arguments.done", map[string]any{
			"item_id":      fcID,
			"output_index": idx,
			"arguments":    tc.Function.Arguments,
		})
		emitEvent("response.output_item.done", map[string]any{
			"output_index": idx,
			"item": map[string]any{
				"id":        fcID,
				"type":      "function_call",
				"status":    "completed",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			},
		})
	})

	sessionID, msgID, err := s.runUpstreamCompletion(r.Context(), req, spec, nil, func(t string) {
		ensureHeaders()
		thinkBuf.WriteString(t)
		emitEvent("response.reasoning_summary_text.delta", map[string]any{
			"delta": t,
		})
	}, func(t string) {
		parser.Feed(t)
	})
	if err != nil {
		if !headersSent {
			writeUpstreamOAIError(w, err)
			return
		}
		emitEvent("error", map[string]any{"message": friendlyError(err)})
		return
	}

	parser.Finish()
	ensureHeaders()
	if msgOpen {
		text := contentBuf.String()
		emitEvent("response.output_text.done", map[string]any{
			"item_id":       msgItemID,
			"output_index":  msgOutIdx,
			"content_index": 0,
			"text":          text,
		})
		emitEvent("response.output_item.done", map[string]any{
			"output_index": msgOutIdx,
			"item": map[string]any{
				"id":     msgItemID,
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
				"content": []any{map[string]any{
					"type":        "output_text",
					"text":        text,
					"annotations": []any{},
				}},
			},
		})
	}

	contentStr := contentBuf.String()
	s.cacheTurn(req, spec, sessionID, msgID, contentStr, toolCalls)
	s.saveResponseHistory(respID, req, contentStr, toolCalls)

	output, outputText := buildResponsesOutputItems(msgItemID, contentStr, thinkBuf.String(), toolCalls)
	promptTok := max(1, len(buildFullPrompt(req))/4)
	compTok := max(1, (len(contentStr)+thinkBuf.Len())/4)

	emitEvent("response.completed", map[string]any{
		"response": map[string]any{
			"id":          respID,
			"object":      "response",
			"created_at":  created,
			"status":      "completed",
			"model":       spec.ID,
			"output":      output,
			"output_text": outputText,
			"usage": map[string]int{
				"input_tokens":  promptTok,
				"output_tokens": compTok,
				"total_tokens":  promptTok + compTok,
			},
		},
	})
}

// -----------------------------------------------------------------------------
// Anthropic Messages API (POST /v1/messages) — used by Claude Code & Anthropic SDK
// -----------------------------------------------------------------------------

type anthropicRequest struct {
	Model         string            `json:"model"`
	System        oaiTextContent    `json:"system,omitempty"`
	Messages      []json.RawMessage `json:"messages"`
	Tools         []json.RawMessage `json:"tools,omitempty"`
	ToolChoice    json.RawMessage   `json:"tool_choice,omitempty"`
	Stream        bool              `json:"stream"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	Thinking      *struct {
		Type string `json:"type"`
	} `json:"thinking,omitempty"`
}

func (s *serveServer) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxServeRequestBytes+1))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body.")
		return
	}
	if len(body) > maxServeRequestBytes {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body exceeds 8MB limit.")
		return
	}

	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON request body: "+err.Error())
		return
	}

	msgs, err := convertAnthropicMessages(string(req.System), req.Messages)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid messages: "+err.Error())
		return
	}
	if len(msgs) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages must not be empty.")
		return
	}

	tools, enableSearch := convertAnthropicTools(req.Tools)
	toolChoice := convertAnthropicToolChoice(req.ToolChoice)

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	}

	spec := resolveServeModel(req.Model, s.opts)
	if enableSearch {
		spec.Search = true
	}
	if req.Thinking != nil && strings.EqualFold(req.Thinking.Type, "enabled") {
		spec.Thinking = true
	}

	oaiReq := oaiChatRequest{
		Model:      spec.ID,
		Messages:   msgs,
		Tools:      tools,
		ToolChoice: toolChoice,
		Stream:     req.Stream,
	}
	mode, _ := parseToolChoice(toolChoice)
	parseTools := len(tools) > 0 && mode != "none"
	msgOutID := "msg_" + shortRandomHex(12)

	if req.Stream {
		s.serveAnthropicStreaming(w, r, oaiReq, spec, req.StopSequences, parseTools, msgOutID)
		return
	}
	s.serveAnthropicNonStreaming(w, r, oaiReq, spec, req.StopSequences, parseTools, msgOutID)
}

func convertAnthropicTools(rawTools []json.RawMessage) ([]oaiTool, bool) {
	var tools []oaiTool
	enableSearch := false
	for _, raw := range rawTools {
		var t struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		if strings.HasPrefix(strings.ToLower(t.Type), "web_search") {
			enableSearch = true
			continue
		}
		if t.Name == "" {
			continue
		}
		params := t.InputSchema
		if len(bytesTrimSpace(params)) == 0 {
			params = t.Parameters
		}
		tools = append(tools, oaiTool{
			Type: "function",
			Function: oaiToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return tools, enableSearch
}

func convertAnthropicToolChoice(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch strings.ToLower(tc.Type) {
	case "none":
		return json.RawMessage(`"none"`)
	case "any":
		return json.RawMessage(`"required"`)
	case "tool":
		if tc.Name != "" {
			b, _ := json.Marshal(map[string]any{
				"type":     "function",
				"function": map[string]string{"name": tc.Name},
			})
			return b
		}
	}
	return json.RawMessage(`"auto"`)
}

func convertAnthropicMessages(system string, rawMsgs []json.RawMessage) ([]oaiMessage, error) {
	var out []oaiMessage
	if strings.TrimSpace(system) != "" {
		out = append(out, oaiMessage{
			Role:    "system",
			Content: oaiTextContent(system),
		})
	}
	for _, raw := range rawMsgs {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		trimmed := strings.TrimSpace(string(m.Content))
		if strings.HasPrefix(trimmed, `"`) || trimmed == "null" || trimmed == "" {
			var s string
			if trimmed != "" && trimmed != "null" {
				if err := json.Unmarshal(m.Content, &s); err != nil {
					return nil, err
				}
			}
			out = append(out, oaiMessage{Role: role, Content: oaiTextContent(s)})
			continue
		}

		var blocks []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   oaiTextContent  `json:"content"`
		}
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return nil, err
		}

		var textParts []string
		var toolCalls []oaiToolCall
		var toolResults []oaiMessage

		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text != "" {
					textParts = append(textParts, b.Text)
				}
			case "tool_use":
				toolCalls = append(toolCalls, oaiToolCall{
					ID:   b.ID,
					Type: "function",
					Function: oaiCallFunction{
						Name:      b.Name,
						Arguments: normalizeToolArgsJSON(b.Input),
					},
				})
			case "tool_result":
				toolResults = append(toolResults, oaiMessage{
					Role:       "tool",
					ToolCallID: b.ToolUseID,
					Content:    b.Content,
				})
			}
		}

		if len(textParts) > 0 || len(toolCalls) > 0 {
			out = append(out, oaiMessage{
				Role:      role,
				Content:   oaiTextContent(strings.Join(textParts, "\n")),
				ToolCalls: toolCalls,
			})
		}
		out = append(out, toolResults...)
	}
	return out, nil
}

func (s *serveServer) serveAnthropicNonStreaming(
	w http.ResponseWriter,
	r *http.Request,
	req oaiChatRequest,
	spec serveModelSpec,
	stops []string,
	parseTools bool,
	msgOutID string,
) {
	var contentBuf, thinkBuf strings.Builder
	var toolCalls []oaiToolCall

	parser := newToolStreamParser(parseTools, stops, func(t string) {
		contentBuf.WriteString(t)
	}, func(tc oaiToolCall) {
		toolCalls = append(toolCalls, tc)
	})

	runCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sessionID, msgID, err := s.runUpstreamCompletion(runCtx, req, spec, nil, func(t string) {
		thinkBuf.WriteString(t)
	}, func(t string) {
		if parser.Feed(t) {
			cancel()
		}
	})
	if err != nil && !(parser.stopped && errors.Is(err, context.Canceled)) {
		writeAnthropicUpstreamError(w, err)
		return
	}
	parser.Finish()

	contentStr := contentBuf.String()
	s.cacheTurn(req, spec, sessionID, msgID, contentStr, toolCalls)

	var blocks []any
	if thinkBuf.Len() > 0 {
		blocks = append(blocks, map[string]any{
			"type":     "thinking",
			"thinking": thinkBuf.String(),
		})
	}
	if contentStr != "" || (len(toolCalls) == 0 && thinkBuf.Len() == 0) {
		blocks = append(blocks, map[string]any{
			"type": "text",
			"text": contentStr,
		})
	}
	for _, tc := range toolCalls {
		var inputObj any = map[string]any{}
		if json.Unmarshal([]byte(tc.Function.Arguments), &inputObj) != nil {
			inputObj = map[string]any{}
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Function.Name,
			"input": inputObj,
		})
	}

	stopReason := "end_turn"
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	} else if parser.stopped {
		stopReason = "stop_sequence"
	}

	promptTok := max(1, len(buildFullPrompt(req))/4)
	compTok := max(1, (len(contentStr)+thinkBuf.Len())/4)

	writeJSON(w, http.StatusOK, map[string]any{
		"id":            msgOutID,
		"type":          "message",
		"role":          "assistant",
		"model":         spec.ID,
		"content":       blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  promptTok,
			"output_tokens": compTok,
		},
	})
}

func (s *serveServer) serveAnthropicStreaming(
	w http.ResponseWriter,
	r *http.Request,
	req oaiChatRequest,
	spec serveModelSpec,
	stops []string,
	parseTools bool,
	msgOutID string,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "Streaming unsupported by response writer.")
		return
	}

	emit := func(eventName string, payload any) {
		raw, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, raw)
		flusher.Flush()
	}

	promptTok := max(1, len(buildFullPrompt(req))/4)
	var headersSent bool
	ensureHeaders := func() {
		if headersSent {
			return
		}
		headersSent = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		emit("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            msgOutID,
				"type":          "message",
				"role":          "assistant",
				"model":         spec.ID,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage": map[string]int{
					"input_tokens":  promptTok,
					"output_tokens": 0,
				},
			},
		})
	}

	blockIdx := 0
	openKind := "" // "", "thinking", "text"
	openIdx := 0

	closeOpenBlock := func() {
		if openKind == "" {
			return
		}
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": openIdx,
		})
		openKind = ""
	}

	var contentBuf, thinkBuf strings.Builder
	var toolCalls []oaiToolCall

	parser := newToolStreamParser(parseTools, stops, func(t string) {
		ensureHeaders()
		if openKind != "text" {
			closeOpenBlock()
			openIdx = blockIdx
			blockIdx++
			openKind = "text"
			emit("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         openIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		}
		contentBuf.WriteString(t)
		emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": openIdx,
			"delta": map[string]any{"type": "text_delta", "text": t},
		})
	}, func(tc oaiToolCall) {
		ensureHeaders()
		closeOpenBlock()
		toolCalls = append(toolCalls, tc)
		idx := blockIdx
		blockIdx++
		emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": map[string]any{},
			},
		})
		emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
		})
		emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		})
	})

	runCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sessionID, msgID, err := s.runUpstreamCompletion(runCtx, req, spec, nil, func(t string) {
		ensureHeaders()
		if openKind != "thinking" {
			closeOpenBlock()
			openIdx = blockIdx
			blockIdx++
			openKind = "thinking"
			emit("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         openIdx,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			})
		}
		thinkBuf.WriteString(t)
		emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": openIdx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": t},
		})
	}, func(t string) {
		if parser.Feed(t) {
			cancel()
		}
	})

	if err != nil && !(parser.stopped && errors.Is(err, context.Canceled)) {
		if !headersSent {
			writeAnthropicUpstreamError(w, err)
			return
		}
		emit("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": friendlyError(err)},
		})
		return
	}

	parser.Finish()
	ensureHeaders()
	closeOpenBlock()

	contentStr := contentBuf.String()
	s.cacheTurn(req, spec, sessionID, msgID, contentStr, toolCalls)

	stopReason := "end_turn"
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	} else if parser.stopped {
		stopReason = "stop_sequence"
	}
	compTok := max(1, (len(contentStr)+thinkBuf.Len())/4)
	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{"output_tokens": compTok},
	})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

func writeAnthropicUpstreamError(w http.ResponseWriter, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		status := apiErr.Status
		if status == 0 || status == http.StatusOK {
			status = http.StatusBadGateway
			if apiErr.Code == 40303 {
				status = http.StatusForbidden
			}
		}
		errType := "api_error"
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			errType = "authentication_error"
		case http.StatusTooManyRequests:
			errType = "rate_limit_error"
		}
		writeAnthropicError(w, status, errType, friendlyError(err))
		return
	}
	writeAnthropicError(w, http.StatusBadGateway, "api_error", friendlyError(err))
}
