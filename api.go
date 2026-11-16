package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	apiBaseURL          = "https://chat.deepseek.com/api/v0"
	defaultUserAgent    = "Mozilla/5.0 (X11; Linux x86_64; rv:151.0) Gecko/20100101 Firefox/151.0"
	clientVersion       = "2.8.0"
	clientBundleID      = "com.deepseek.chat"
	clientLocale        = "en_US"
	clientTimezone      = "-18000"
	defaultDeviceID     = "00000000-0000-4000-8000-000000000000"
	completionTargetURL = "/api/v0/chat/completion"
)

// APIError is a structured upstream failure from chat.deepseek.com.
type APIError struct {
	Status  int
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("deepseek: HTTP %d (code %d): %s", e.Status, e.Code, e.Message)
	}
	if e.Status != 0 {
		return fmt.Sprintf("deepseek: HTTP %d: %s", e.Status, e.Message)
	}
	return "deepseek: " + e.Message
}

// APIClient talks to the DeepSeek web chat backend with a browser userToken.
type APIClient struct {
	baseURL  string
	token    string
	deviceID string
	http     *http.Client
}

func NewAPIClient(token string) *APIClient {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	return &APIClient{
		baseURL:  apiBaseURL,
		token:    token,
		deviceID: defaultDeviceID,
		http:     &http.Client{Transport: transport},
	}
}

func (c *APIClient) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("x-client-version", clientVersion)
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("x-client-bundle-id", clientBundleID)
	req.Header.Set("x-client-locale", clientLocale)
	req.Header.Set("x-client-timezone-offset", clientTimezone)
	req.Header.Set("x-device-id", c.deviceID)
	req.Header.Set("Origin", "https://chat.deepseek.com")
	req.Header.Set("Referer", "https://chat.deepseek.com/")
	return req, nil
}

type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// postJSON sends a JSON request and decodes the standard DeepSeek envelope.
// It tolerates both {code,msg,data:{biz_data:...}} and flat responses.
func (c *APIClient) postJSON(ctx context.Context, path string, payload any, out any) error {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	return c.doJSON(ctx, http.MethodPost, path, body, out)
}

// getJSON performs a GET and decodes the standard DeepSeek envelope.
func (c *APIClient) getJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, out)
}

func (c *APIClient) doJSON(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return apiErrorFromBody(resp.StatusCode, raw)
	}

	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && (env.Code != 0 || env.Msg != "" || len(env.Data) > 0) {
		if env.Code != 0 {
			return &APIError{Status: resp.StatusCode, Code: env.Code, Message: env.Msg}
		}
		if out == nil {
			return nil
		}
		return decodeEnvelopeData(raw, env.Data, out)
	}

	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func decodeEnvelopeData(rawBody, data json.RawMessage, out any) error {
	if len(data) > 0 && !bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		var nested struct {
			BizCode int             `json:"biz_code"`
			BizMsg  string          `json:"biz_msg"`
			BizData json.RawMessage `json:"biz_data"`
		}
		if err := json.Unmarshal(data, &nested); err == nil && len(nested.BizData) > 0 && !bytes.Equal(bytes.TrimSpace(nested.BizData), []byte("null")) {
			if nested.BizCode != 0 {
				return &APIError{Code: nested.BizCode, Message: nested.BizMsg}
			}
			return json.Unmarshal(nested.BizData, out)
		}
		return json.Unmarshal(data, out)
	}
	return json.Unmarshal(rawBody, out)
}

func apiErrorFromBody(status int, body []byte) error {
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(body, &env) == nil && (env.Code != 0 || env.Msg != "") {
		return &APIError{Status: status, Code: env.Code, Message: env.Msg}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &APIError{Status: status, Message: msg}
}

// flexInt64 accepts both JSON numbers and numeric strings. DeepSeek's web
// endpoints have served message ids in both encodings.
type flexInt64 int64

func (n *flexInt64) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*n = 0
		return nil
	}
	var value int64
	if err := json.Unmarshal(data, &value); err == nil {
		*n = flexInt64(value)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("deepseek: invalid integer %s", data)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		*n = 0
		return nil
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fmt.Errorf("deepseek: invalid integer %q", text)
	}
	*n = flexInt64(value)
	return nil
}

