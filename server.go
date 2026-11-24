package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultServeAddr        = "127.0.0.1:8080"
	defaultServeConcurrency = 2
	maxServeRequestBytes    = 8 << 20 // 8 MiB
	maxSessionCacheEntries  = 64
	toolCallOpenTag   = "<tool_call>"
	toolCallCloseTag  = "</tool_call>"
	dsmlOpenFullwidth = "<｜DSML｜"
	dsmlOpenDouble    = "<｜｜DSML｜｜"
	dsmlOpenASCII     = "<|DSML|"
)

var (
	dsmlOpenRe        = regexp.MustCompile(`<[｜|]+DSML[｜|]+`)
	dsmlInvokeRe      = regexp.MustCompile(`(?s)<[｜|]+DSML[｜|]+\s*invoke\s+name="([^"]+)"\s*>(.*?)</[｜|]+DSML[｜|]+\s*invoke\s*>`)
	dsmlParamRe       = regexp.MustCompile(`(?s)<[｜|]+DSML[｜|]+\s*parameter\s+name="([^"]+)"(?:\s+string="(true|false)")?\s*>(.*?)</[｜|]+DSML[｜|]+\s*parameter\s*>`)
	dsmlCallsCloseRe  = regexp.MustCompile(`</[｜|]+DSML[｜|]+\s*(?:function_)?calls\s*>`)
	dsmlInvokeCloseRe = regexp.MustCompile(`</[｜|]+DSML[｜|]+\s*invoke\s*>`)
)

type serveModelSpec struct {
	ID       string
	Thinking bool
	Search   bool
}

var defaultServeModels = []serveModelSpec{
	{ID: "deepseek-chat", Thinking: false, Search: false},
	{ID: "deepseek-reasoner", Thinking: true, Search: false},
	{ID: "deepseek-chat-search", Thinking: false, Search: true},
	{ID: "deepseek-reasoner-search", Thinking: true, Search: true},
}

// normalizeServeAddr converts a port ("8080") or host:port into a validated listen address.
// Bare ports bind to loopback (127.0.0.1) by default.
func normalizeServeAddr(raw string) (string, error) {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return defaultServeAddr, nil
	}
	if isAllDigits(addr) {
		port, err := strconv.Atoi(addr)
		if err != nil || port < 1 || port > 65535 {
			return "", fmt.Errorf("invalid serve port %q", raw)
		}
		return "127.0.0.1:" + addr, nil
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid serve address %q: %w", raw, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return "", fmt.Errorf("invalid serve port %q", portStr)
	}
	return net.JoinHostPort(host, portStr), nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func resolveServeModel(model string, opts stealthOptions) serveModelSpec {
	name := strings.TrimSpace(model)
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	lower := strings.ToLower(name)
	for _, spec := range defaultServeModels {
		if lower == spec.ID {
			if opts.noSearch {
				spec.Search = false
			}
			return spec
		}
	}

	thinking := opts.think
	if strings.Contains(lower, "reasoner") || strings.Contains(lower, "think") || strings.Contains(lower, "r1") {
		thinking = true
	}
	search := !opts.noSearch
	if strings.Contains(lower, "-search") {
		search = true
	}
	if model == "" {
		model = "deepseek-chat"
	}
	return serveModelSpec{ID: model, Thinking: thinking, Search: search}
}

// -----------------------------------------------------------------------------
// OpenAI API wire types
// -----------------------------------------------------------------------------

type oaiChatRequest struct {
	Model         string            `json:"model"`
	Messages      []oaiMessage      `json:"messages"`
	Tools         []oaiTool         `json:"tools,omitempty"`
	Functions     []oaiToolFunction `json:"functions,omitempty"`
	ToolChoice    json.RawMessage   `json:"tool_choice,omitempty"`
	FunctionCall  json.RawMessage   `json:"function_call,omitempty"`
	Stream        bool              `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	WebSearchOptions json.RawMessage `json:"web_search_options,omitempty"`
}

type oaiMessage struct {
	Role             string          `json:"role"`
	Content          oaiTextContent  `json:"content"`
	Name             string          `json:"name,omitempty"`
	ToolCalls        []oaiToolCall   `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	RawToolCall      json.RawMessage `json:"-"`
}

// oaiTextContent accepts OpenAI string content, null, or arrays of content parts.
type oaiTextContent string

func (c *oaiTextContent) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*c = ""
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*c = oaiTextContent(s)
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var parts []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url,omitempty"`
		}
		if err := json.Unmarshal(data, &parts); err != nil {
			return err
		}
		var b strings.Builder
		for _, part := range parts {
			switch part.Type {
			case "text", "input_text", "output_text":
				if b.Len() > 0 && part.Text != "" {
					b.WriteByte('\n')
				}
				b.WriteString(part.Text)
			case "image_url":
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString("[image omitted]")
			default:
				if part.Text != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(part.Text)
				}
			}
		}
		*c = oaiTextContent(b.String())
		return nil
	}
	return fmt.Errorf("unsupported message content format")
}

type oaiTool struct {
	Type     string          `json:"type"`
	Function oaiToolFunction `json:"function"`
}

type oaiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type oaiToolCall struct {
	Index    *int            `json:"index,omitempty"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Function oaiCallFunction `json:"function"`
}

type oaiCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type oaiChatResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage"`
}