// flexFloat64 accepts JSON numbers and numeric strings for epoch timestamps.
type flexFloat64 float64

func (n *flexFloat64) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*n = 0
		return nil
	}
	var value float64
	if err := json.Unmarshal(data, &value); err == nil {
		*n = flexFloat64(value)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("deepseek: invalid number %s", data)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		*n = 0
		return nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return fmt.Errorf("deepseek: invalid number %q", text)
	}
	*n = flexFloat64(value)
	return nil
}

// SessionInfo is one entry in the account's chat drawer.
type SessionInfo struct {
	ID         string      `json:"id"`
	Title      string      `json:"title"`
	Pinned     bool        `json:"pinned"`
	UpdatedAt  flexFloat64 `json:"updated_at"`
	InsertedAt flexFloat64 `json:"inserted_at"`
}

// SessionCursor is the fetch_page cursor. The server returns sessions
// strictly older than this entry, newest first.
type SessionCursor struct {
	Pinned    bool
	UpdatedAt float64
}

// SessionPage is one page of the chat drawer.
type SessionPage struct {
	Sessions []SessionInfo
	HasMore  bool
}

// FetchSessions fetches one page of the account's chat list. A nil cursor
// requests the first page. DeepSeek's endpoint is a GET and needs no PoW.
func (c *APIClient) FetchSessions(ctx context.Context, cursor *SessionCursor, count int) (*SessionPage, error) {
	if count <= 0 {
		count = 50
	}

	values := url.Values{}
	values.Set("count", strconv.Itoa(count))
	if cursor != nil {
		values.Set("lte_cursor.pinned", strconv.FormatBool(cursor.Pinned))
		values.Set("lte_cursor.updated_at", strconv.FormatFloat(cursor.UpdatedAt, 'f', -1, 64))
	}

	var out struct {
		ChatSessions []SessionInfo `json:"chat_sessions"`
		Sessions     []SessionInfo `json:"sessions"`
		HasMore      bool          `json:"has_more"`
	}
	if err := c.getJSON(ctx, "/chat_session/fetch_page?"+values.Encode(), &out); err != nil {
		return nil, err
	}
	sessions := out.ChatSessions
	if sessions == nil {
		sessions = out.Sessions
	}
	return &SessionPage{Sessions: sessions, HasMore: out.HasMore}, nil
}

// HistoryFragment is one piece of a user or assistant message.
type HistoryFragment struct {
	Type        string      `json:"type"`
	Content     string      `json:"content"`
	ElapsedSecs flexFloat64 `json:"elapsed_secs"`
}

// HistoryMessage is one message in a server-side chat session. DeepSeek has
// served two shapes: text in fragments[] (REQUEST/THINK/RESPONSE), and text
// in the top-level content / thinking_content fields. Both are supported.
type HistoryMessage struct {
	MessageID       flexInt64         `json:"message_id"`
	ParentID        *flexInt64        `json:"parent_id"`
	Role            string            `json:"role"`
	Content         string            `json:"content"`
	ThinkingContent string            `json:"thinking_content"`
	Status          string            `json:"status"`
	Fragments       []HistoryFragment `json:"fragments"`
	InsertedAt      flexFloat64       `json:"inserted_at"`
}

// ChatHistory is a session plus its complete message list. The history
// endpoint returns the whole conversation in one response (no pagination).
type ChatHistory struct {
	SessionID        string
	Title            string
	CurrentMessageID int64
	Messages         []HistoryMessage
}

// FetchHistory loads every message of one chat session.
func (c *APIClient) FetchHistory(ctx context.Context, sessionID string) (*ChatHistory, error) {
	values := url.Values{}
	values.Set("chat_session_id", sessionID)

	var out struct {
		ChatSession struct {
			ID               string    `json:"id"`
			Title            string    `json:"title"`
			CurrentMessageID flexInt64 `json:"current_message_id"`
		} `json:"chat_session"`
		ChatMessages     []HistoryMessage `json:"chat_messages"`
		CurrentMessageID flexInt64        `json:"current_message_id"`
	}
	if err := c.getJSON(ctx, "/chat/history_messages?"+values.Encode(), &out); err != nil {
		return nil, err
	}

	history := &ChatHistory{
		SessionID: sessionID,
		Title:     out.ChatSession.Title,
		Messages:  out.ChatMessages,
	}
	history.CurrentMessageID = int64(out.ChatSession.CurrentMessageID)
	if history.CurrentMessageID == 0 {
		history.CurrentMessageID = int64(out.CurrentMessageID)
	}
	if history.SessionID == "" {
		history.SessionID = out.ChatSession.ID
	}
	return history, nil
}

// DeleteSession removes one chat from the account.
func (c *APIClient) DeleteSession(ctx context.Context, sessionID string) error {
	return c.postJSON(ctx, "/chat_session/delete", map[string]string{"chat_session_id": sessionID}, nil)
}

// fragmentText joins the text of all fragments with the given type.
func (m HistoryMessage) fragmentText(want string) string {
	var parts []string
	for _, fragment := range m.Fragments {
		if !strings.EqualFold(strings.TrimSpace(fragment.Type), want) {
			continue
		}
		if strings.TrimSpace(fragment.Content) == "" {
			continue
		}
		parts = append(parts, fragment.Content)
	}
	return strings.Join(parts, "\n\n")
}

// UserText returns the user's prompt.
func (m HistoryMessage) UserText() string {
	if text := m.fragmentText("REQUEST"); text != "" {
		return text
	}
	return strings.TrimSpace(m.Content)
}

// AssistantText returns the assistant's visible answer.
func (m HistoryMessage) AssistantText() string {
	if text := m.fragmentText("RESPONSE"); text != "" {
		return text
	}
	return strings.TrimSpace(m.Content)
}

// AssistantThinking returns the assistant's reasoning, if any.
func (m HistoryMessage) AssistantThinking() string {
	if text := m.fragmentText("THINK"); text != "" {
		return text
	}
	return strings.TrimSpace(m.ThinkingContent)
}

// CreateSession opens a fresh chat session and returns its id.
func (c *APIClient) CreateSession(ctx context.Context) (string, error) {
	var out struct {
		ID          string `json:"id"`
		ChatSession struct {
			ID string `json:"id"`
		} `json:"chat_session"`
	}
	if err := c.postJSON(ctx, "/chat_session/create", map[string]string{"agent": "chat"}, &out); err != nil {
		return "", err
	}
	id := out.ID
	if id == "" {
		id = out.ChatSession.ID
	}
	if id == "" {
		return "", errors.New("deepseek: session response did not contain an id")
	}
	return id, nil
}