type oaiChoice struct {
	Index        int               `json:"index"`
	Message      oaiResponseMsg    `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type oaiResponseMsg struct {
	Role             string        `json:"role"`
	Content          *string       `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiStreamChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []oaiStreamChoice `json:"choices"`
	Usage   *oaiUsage         `json:"usage,omitempty"`
}

type oaiStreamChoice struct {
	Index        int            `json:"index"`
	Delta        oaiStreamDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type oaiStreamDelta struct {
	Role             string        `json:"role,omitempty"`
	Content          *string       `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
}

// -----------------------------------------------------------------------------
// Prompt serialization & tool protocol
// -----------------------------------------------------------------------------

func parseStopSequences(raw json.RawMessage) []string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return []string{s}
		}
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		out := make([]string, 0, len(list))
		for _, item := range list {
			if item != "" {
				out = append(out, item)
			}
		}
		return out
	}
	return nil
}

func parseToolChoice(raw json.RawMessage) (mode string, forcedName string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "auto", ""
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "none", "required", "auto":
				return strings.ToLower(strings.TrimSpace(s)), ""
			}
		}
		return "auto", ""
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Function.Name != "" {
		return "function", obj.Function.Name
	}
	return "auto", ""
}

func buildFullPrompt(req oaiChatRequest) string {
	var systemParts []string
	var turns []oaiMessage
	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role == "system" || role == "developer" {
			if txt := strings.TrimSpace(string(msg.Content)); txt != "" {
				systemParts = append(systemParts, txt)
			}
			continue
		}
		turns = append(turns, msg)
	}

	var b strings.Builder
	if len(systemParts) > 0 {
		b.WriteString("### System Instructions\n")
		b.WriteString(strings.Join(systemParts, "\n\n"))
		b.WriteString("\n\n")
	}

	mode, forcedName := parseToolChoice(req.ToolChoice)
	if len(req.Tools) > 0 && mode != "none" {
		b.WriteString(formatToolsInstruction(req.Tools, mode, forcedName))
		b.WriteString("\n\n")
	} else if mode == "none" && len(req.Tools) > 0 {
		b.WriteString("### Tool Calling Disabled\nDo not call any tools. Respond directly in plain text or Markdown.\n\n")
	}

	// Optimize simple single-turn plain prompt when there is no system/tool scaffolding.
	if len(systemParts) == 0 && (len(req.Tools) == 0 || mode == "none") && len(turns) == 1 && strings.EqualFold(turns[0].Role, "user") {
		return strings.TrimSpace(string(turns[0].Content))
	}

	b.WriteString(formatMessageSlice(turns))
	if len(turns) > 0 && !strings.EqualFold(turns[len(turns)-1].Role, "assistant") {
		b.WriteString("\n### Assistant\n")
	}
	return strings.TrimSpace(b.String())
}

func formatToolsInstruction(tools []oaiTool, mode, forcedName string) string {
	var b strings.Builder
	b.WriteString("### Available Tools\n")
	b.WriteString("You can invoke tools by outputting one or more XML blocks with this exact format:\n")
	b.WriteString("<tool_call>\n{\"name\": \"<tool_name>\", \"arguments\": {<json_arguments>}}\n</tool_call>\n\n")
	b.WriteString("Rules for tool calls:\n")
	b.WriteString("- Output valid JSON inside <tool_call>...</tool_call> using the exact tool name and exact parameter property names from its schema.\n")
	b.WriteString("- Do NOT wrap <tool_call> blocks inside Markdown code fences (no ```).\n")
	b.WriteString("- You may emit multiple <tool_call> blocks in one turn if needed, then stop and wait for <tool_result> responses.\n")
	switch mode {
	case "required":
		b.WriteString("- You MUST invoke at least one tool in your response.\n")
	case "function":
		b.WriteString(fmt.Sprintf("- You MUST invoke the tool %q in your response.\n", forcedName))
	}
	b.WriteString("\nTool definitions:\n")
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		b.WriteString(fmt.Sprintf("- `%s`", t.Function.Name))
		if desc := strings.TrimSpace(t.Function.Description); desc != "" {
			b.WriteString(": " + desc)
		}
		b.WriteByte('\n')
		if params := strings.TrimSpace(string(t.Function.Parameters)); params != "" && params != "null" {
			var compact bytes.Buffer
			if json.Compact(&compact, []byte(params)) == nil {
				b.WriteString("  parameters: " + compact.String() + "\n")
			} else {
				b.WriteString("  parameters: " + params + "\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatMessageSlice(msgs []oaiMessage) string {
	var b strings.Builder
	for i, msg := range msgs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "user":
			b.WriteString("### User\n")
			b.WriteString(string(msg.Content))
		case "assistant":
			b.WriteString("### Assistant\n")
			wroteBody := false
			if content := strings.TrimSpace(string(msg.Content)); content != "" {
				b.WriteString(content)
				wroteBody = true
			}
			for _, tc := range msg.ToolCalls {
				if wroteBody {
					b.WriteByte('\n')
				}
				wroteBody = true
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					encoded, _ := json.Marshal(args)
					args = string(encoded)
				}
				nameJSON, _ := json.Marshal(tc.Function.Name)
				b.WriteString(fmt.Sprintf("%s\n{\"name\":%s,\"arguments\":%s}\n%s",
					toolCallOpenTag, string(nameJSON), args, toolCallCloseTag))
			}
		case "tool":
			b.WriteString(fmt.Sprintf("<tool_result id=%q", msg.ToolCallID))
			if msg.Name != "" {
				b.WriteString(fmt.Sprintf(" name=%q", msg.Name))
			}
			b.WriteString(">\n")
			b.WriteString(string(msg.Content))
			b.WriteString("\n</tool_result>")
		default:
			b.WriteString("### " + role + "\n" + string(msg.Content))
		}
	}
	return b.String()
}

// -----------------------------------------------------------------------------
// Streaming tool-call & stop-sequence parser
// -----------------------------------------------------------------------------

type toolStreamParser struct {
	parseTools bool
	stops      []string
	maxStopLen int

	buf      strings.Builder
	inTool   bool
	inDSML   bool
	toolBuf  strings.Builder
	stopped  bool
	sawTools bool

	onText     func(string)
	onToolCall func(oaiToolCall)
}

func newToolStreamParser(parseTools bool, stops []string, onText func(string), onToolCall func(oaiToolCall)) *toolStreamParser {
	maxStop := 0
	for _, s := range stops {
		if len(s) > maxStop {
			maxStop = len(s)
		}
	}
	return &toolStreamParser{
		parseTools: parseTools,
		stops:      stops,
		maxStopLen: maxStop,
		onText:     onText,
		onToolCall: onToolCall,
	}
}

func (p *toolStreamParser) emitText(s string) {
	if s == "" || p.stopped {
		return
	}
	if p.onText != nil {
		p.onText(s)
	}
}

func (p *toolStreamParser) Feed(chunk string) bool {
	if chunk == "" || p.stopped {
		return p.stopped
	}
	if p.inTool || p.inDSML {
		p.toolBuf.WriteString(chunk)
	} else {
		p.buf.WriteString(chunk)
	}
	p.drain(false)
	return p.stopped
}

func (p *toolStreamParser) Finish() {
	if p.stopped {
		return
	}
	p.drain(true)
}

func (p *toolStreamParser) drain(eof bool) {
	for !p.stopped {
		if p.inTool {
			s := p.toolBuf.String()
			end := strings.Index(s, toolCallCloseTag)
			if end < 0 {
				if !eof {
					return
				}
				if tc, ok := parseToolCallPayload(s); ok {
					p.sawTools = true
					if p.onToolCall != nil {
						p.onToolCall(tc)
					}
				} else {
					p.emitText(toolCallOpenTag + s)
				}
				p.toolBuf.Reset()
				p.inTool = false
				return
			}

			body := s[:end]
			rest := s[end+len(toolCallCloseTag):]
			p.toolBuf.Reset()
			p.inTool = false

			if tc, ok := parseToolCallPayload(body); ok {
				p.sawTools = true
				if p.onToolCall != nil {
					p.onToolCall(tc)
				}
			} else {
				p.emitText(toolCallOpenTag + body + toolCallCloseTag)
			}
			if rest != "" {
				p.buf.WriteString(rest)
			}
			continue
		}

		if p.inDSML {
			s := p.toolBuf.String()
			endIdx, complete := findDSMLClose(s)
			if !complete {
				if !eof {
					return
				}
				if calls := parseDSMLToolCalls(s); len(calls) > 0 {
					p.sawTools = true
					for _, tc := range calls {
						if p.onToolCall != nil {
							p.onToolCall(tc)
						}
					}
				} else {
					p.emitText(s)
				}
				p.toolBuf.Reset()
				p.inDSML = false
				return
			}

			block := s[:endIdx]
			rest := s[endIdx:]
			p.toolBuf.Reset()
			p.inDSML = false

			if calls := parseDSMLToolCalls(block); len(calls) > 0 {
				p.sawTools = true
				for _, tc := range calls {
					if p.onToolCall != nil {
						p.onToolCall(tc)
					}
				}
			} else {
				p.emitText(block)
			}
			if rest != "" {
				p.buf.WriteString(rest)
			}
			continue
		}

		s := p.buf.String()
		if s == "" {
			return
		}

		stopIdx, stopHit := earliestStopIndex(s, p.stops)
		tagIdx, tagKind, tagLen := -1, "", 0
		if p.parseTools {
			tagIdx, tagKind, tagLen = earliestToolTag(s)
		}

		if stopIdx >= 0 && (tagIdx < 0 || stopIdx <= tagIdx) {
			p.emitText(s[:stopIdx])
			p.buf.Reset()
			p.stopped = true
			_ = stopHit
			return
		}

		if tagIdx >= 0 {
			p.emitText(s[:tagIdx])
			if tagKind == "dsml" {
				rest := s[tagIdx:]
				p.buf.Reset()
				p.inDSML = true
				p.toolBuf.WriteString(rest)
			} else {
				rest := s[tagIdx+tagLen:]
				p.buf.Reset()
				p.inTool = true
				p.toolBuf.WriteString(rest)
			}
			continue
		}

		if eof {
			p.emitText(s)
			p.buf.Reset()
			return
		}

		hold := 0
		if p.parseTools {
			for _, prefixTag := range []string{toolCallOpenTag, dsmlOpenFullwidth, dsmlOpenDouble, dsmlOpenASCII} {
				if h := partialTagSuffixLen(s, prefixTag); h > hold {
					hold = h
				}
			}
		}
		if p.maxStopLen > 1 && p.maxStopLen-1 > hold {
			// Hold back enough bytes to detect a split stop sequence.
			if maxHold := p.maxStopLen - 1; maxHold > hold {
				hold = maxHold
			}
		}
		if hold >= len(s) {
			return
		}
		emitLen := len(s) - hold
		p.emitText(s[:emitLen])
		rem := s[emitLen:]
		p.buf.Reset()
		p.buf.WriteString(rem)
		return
	}
}