// CreatePowChallenge fetches a proof-of-work challenge for /chat/completion.
func (c *APIClient) CreatePowChallenge(ctx context.Context) (*PowChallenge, error) {
	var out struct {
		Challenge  json.RawMessage `json:"challenge"`
		Salt       string          `json:"salt"`
		ExpireAt   int64           `json:"expire_at"`
		Difficulty int64           `json:"difficulty"`
		Algorithm  string          `json:"algorithm"`
		Signature  string          `json:"signature"`
		TargetPath string          `json:"target_path"`
	}
	if err := c.postJSON(ctx, "/chat/create_pow_challenge", map[string]string{"target_path": completionTargetURL}, &out); err != nil {
		return nil, err
	}

	var challenge PowChallenge
	if len(out.Challenge) > 0 {
		trimmed := bytes.TrimSpace(out.Challenge)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			if err := json.Unmarshal(trimmed, &challenge); err != nil {
				return nil, fmt.Errorf("pow: bad challenge object: %w", err)
			}
		} else {
			var hexDigest string
			if err := json.Unmarshal(trimmed, &hexDigest); err != nil {
				return nil, fmt.Errorf("pow: bad challenge digest: %w", err)
			}
			challenge.Challenge = hexDigest
		}
	}
	if challenge.Salt == "" {
		challenge.Salt = out.Salt
	}
	if challenge.ExpireAt == 0 {
		challenge.ExpireAt = out.ExpireAt
	}
	if challenge.Difficulty == 0 {
		challenge.Difficulty = out.Difficulty
	}
	if challenge.Algorithm == "" {
		challenge.Algorithm = out.Algorithm
	}
	if challenge.Signature == "" {
		challenge.Signature = out.Signature
	}
	if challenge.TargetPath == "" {
		challenge.TargetPath = out.TargetPath
	}
	if challenge.Algorithm == "" {
		challenge.Algorithm = "DeepSeekHashV1"
	}
	if challenge.Algorithm != "DeepSeekHashV1" {
		return nil, fmt.Errorf("pow: unsupported algorithm %q", challenge.Algorithm)
	}
	if challenge.Challenge == "" || challenge.Salt == "" || challenge.Difficulty <= 0 {
		return nil, errors.New("pow: incomplete challenge from server")
	}
	if challenge.TargetPath == "" {
		challenge.TargetPath = completionTargetURL
	}
	return &challenge, nil
}

// CompletionRequest describes one turn in a chat session.
type CompletionRequest struct {
	SessionID       string
	ParentMessageID *int64
	Prompt          string
	FirstTurn       bool
}

// StreamEventKind identifies what a streamed event carries.
type StreamEventKind int

const (
	StreamThink StreamEventKind = iota
	StreamReply
	StreamReady
)

// StreamEvent is one parsed piece of the completion stream.
type StreamEvent struct {
	Kind      StreamEventKind
	Text      string
	MessageID int64
}

// CompletionHooks receive progress and streamed content.
type CompletionHooks struct {
	OnStatus      func(string)
	OnPowProgress func(done, total int64)
	OnEvent       func(StreamEvent)
}

// Complete runs the full flow: create a PoW challenge, solve it, submit the
// prompt, then stream the assistant response.
func (c *APIClient) Complete(ctx context.Context, req CompletionRequest, hooks CompletionHooks) error {
	status := func(s string) {
		if hooks.OnStatus != nil {
			hooks.OnStatus(s)
		}
	}

	status("requesting proof-of-work challenge…")
	challenge, err := c.CreatePowChallenge(ctx)
	if err != nil {
		return fmt.Errorf("pow challenge: %w", err)
	}

	status("solving proof-of-work…")
	answer, err := challenge.Solve(ctx, hooks.OnPowProgress)
	if err != nil {
		return fmt.Errorf("solve proof-of-work: %w", err)
	}

	powHeader, err := powResponseHeader(challenge, answer)
	if err != nil {
		return err
	}

	var modelType *string
	if req.FirstTurn {
		mt := "default"
		modelType = &mt
	}
	payload := struct {
		ChatSessionID   string   `json:"chat_session_id"`
		ParentMessageID *int64   `json:"parent_message_id"`
		ModelType       *string  `json:"model_type"`
		Prompt          string   `json:"prompt"`
		RefFileIDs      []string `json:"ref_file_ids"`
		ThinkingEnabled bool     `json:"thinking_enabled"`
		SearchEnabled   bool     `json:"search_enabled"`
		Action          *string  `json:"action"`
		Preempt         bool     `json:"preempt"`
	}{
		ChatSessionID:   req.SessionID,
		ParentMessageID: req.ParentMessageID,
		ModelType:       modelType,
		Prompt:          req.Prompt,
		RefFileIDs:      []string{},
		ThinkingEnabled: true,
		SearchEnabled:   true,
		Preempt:         false,
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	status("waiting for DeepSeek…")
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/chat/completion", raw)
	if err != nil {
		return err
	}
	httpReq.Header.Set("x-ds-pow-response", powHeader)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return apiErrorFromBody(resp.StatusCode, body)
	}

	status("streaming")
	parser := newStreamParser(hooks.OnEvent)
	return readSSE(resp.Body, parser.handle)
}