func earliestToolTag(s string) (idx int, kind string, length int) {
	best := -1
	if i := strings.Index(s, toolCallOpenTag); i >= 0 {
		best = i
		kind = "xmljson"
		length = len(toolCallOpenTag)
	}
	if loc := dsmlOpenRe.FindStringIndex(s); loc != nil && (best < 0 || loc[0] < best) {
		best = loc[0]
		kind = "dsml"
		length = loc[1] - loc[0]
	}
	return best, kind, length
}

func findDSMLClose(s string) (int, bool) {
	gt := strings.IndexByte(s, '>')
	if gt < 0 {
		return 0, false
	}
	header := strings.ToLower(s[:gt])
	if strings.Contains(header, "calls") {
		if loc := dsmlCallsCloseRe.FindStringIndex(s[gt+1:]); loc != nil {
			return gt + 1 + loc[1], true
		}
		return 0, false
	}
	if strings.Contains(header, "invoke") {
		if loc := dsmlInvokeCloseRe.FindStringIndex(s[gt+1:]); loc != nil {
			return gt + 1 + loc[1], true
		}
		return 0, false
	}
	return gt + 1, true
}

func parseDSMLToolCalls(block string) []oaiToolCall {
	matches := dsmlInvokeRe.FindAllStringSubmatch(block, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]oaiToolCall, 0, len(matches))
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		name := strings.TrimSpace(m[1])
		if name == "" {
			continue
		}
		argsMap := make(map[string]any)
		for _, pm := range dsmlParamRe.FindAllStringSubmatch(m[2], -1) {
			if len(pm) < 4 {
				continue
			}
			key := strings.TrimSpace(pm[1])
			if key == "" {
				continue
			}
			isString := pm[2] != "false"
			val := pm[3]
			if !isString {
				trimmedVal := strings.TrimSpace(val)
				if json.Valid([]byte(trimmedVal)) {
					argsMap[key] = json.RawMessage(trimmedVal)
					continue
				}
			}
			argsMap[key] = val
		}
		argsBytes, err := json.Marshal(argsMap)
		if err != nil {
			argsBytes = []byte("{}")
		}
		out = append(out, oaiToolCall{
			ID:   "call_" + shortRandomHex(12),
			Type: "function",
			Function: oaiCallFunction{
				Name:      name,
				Arguments: string(argsBytes),
			},
		})
	}
	return out
}

func earliestStopIndex(s string, stops []string) (int, string) {
	best := -1
	bestStop := ""
	for _, stop := range stops {
		if stop == "" {
			continue
		}
		if idx := strings.Index(s, stop); idx >= 0 && (best < 0 || idx < best) {
			best = idx
			bestStop = stop
		}
	}
	return best, bestStop
}

func partialTagSuffixLen(s, tag string) int {
	maxCheck := len(tag) - 1
	if len(s) < maxCheck {
		maxCheck = len(s)
	}
	for n := maxCheck; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}

func parseToolCallPayload(raw string) (oaiToolCall, bool) {
	cleaned := strings.TrimSpace(raw)
	if strings.HasPrefix(cleaned, "```") {
		if nl := strings.IndexByte(cleaned, '\n'); nl >= 0 {
			cleaned = cleaned[nl+1:]
		}
		cleaned = strings.TrimSuffix(strings.TrimSpace(cleaned), "```")
		cleaned = strings.TrimSpace(cleaned)
	}
	if cleaned == "" {
		return oaiToolCall{}, false
	}

	var payload struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(cleaned), &payload); err != nil || strings.TrimSpace(payload.Name) == "" {
		return oaiToolCall{}, false
	}

	argsRaw := payload.Arguments
	if len(bytesTrimSpace(argsRaw)) == 0 {
		argsRaw = payload.Input
	}
	argsStr := normalizeToolArgsJSON(argsRaw)

	return oaiToolCall{
		ID:   "call_" + shortRandomHex(12),
		Type: "function",
		Function: oaiCallFunction{
			Name:      strings.TrimSpace(payload.Name),
			Arguments: argsStr,
		},
	}, true
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func normalizeToolArgsJSON(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	if strings.HasPrefix(trimmed, `"`) {
		var inner string
		if json.Unmarshal(raw, &inner) == nil {
			innerTrim := strings.TrimSpace(inner)
			if innerTrim == "" {
				return "{}"
			}
			if json.Valid([]byte(innerTrim)) {
				var compact bytes.Buffer
				if json.Compact(&compact, []byte(innerTrim)) == nil {
					return compact.String()
				}
			}
			return inner
		}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(trimmed)) == nil {
		return compact.String()
	}
	return trimmed
}

func shortRandomHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// -----------------------------------------------------------------------------
// Prefix-matched session cache
// -----------------------------------------------------------------------------

type cachedSessionState struct {
	sessionID string
	parentID  int64
	modelID   string
	toolsHash string
	messages  []oaiMessage
}

type serveSessionCache struct {
	mu      sync.Mutex
	entries map[string]cachedSessionState
	order   []string
}

func newServeSessionCache() *serveSessionCache {
	return &serveSessionCache{
		entries: make(map[string]cachedSessionState),
	}
}

func (c *serveSessionCache) lookup(key string) (cachedSessionState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	val, ok := c.entries[key]
	return val, ok
}

func (c *serveSessionCache) store(key string, state cachedSessionState) {
	if key == "" || state.sessionID == "" || state.parentID == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists {
		if len(c.order) >= maxSessionCacheEntries {
			evict := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, evict)
		}
		c.order = append(c.order, key)
	}
	c.entries[key] = state
}

func (c *serveSessionCache) delete(key string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func canonicalJSONAny(raw json.RawMessage) any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var v any
	if json.Unmarshal([]byte(trimmed), &v) == nil {
		return v
	}
	return trimmed
}

func normalizeMessagesForHash(messages []oaiMessage) []oaiMessage {
	normMsgs := make([]oaiMessage, len(messages))
	for i, m := range messages {
		norm := oaiMessage{
			Role:       strings.ToLower(strings.TrimSpace(m.Role)),
			Content:    oaiTextContent(strings.TrimSpace(string(m.Content))),
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			norm.ToolCalls = make([]oaiToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				typ := tc.Type
				if typ == "" {
					typ = "function"
				}
				norm.ToolCalls[j] = oaiToolCall{
					ID:   tc.ID,
					Type: typ,
					Function: oaiCallFunction{
						Name:      tc.Function.Name,
						Arguments: normalizeToolArgsJSON(json.RawMessage(tc.Function.Arguments)),
					},
				}
			}
		}
		normMsgs[i] = norm
	}
	return normMsgs
}

func toolsFingerprint(tools []oaiTool, toolChoice json.RawMessage) string {
	type normTool struct {
		Type        string `json:"type"`
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Parameters  any    `json:"parameters,omitempty"`
	}
	var normTools []normTool
	if len(tools) > 0 {
		normTools = make([]normTool, len(tools))
		for i, t := range tools {
			normTools[i] = normTool{
				Type:        t.Type,
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  canonicalJSONAny(t.Function.Parameters),
			}
		}
	}
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode(struct {
		Tools      []normTool `json:"tools,omitempty"`
		ToolChoice any        `json:"tool_choice,omitempty"`
	}{
		Tools:      normTools,
		ToolChoice: canonicalJSONAny(toolChoice),
	})
	return hex.EncodeToString(h.Sum(nil))
}

func prefixFingerprint(modelID string, tools []oaiTool, toolChoice json.RawMessage, messages []oaiMessage) string {
	normMsgs := normalizeMessagesForHash(messages)
	tHash := toolsFingerprint(tools, toolChoice)

	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode(struct {
		Model     string       `json:"model"`
		ToolsHash string       `json:"tools_hash"`
		Messages  []oaiMessage `json:"messages"`
	}{
		Model:     modelID,
		ToolsHash: tHash,
		Messages:  normMsgs,
	})
	return hex.EncodeToString(h.Sum(nil))
}

// -----------------------------------------------------------------------------
// OpenAI HTTP Server
// -----------------------------------------------------------------------------

type serveServer struct {
	client      *APIClient
	opts        stealthOptions
	serveKey    string
	cache       *serveSessionCache
	respHistory *responsesHistoryStore
	sem         chan struct{}
	logger      *log.Logger
}

func newServeServer(client *APIClient, opts stealthOptions, logger *log.Logger) *serveServer {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	key := strings.TrimSpace(opts.serveKey)
	if key == "" {
		key = strings.TrimSpace(os.Getenv("DEEPSEEK_SERVE_KEY"))
	}
	return &serveServer{
		client:      client,
		opts:        opts,
		serveKey:    key,
		cache:       newServeSessionCache(),
		respHistory: newResponsesHistoryStore(),
		sem:         make(chan struct{}, defaultServeConcurrency),
		logger:      logger,
	}
}

func (s *serveServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	mux.HandleFunc("POST /responses", s.handleResponses)
	mux.HandleFunc("POST /v1/messages", s.handleAnthropicMessages)
	mux.HandleFunc("POST /messages", s.handleAnthropicMessages)
	mux.HandleFunc("/", s.handleNotFound)
	return s.withMiddleware(mux)
}

func (s *serveServer) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.serveKey != "" && r.URL.Path != "/health" && r.URL.Path != "/healthz" {
			if !s.authorized(r) {
				writeOAIError(w, http.StatusUnauthorized, "invalid_api_key", "Incorrect API key provided.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *serveServer) authorized(r *http.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
		token := strings.TrimSpace(auth[7:])
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.serveKey)) == 1 {
			return true
		}
	}
	if apiKey := strings.TrimSpace(r.Header.Get("X-API-Key")); apiKey != "" {
		if subtle.ConstantTimeCompare([]byte(apiKey), []byte(s.serveKey)) == 1 {
			return true
		}
	}
	return false
}