// powResponseHeader builds the base64 JSON blob DeepSeek expects in
// x-ds-pow-response.
func powResponseHeader(challenge *PowChallenge, answer uint64) (string, error) {
	targetPath := challenge.TargetPath
	if targetPath == "" {
		targetPath = completionTargetURL
	}
	payload := struct {
		Algorithm  string `json:"algorithm"`
		Challenge  string `json:"challenge"`
		Salt       string `json:"salt"`
		Answer     uint64 `json:"answer"`
		Signature  string `json:"signature"`
		TargetPath string `json:"target_path"`
	}{
		Algorithm:  challenge.Algorithm,
		Challenge:  challenge.Challenge,
		Salt:       challenge.Salt,
		Answer:     answer,
		Signature:  challenge.Signature,
		TargetPath: targetPath,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

type wireFragment struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

type fragmentState struct {
	id   int64
	typ  string
	sent int
}

type streamParser struct {
	onEvent  func(StreamEvent)
	lastPath string
	frags    []fragmentState
}

func newStreamParser(onEvent func(StreamEvent)) *streamParser {
	return &streamParser{onEvent: onEvent}
}

func (p *streamParser) emit(event StreamEvent) {
	if p.onEvent != nil {
		p.onEvent(event)
	}
}

func (p *streamParser) emitFragment(typ, text string) {
	if text == "" {
		return
	}
	switch typ {
	case "THINK", "THINKING", "REASONING":
		p.emit(StreamEvent{Kind: StreamThink, Text: text})
	case "RESPONSE":
		p.emit(StreamEvent{Kind: StreamReply, Text: text})
	}
}

// handle consumes one SSE event. It returns done=true for a close event.
func (p *streamParser) handle(event string, data []byte) (bool, error) {
	switch event {
	case "close":
		return true, nil
	case "error":
		var e struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(data, &e) == nil && e.Msg != "" {
			return false, fmt.Errorf("deepseek stream: %s", e.Msg)
		}
		return false, errors.New("deepseek stream: upstream error")
	case "ready":
		var ready struct {
			ResponseMessageID int64 `json:"response_message_id"`
		}
		if json.Unmarshal(data, &ready) == nil && ready.ResponseMessageID != 0 {
			p.emit(StreamEvent{Kind: StreamReady, MessageID: ready.ResponseMessageID})
		}
		return false, nil
	case "", "message", "update_session", "title":
		p.applyPatch(data)
	}
	return false, nil
}

func (p *streamParser) applyPatch(data []byte) {
	if len(data) == 0 {
		return
	}
	var patch struct {
		P *string         `json:"p"`
		O string          `json:"o"`
		V json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal(data, &patch); err != nil {
		return
	}

	if patch.P == nil {
		var text string
		if json.Unmarshal(patch.V, &text) == nil {
			p.appendContinuation(text)
			return
		}
		var dump struct {
			Response struct {
				MessageID int64          `json:"message_id"`
				Fragments []wireFragment `json:"fragments"`
			} `json:"response"`
		}
		if json.Unmarshal(patch.V, &dump) == nil {
			if dump.Response.MessageID != 0 {
				p.emit(StreamEvent{Kind: StreamReady, MessageID: dump.Response.MessageID})
			}
			p.snapshot(dump.Response.Fragments)
		}
		return
	}

	path := *patch.P
	p.lastPath = path

	switch {
	case strings.Contains(path, "response/fragments/") && strings.HasSuffix(path, "/content"):
		var text string
		if json.Unmarshal(patch.V, &text) == nil {
			p.appendToFragmentPath(path, text)
		}
	case path == "response/content":
		var text string
		if json.Unmarshal(patch.V, &text) == nil {
			p.emit(StreamEvent{Kind: StreamReply, Text: text})
		}
	case path == "response/fragments":
		var fragments []wireFragment
		if json.Unmarshal(patch.V, &fragments) == nil {
			if patch.O == "APPEND" {
				for _, fragment := range fragments {
					p.appendFragment(fragment)
				}
			} else {
				p.snapshot(fragments)
			}
		}
	}
}

func (p *streamParser) appendContinuation(text string) {
	if text == "" {
		return
	}
	switch {
	case p.lastPath == "response/content":
		p.emit(StreamEvent{Kind: StreamReply, Text: text})
	case strings.Contains(p.lastPath, "response/fragments/") && strings.HasSuffix(p.lastPath, "/content"):
		p.appendToFragmentPath(p.lastPath, text)
	}
}

func (p *streamParser) appendToFragmentPath(path, text string) {
	if text == "" {
		return
	}
	if len(p.frags) == 0 {
		p.emit(StreamEvent{Kind: StreamReply, Text: text})
		return
	}

	index := -1
	rest := strings.TrimPrefix(path, "response/fragments/")
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	if n, err := strconv.Atoi(rest); err == nil {
		index = n
	}
	if index < 0 || index >= len(p.frags) {
		index = len(p.frags) - 1
	}

	state := &p.frags[index]
	state.sent += len(text)
	p.emitFragment(state.typ, text)
}

func (p *streamParser) appendFragment(fragment wireFragment) {
	for i := range p.frags {
		if p.frags[i].id != fragment.ID {
			continue
		}
		state := &p.frags[i]
		if state.typ != fragment.Type {
			state.typ = fragment.Type
			state.sent = 0
		}
		if state.sent < len(fragment.Content) {
			p.emitFragment(fragment.Type, fragment.Content[state.sent:])
		}
		state.sent = len(fragment.Content)
		return
	}
	p.frags = append(p.frags, fragmentState{id: fragment.ID, typ: fragment.Type, sent: len(fragment.Content)})
	p.emitFragment(fragment.Type, fragment.Content)
}

func (p *streamParser) snapshot(fragments []wireFragment) {
	previous := make(map[int64]fragmentState, len(p.frags))
	for _, state := range p.frags {
		previous[state.id] = state
	}
	next := make([]fragmentState, 0, len(fragments))
	for _, fragment := range fragments {
		already := 0
		if old, ok := previous[fragment.ID]; ok && old.typ == fragment.Type {
			already = old.sent
		}
		if already > len(fragment.Content) {
			already = 0
		}
		if already < len(fragment.Content) {
			p.emitFragment(fragment.Type, fragment.Content[already:])
		}
		next = append(next, fragmentState{id: fragment.ID, typ: fragment.Type, sent: len(fragment.Content)})
	}
	p.frags = next
	p.lastPath = "response/fragments/-1/content"
}

// readSSE parses a text/event-stream. The callback returns done=true to stop.
func readSSE(r io.Reader, handle func(event string, data []byte) (bool, error)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var event string
	var data bytes.Buffer

	flush := func() (bool, error) {
		if event == "" && data.Len() == 0 {
			return false, nil
		}
		payload := data.Bytes()
		trimmed := bytes.TrimSpace(payload)
		var (
			done bool
			err  error
		)
		if bytes.Equal(trimmed, []byte("[DONE]")) {
			done = true
		} else {
			done, err = handle(event, payload)
		}
		event = ""
		data.Reset()
		return done, err
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if done, err := flush(); err != nil || done {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	_, err := flush()
	return err
}