func (s *serveServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *serveServer) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeOAIError(w, http.StatusNotFound, "not_found", fmt.Sprintf("Unsupported endpoint: %s %s", r.Method, r.URL.Path))
}

func (s *serveServer) handleModels(w http.ResponseWriter, _ *http.Request) {
	created := time.Now().Unix()
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]modelEntry, 0, len(defaultServeModels))
	for _, m := range defaultServeModels {
		data = append(data, modelEntry{
			ID:      m.ID,
			Object:  "model",
			Created: created,
			OwnedBy: "deepseek",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (s *serveServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxServeRequestBytes+1))
	if err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body.")
		return
	}
	if len(body) > maxServeRequestBytes {
		writeOAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body exceeds 8MB limit.")
		return
	}

	var req oaiChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON request body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeOAIError(w, http.StatusBadRequest, "invalid_request_error", "messages must be a non-empty array.")
		return
	}

	// Acquire concurrency slot so parallel agent requests don't stampede DeepSeek web limits.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	}

	spec := resolveServeModel(req.Model, s.opts)
	// Support legacy OpenAI functions/function_call and web_search_options.
	if len(req.Tools) == 0 && len(req.Functions) > 0 {
		for _, fn := range req.Functions {
			req.Tools = append(req.Tools, oaiTool{Type: "function", Function: fn})
		}
	}
	if len(req.ToolChoice) == 0 && len(req.FunctionCall) > 0 {
		req.ToolChoice = req.FunctionCall
	}
	if len(bytesTrimSpace(req.WebSearchOptions)) > 0 && string(bytesTrimSpace(req.WebSearchOptions)) != "null" {
		spec.Search = true
	}
	if len(req.Tools) > 0 {
		filtered := make([]oaiTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			if strings.HasPrefix(strings.ToLower(t.Type), "web_search") {
				spec.Search = true
				continue
			}
			filtered = append(filtered, t)
		}
		req.Tools = filtered
	}
	stops := parseStopSequences(req.Stop)
	mode, _ := parseToolChoice(req.ToolChoice)
	parseTools := len(req.Tools) > 0 && mode != "none"

	completionID := "chatcmpl-" + shortRandomHex(12)
	created := time.Now().Unix()

	if req.Stream {
		s.serveStreaming(w, r, req, spec, stops, parseTools, completionID, created)
		return
	}
	s.serveNonStreaming(w, r, req, spec, stops, parseTools, completionID, created)
}

func (c *serveSessionCache) explainMiss(modelID, tHash string, messages []oaiMessage) string {
	norm := normalizeMessagesForHash(messages)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.order) == 0 {
		return "cache_empty"
	}
	// Inspect the most recent cached turn for structural mismatch diagnostics.
	lastKey := c.order[len(c.order)-1]
	st, ok := c.entries[lastKey]
	if !ok {
		return "no_entry"
	}
	if st.modelID != modelID {
		return fmt.Sprintf("model_changed(%s->%s)", st.modelID, modelID)
	}
	if st.toolsHash != tHash {
		return "tools_or_choice_changed"
	}
	limit := min(len(st.messages), len(norm))
	for i := 0; i < limit; i++ {
		a, _ := json.Marshal(st.messages[i])
		b, _ := json.Marshal(norm[i])
		if !bytes.Equal(a, b) {
			return fmt.Sprintf("msg[%d](role=%s)_changed(cached_len=%d,req_len=%d)", i, norm[i].Role, len(st.messages), len(norm))
		}
	}
	return fmt.Sprintf("len_mismatch(cached=%d,req=%d)", len(st.messages), len(norm))
}

func (s *serveServer) resolveSessionAndPrompt(req oaiChatRequest, spec serveModelSpec) (sessionID string, parentID *int64, prompt string, matchedKey string, missReason string) {
	// Look for the longest cached prefix terminating at an assistant message.
	for k := len(req.Messages) - 1; k >= 1; k-- {
		if !strings.EqualFold(req.Messages[k-1].Role, "assistant") {
			continue
		}
		key := prefixFingerprint(spec.ID, req.Tools, req.ToolChoice, req.Messages[:k])
		if cached, ok := s.cache.lookup(key); ok {
			tail := formatMessageSlice(req.Messages[k:])
			if !strings.EqualFold(req.Messages[len(req.Messages)-1].Role, "assistant") {
				tail += "\n### Assistant\n"
			}
			pid := cached.parentID
			return cached.sessionID, &pid, strings.TrimSpace(tail), key, ""
		}
	}
	reason := ""
	if len(req.Messages) > 2 {
		reason = s.cache.explainMiss(spec.ID, toolsFingerprint(req.Tools, req.ToolChoice), req.Messages)
	}
	return "", nil, buildFullPrompt(req), "", reason
}

func (s *serveServer) runUpstreamCompletion(
	ctx context.Context,
	req oaiChatRequest,
	spec serveModelSpec,
	onReady func(messageID int64),
	onThink func(text string),
	onReply func(text string),
) (usedSessionID string, finalMsgID int64, err error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sessionID, parentID, prompt, matchedKey, missReason := s.resolveSessionAndPrompt(req, spec)
	usedCache := matchedKey != ""

	for attempt := 0; attempt < 2; attempt++ {
		if sessionID == "" {
			sessionID, err = s.client.CreateSession(runCtx)
			if err != nil {
				return "", 0, err
			}
			if missReason != "" {
				s.logger.Printf("chat completion: model=%s msgs=%d tools=%d session=%s (new: %s)", spec.ID, len(req.Messages), len(req.Tools), sessionID, missReason)
			} else {
				s.logger.Printf("chat completion: model=%s msgs=%d tools=%d session=%s (new)", spec.ID, len(req.Messages), len(req.Tools), sessionID)
			}
		} else {
			var pid int64
			if parentID != nil {
				pid = *parentID
			}
			s.logger.Printf("chat completion: model=%s msgs=%d tools=%d session=%s parent=%d (reused)", spec.ID, len(req.Messages), len(req.Tools), sessionID, pid)
		}

		emittedAny := false
		var msgID int64
		err = s.client.Complete(runCtx, CompletionRequest{
			SessionID:       sessionID,
			ParentMessageID: parentID,
			Prompt:          prompt,
			FirstTurn:       parentID == nil,
			ThinkingEnabled: spec.Thinking,
			SearchEnabled:   spec.Search,
		}, CompletionHooks{
			OnEvent: func(ev StreamEvent) {
				switch ev.Kind {
				case StreamReady:
					if ev.MessageID != 0 {
						msgID = ev.MessageID
						if onReady != nil {
							onReady(ev.MessageID)
						}
					}
				case StreamThink:
					if ev.Text != "" {
						emittedAny = true
						if onThink != nil {
							onThink(ev.Text)
						}
					}
				case StreamReply:
					if ev.Text != "" {
						emittedAny = true
						if onReply != nil {
							onReply(ev.Text)
						}
					}
				}
			},
		})
		if err == nil {
			return sessionID, msgID, nil
		}
		if usedCache && !emittedAny && runCtx.Err() == nil {
			// Cached session expired or was rejected; evict and retry once with full prompt.
			s.cache.delete(matchedKey)
			usedCache = false
			sessionID = ""
			parentID = nil
			prompt = buildFullPrompt(req)
			continue
		}
		return sessionID, msgID, err
	}
	return sessionID, 0, err
}

func (s *serveServer) cacheTurn(req oaiChatRequest, spec serveModelSpec, sessionID string, msgID int64, content string, toolCalls []oaiToolCall) {
	if sessionID == "" || msgID == 0 {
		return
	}
	nextMsgs := make([]oaiMessage, len(req.Messages)+1)
	copy(nextMsgs, req.Messages)
	nextMsgs[len(req.Messages)] = oaiMessage{
		Role:      "assistant",
		Content:   oaiTextContent(content),
		ToolCalls: toolCalls,
	}
	key := prefixFingerprint(spec.ID, req.Tools, req.ToolChoice, nextMsgs)
	s.cache.store(key, cachedSessionState{
		sessionID: sessionID,
		parentID:  msgID,
		modelID:   spec.ID,
		toolsHash: toolsFingerprint(req.Tools, req.ToolChoice),
		messages:  normalizeMessagesForHash(nextMsgs),
	})
}

func (s *serveServer) serveNonStreaming(
	w http.ResponseWriter,
	r *http.Request,
	req oaiChatRequest,
	spec serveModelSpec,
	stops []string,
	parseTools bool,
	completionID string,
	created int64,
) {
	var contentBuf strings.Builder
	var thinkBuf strings.Builder
	var toolCalls []oaiToolCall

	parser := newToolStreamParser(parseTools, stops, func(text string) {
		contentBuf.WriteString(text)
	}, func(tc oaiToolCall) {
		toolCalls = append(toolCalls, tc)
	})

	runCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sessionID, msgID, err := s.runUpstreamCompletion(runCtx, req, spec, nil, func(text string) {
		thinkBuf.WriteString(text)
	}, func(text string) {
		if parser.Feed(text) {
			cancel()
		}
	})
	if err != nil && !(parser.stopped && errors.Is(err, context.Canceled)) {
		writeUpstreamOAIError(w, err)
		return
	}
	parser.Finish()

	contentStr := contentBuf.String()
	finishReason := "stop"
	var contentPtr *string
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
		if contentStr != "" {
			contentPtr = &contentStr
		}
	} else {
		contentPtr = &contentStr
	}

	s.cacheTurn(req, spec, sessionID, msgID, contentStr, toolCalls)

	promptChars := len(buildFullPrompt(req))
	compChars := len(contentStr) + thinkBuf.Len()
	promptTok := max(1, promptChars/4)
	compTok := max(1, compChars/4)

	writeJSON(w, http.StatusOK, oaiChatResponse{
		ID:      completionID,
		Object:  "chat.completion",
		Created: created,
		Model:   spec.ID,
		Choices: []oaiChoice{{
			Index: 0,
			Message: oaiResponseMsg{
				Role:             "assistant",
				Content:          contentPtr,
				ReasoningContent: thinkBuf.String(),
				ToolCalls:        toolCalls,
			},
			FinishReason: finishReason,
		}},
		Usage: oaiUsage{
			PromptTokens:     promptTok,
			CompletionTokens: compTok,
			TotalTokens:      promptTok + compTok,
		},
	})
}

func (s *serveServer) serveStreaming(
	w http.ResponseWriter,
	r *http.Request,
	req oaiChatRequest,
	spec serveModelSpec,
	stops []string,
	parseTools bool,
	completionID string,
	created int64,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOAIError(w, http.StatusInternalServerError, "server_error", "Streaming unsupported by response writer.")
		return
	}

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
		_ = writeSSEChunk(w, oaiStreamChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   spec.ID,
			Choices: []oaiStreamChoice{{
				Index: 0,
				Delta: oaiStreamDelta{Role: "assistant"},
			}},
		})
		flusher.Flush()
	}

	var contentBuf strings.Builder
	var thinkLen int
	var toolCalls []oaiToolCall
	toolIdx := 0

	parser := newToolStreamParser(parseTools, stops, func(text string) {
		ensureHeaders()
		contentBuf.WriteString(text)
		_ = writeSSEChunk(w, oaiStreamChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   spec.ID,
			Choices: []oaiStreamChoice{{
				Index: 0,
				Delta: oaiStreamDelta{Content: &text},
			}},
		})
		flusher.Flush()
	}, func(tc oaiToolCall) {
		ensureHeaders()
		idx := toolIdx
		toolIdx++
		stored := tc
		stored.Index = nil
		toolCalls = append(toolCalls, stored)

		// Emit OpenAI-compatible incremental tool_calls chunks: metadata first, then arguments.
		_ = writeSSEChunk(w, oaiStreamChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   spec.ID,
			Choices: []oaiStreamChoice{{
				Index: 0,
				Delta: oaiStreamDelta{
					ToolCalls: []oaiToolCall{{
						Index: &idx,
						ID:    tc.ID,
						Type:  "function",
						Function: oaiCallFunction{
							Name:      tc.Function.Name,
							Arguments: "",
						},
					}},
				},
			}},
		})
		_ = writeSSEChunk(w, oaiStreamChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   spec.ID,
			Choices: []oaiStreamChoice{{
				Index: 0,
				Delta: oaiStreamDelta{
					ToolCalls: []oaiToolCall{{
						Index: &idx,
						Function: oaiCallFunction{
							Arguments: tc.Function.Arguments,
						},
					}},
				},
			}},
		})
		flusher.Flush()
	})

	runCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sessionID, msgID, err := s.runUpstreamCompletion(runCtx, req, spec, nil, func(text string) {
		ensureHeaders()
		thinkLen += len(text)
		_ = writeSSEChunk(w, oaiStreamChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   spec.ID,
			Choices: []oaiStreamChoice{{
				Index: 0,
				Delta: oaiStreamDelta{ReasoningContent: text},
			}},
		})
		flusher.Flush()
	}, func(text string) {
		if parser.Feed(text) {
			cancel()
		}
	})

	if err != nil && !(parser.stopped && errors.Is(err, context.Canceled)) {
		if !headersSent {
			writeUpstreamOAIError(w, err)
			return
		}
		errPayload, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": friendlyError(err),
				"type":    "upstream_error",
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", errPayload)
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	parser.Finish()
	ensureHeaders()

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	s.cacheTurn(req, spec, sessionID, msgID, contentBuf.String(), toolCalls)

	_ = writeSSEChunk(w, oaiStreamChunk{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   spec.ID,
		Choices: []oaiStreamChoice{{
			Index:        0,
			Delta:        oaiStreamDelta{},
			FinishReason: &finishReason,
		}},
	})
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		promptTok := max(1, len(buildFullPrompt(req))/4)
		compTok := max(1, (contentBuf.Len()+thinkLen)/4)
		_ = writeSSEChunk(w, oaiStreamChunk{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   spec.ID,
			Choices: []oaiStreamChoice{},
			Usage: &oaiUsage{
				PromptTokens:     promptTok,
				CompletionTokens: compTok,
				TotalTokens:      promptTok + compTok,
			},
		})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeSSEChunk(w io.Writer, chunk oaiStreamChunk) error {
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

func writeUpstreamOAIError(w http.ResponseWriter, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		status := apiErr.Status
		if status == 0 || status == http.StatusOK {
			status = http.StatusBadGateway
			if apiErr.Code == 40303 {
				status = http.StatusForbidden
			}
		}
		code := "upstream_error"
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			code = "authentication_error"
			if apiErr.Code == 40303 {
				code = "challenge_required"
			}
		case http.StatusTooManyRequests:
			code = "rate_limit_exceeded"
		}
		writeOAIError(w, status, code, friendlyError(err))
		return
	}
	writeOAIError(w, http.StatusBadGateway, "upstream_error", friendlyError(err))
}

func runServeMode(opts stealthOptions, token string) int {
	addr, err := normalizeServeAddr(opts.serveAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deepseek-tui:", err)
		return 2
	}

	client := NewAPIClient(token)
	logger := log.New(os.Stderr, "deepseek-tui: ", log.LstdFlags)
	srv := newServeServer(client, opts, logger)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "deepseek-tui: listen %s: %v\n", addr, err)
		return 1
	}

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stderr, "deepseek-tui OpenAI server listening on http://%s/v1\n", ln.Addr().String())
	if srv.serveKey != "" {
		fmt.Fprintln(os.Stderr, "deepseek-tui: local bearer auth enabled")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		<-errCh
		return 0
	case err := <-errCh:
		if err != nil {
			fmt.Fprintln(os.Stderr, "deepseek-tui:", err)
			return 1
		}
		return 0
	}
}
