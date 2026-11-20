package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNewRequestUsesClientIdentity(t *testing.T) {
	client := &APIClient{
		baseURL:   "https://example.invalid",
		token:     "tok",
		deviceID:  "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		locale:    "de_DE",
		timezone:  "7200",
		userAgent: "custom-agent/1.0",
		http:      http.DefaultClient,
	}

	req, err := client.newRequest(context.Background(), http.MethodPost, "/x", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	want := map[string]string{
		"Authorization":            "Bearer tok",
		"User-Agent":               "custom-agent/1.0",
		"x-client-version":         clientVersion,
		"x-client-platform":        "web",
		"x-client-bundle-id":       clientBundleID,
		"x-client-locale":          "de_DE",
		"x-client-timezone-offset": "7200",
		"x-device-id":              "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
	}
	for name, value := range want {
		if got := req.Header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}

	client.deviceID = ""
	req, err = client.newRequest(context.Background(), http.MethodPost, "/x", nil)
	if err != nil {
		t.Fatalf("new request without device id: %v", err)
	}
	if _, ok := req.Header["X-Device-Id"]; ok {
		t.Fatalf("empty device id should omit the header, got %q", req.Header.Get("x-device-id"))
	}
}

func TestNewAPIClientDerivesIdentity(t *testing.T) {
	t.Setenv("DEEPSEEK_DEVICE_ID", "env-device-id")
	t.Setenv("DEEPSEEK_USER_AGENT", "env-agent/2.0")
	client := NewAPIClient("tok")

	if client.deviceID != "env-device-id" {
		t.Fatalf("device id = %q", client.deviceID)
	}
	if client.userAgent != "env-agent/2.0" {
		t.Fatalf("user agent = %q", client.userAgent)
	}
	if client.locale == "" || !validLocale(client.locale) {
		t.Fatalf("locale = %q", client.locale)
	}
	if _, err := strconv.Atoi(client.timezone); err != nil {
		t.Fatalf("timezone = %q: %v", client.timezone, err)
	}
}

func TestCreateSessionResponseShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"spec", `{"id":"session-spec"}`, "session-spec"},
		{"live-envelope", `{"code":0,"msg":"","data":{"biz_data":{"chat_session":{"id":"session-live"}}}}`, "session-live"},
		{"biz-flat", `{"code":0,"data":{"biz_data":{"id":"session-flat"}}}`, "session-flat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat_session/create" {
					t.Errorf("path = %s", r.URL.Path)
				}
				var payload map[string]any
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if payload["agent"] != "chat" {
					t.Errorf("agent = %v, want chat", payload["agent"])
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()

			client := NewAPIClient("test-token")
			client.baseURL = server.URL
			id, err := client.CreateSession(context.Background())
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			if id != tc.want {
				t.Fatalf("id = %q, want %q", id, tc.want)
			}
		})
	}
}

func TestCreatePowChallengeShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			"nested",
			`{"code":0,"data":{"biz_data":{"challenge":{"algorithm":"DeepSeekHashV1","challenge":"168efa309e5a73033fc82dde4333bbc6515da440ef961574f73f99cbe7b397ad","salt":"a40318e6b7af56bf27cf","expire_at":1790959021012,"difficulty":144000,"signature":"sig","target_path":"/api/v0/chat/completion"}}}}`,
		},
		{
			"flat",
			`{"code":0,"data":{"biz_data":{"algorithm":"DeepSeekHashV1","challenge":"168efa309e5a73033fc82dde4333bbc6515da440ef961574f73f99cbe7b397ad","salt":"a40318e6b7af56bf27cf","expire_at":1790959021012,"difficulty":144000,"signature":"sig","target_path":"/api/v0/chat/completion"}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if payload["target_path"] != completionTargetURL {
					t.Errorf("target_path = %v", payload["target_path"])
				}
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()

			client := NewAPIClient("test-token")
			client.baseURL = server.URL
			challenge, err := client.CreatePowChallenge(context.Background())
			if err != nil {
				t.Fatalf("challenge: %v", err)
			}
			if challenge.Salt != "a40318e6b7af56bf27cf" || challenge.Difficulty != 144000 {
				t.Fatalf("unexpected challenge: %+v", challenge)
			}
			if challenge.TargetPath != completionTargetURL {
				t.Fatalf("target path = %q", challenge.TargetPath)
			}
		})
	}
}

func TestPowResponseHeader(t *testing.T) {
	challenge := &PowChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  "168efa309e5a73033fc82dde4333bbc6515da440ef961574f73f99cbe7b397ad",
		Salt:       "a40318e6b7af56bf27cf",
		ExpireAt:   1790959021012,
		Difficulty: 144000,
		Signature:  "sig",
		TargetPath: completionTargetURL,
	}
	header, err := powResponseHeader(challenge, 58595)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	var decoded struct {
		Algorithm  string `json:"algorithm"`
		Challenge  string `json:"challenge"`
		Salt       string `json:"salt"`
		Answer     uint64 `json:"answer"`
		Signature  string `json:"signature"`
		TargetPath string `json:"target_path"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json: %v", err)
	}
	if decoded.Answer != 58595 || decoded.TargetPath != completionTargetURL || decoded.Salt != challenge.Salt {
		t.Fatalf("unexpected payload: %s", raw)
	}
}

func collectEvents(t *testing.T, input string) []StreamEvent {
	t.Helper()
	var events []StreamEvent
	parser := newStreamParser(func(event StreamEvent) {
		events = append(events, event)
	})
	if err := readSSE(strings.NewReader(input), parser.handle); err != nil {
		t.Fatalf("read SSE: %v", err)
	}
	return events
}

func joinEvents(events []StreamEvent, kind StreamEventKind) string {
	var out strings.Builder
	for _, event := range events {
		if event.Kind == kind {
			out.WriteString(event.Text)
		}
	}
	return out.String()
}

func TestReadSSESimplePatchProtocol(t *testing.T) {
	input := strings.Join([]string{
		`data: {"p":"response/content","o":"APPEND","v":"Hello"}`,
		``,
		`data: {"p":"response/content","o":"APPEND","v":" there"}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	events := collectEvents(t, input)
	if got := joinEvents(events, StreamReply); got != "Hello there" {
		t.Fatalf("reply = %q", got)
	}
}

func TestReadSSEFragmentProtocol(t *testing.T) {
	input := strings.Join([]string{
		`event: ready`,
		`data: {"request_message_id":1,"response_message_id":42}`,
		``,
		`data: {"v":{"response":{"fragments":[{"id":1,"type":"THINK","content":"pondering"}]}}}`,
		``,
		`data: {"p":"response/fragments","o":"APPEND","v":[{"id":2,"type":"RESPONSE","content":"Hello"}]}`,
		``,
		`data: {"p":"response/fragments/-1/content","o":"APPEND","v":" world"}`,
		``,
		`data: {"v":{"response":{"fragments":[{"id":1,"type":"THINK","content":"pondering"},{"id":2,"type":"RESPONSE","content":"Hello world"}]}}}`,
		``,
		`data: {"p":"response/fragments/-1/content","o":"APPEND","v":"!"}`,
		``,
		`event: close`,
		`data: {}`,
		``,
	}, "\n")

	events := collectEvents(t, input)
	if got := joinEvents(events, StreamReply); got != "Hello world!" {
		t.Fatalf("reply = %q, want %q", got, "Hello world!")
	}
	if got := joinEvents(events, StreamThink); got != "pondering" {
		t.Fatalf("think = %q", got)
	}
	var messageID int64
	for _, event := range events {
		if event.Kind == StreamReady {
			messageID = event.MessageID
		}
	}
	if messageID != 42 {
		t.Fatalf("message id = %d, want 42", messageID)
	}
}

func TestReadSSEErrorEvent(t *testing.T) {
	input := "event: error\ndata: {\"code\":400,\"msg\":\"bad session\"}\n\n"
	var events []StreamEvent
	parser := newStreamParser(func(event StreamEvent) { events = append(events, event) })
	err := readSSE(strings.NewReader(input), parser.handle)
	if err == nil || !strings.Contains(err.Error(), "bad session") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchSessionsPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/chat_session/fetch_page" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("authorization = %q", got)
		}
		query := r.URL.Query()
		if got := query.Get("count"); got != "50" {
			t.Errorf("count = %q", got)
		}
		if got := query.Get("lte_cursor.pinned"); got != "true" {
			t.Errorf("pinned cursor = %q", got)
		}
		if got := query.Get("lte_cursor.updated_at"); got != "1782741816.158" {
			t.Errorf("updated cursor = %q", got)
		}
		fmt.Fprint(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"chat_sessions":[{"id":"s1","title":"First chat","pinned":true,"updated_at":1782741816.158},{"id":"s2","title":null,"pinned":false,"updated_at":"1782740000"}],"has_more":true}}}`)
	}))
	defer server.Close()

	client := NewAPIClient("test-token")
	client.baseURL = server.URL
	page, err := client.FetchSessions(context.Background(), &SessionCursor{Pinned: true, UpdatedAt: 1782741816.158}, 50)
	if err != nil {
		t.Fatalf("fetch sessions: %v", err)
	}
	if !page.HasMore {
		t.Fatal("has_more was not decoded")
	}
	if len(page.Sessions) != 2 {
		t.Fatalf("sessions = %d", len(page.Sessions))
	}
	if page.Sessions[0].ID != "s1" || page.Sessions[0].Title != "First chat" || !page.Sessions[0].Pinned {
		t.Fatalf("unexpected first session: %+v", page.Sessions[0])
	}
	if page.Sessions[0].UpdatedAt != flexFloat64(1782741816.158) {
		t.Fatalf("updated_at = %v", page.Sessions[0].UpdatedAt)
	}
	if page.Sessions[1].Title != "" {
		t.Fatalf("null title = %q", page.Sessions[1].Title)
	}
	if page.Sessions[1].UpdatedAt != flexFloat64(1782740000) {
		t.Fatalf("string updated_at = %v", page.Sessions[1].UpdatedAt)
	}
}

func TestFetchSessionsFirstPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Has("lte_cursor.pinned") || query.Has("lte_cursor.updated_at") {
			t.Errorf("first page carried a cursor: %s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"code":0,"data":{"biz_data":{"chat_sessions":[],"has_more":false}}}`)
	}))
	defer server.Close()

	client := NewAPIClient("test-token")
	client.baseURL = server.URL
	page, err := client.FetchSessions(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("fetch sessions: %v", err)
	}
	if page.HasMore || len(page.Sessions) != 0 {
		t.Fatalf("unexpected page: %+v", page)
	}
}

func TestFetchHistoryDualShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/chat/history_messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("chat_session_id"); got != "session-1" {
			t.Errorf("chat_session_id = %q", got)
		}
		fmt.Fprint(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"chat_session":{"id":"session-1","title":"Old chat","current_message_id":42},"chat_messages":[{"message_id":40,"role":"USER","content":"","fragments":[{"type":"REQUEST","content":"first question"}]},{"message_id":41,"parent_id":40,"role":"ASSISTANT","fragments":[{"type":"THINK","content":"first reasoning"},{"type":"RESPONSE","content":"first answer"}]},{"message_id":"42","parent_id":41,"role":"ASSISTANT","content":"second answer","thinking_content":"second reasoning","status":"FINISHED"}]}}}`)
	}))
	defer server.Close()

	client := NewAPIClient("test-token")
	client.baseURL = server.URL
	history, err := client.FetchHistory(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("fetch history: %v", err)
	}
	if history.SessionID != "session-1" || history.Title != "Old chat" {
		t.Fatalf("unexpected history header: %+v", history)
	}
	if history.CurrentMessageID != 42 {
		t.Fatalf("current_message_id = %d", history.CurrentMessageID)
	}
	if len(history.Messages) != 3 {
		t.Fatalf("messages = %d", len(history.Messages))
	}
	if got := history.Messages[0].UserText(); got != "first question" {
		t.Fatalf("user text = %q", got)
	}
	if got := history.Messages[1].AssistantText(); got != "first answer" {
		t.Fatalf("assistant text = %q", got)
	}
	if got := history.Messages[1].AssistantThinking(); got != "first reasoning" {
		t.Fatalf("assistant thinking = %q", got)
	}
	if got := history.Messages[2].AssistantText(); got != "second answer" {
		t.Fatalf("content fallback text = %q", got)
	}
	if got := history.Messages[2].AssistantThinking(); got != "second reasoning" {
		t.Fatalf("thinking_content fallback = %q", got)
	}
	if int64(history.Messages[2].MessageID) != 42 {
		t.Fatalf("string message_id = %d", history.Messages[2].MessageID)
	}
}

func TestDeleteSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat_session/delete" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		if payload["chat_session_id"] != "session-1" {
			t.Errorf("chat_session_id = %q", payload["chat_session_id"])
		}
		fmt.Fprint(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{}}}`)
	}))
	defer server.Close()

	client := NewAPIClient("test-token")
	client.baseURL = server.URL
	if err := client.DeleteSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
}

func TestChatHistoryToMarkdown(t *testing.T) {
	history := &ChatHistory{
		Title: "Test Chat",
		Messages: []HistoryMessage{
			{Role: "USER", Fragments: []HistoryFragment{{Type: "REQUEST", Content: "Hello world"}}},
			{Role: "ASSISTANT", Fragments: []HistoryFragment{
				{Type: "THINK", Content: "thinking process here"},
				{Type: "RESPONSE", Content: "assistant response here"},
			}},
		},
	}
	md := history.ToMarkdown()
	for _, want := range []string{"# Test Chat", "### User", "Hello world", "### DeepSeek", "thinking process here", "assistant response here", "<details>", "</details>"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown output missing %q:\n%s", want, md)
		}
	}
}

func TestUploadFile(t *testing.T) {
	tmp, err := os.CreateTemp("", "upload-test-*.txt")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString("sample attachment content"); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	tmp.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/create_pow_challenge":
			var req map[string]string
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode pow req: %v", err)
			}
			if req["target_path"] != "/api/v0/file/upload_file" {
				t.Errorf("target_path = %q, want /api/v0/file/upload_file", req["target_path"])
			}
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"algorithm":"DeepSeekHashV1","challenge":"168efa309e5a73033fc82dde4333bbc6515da440ef961574f73f99cbe7b397ad","salt":"a40318e6b7af56bf27cf","expire_at":1790959021012,"difficulty":144000,"signature":"captured","target_path":"/api/v0/file/upload_file"}}}`)
		case "/file/upload_file":
			if r.Method != http.MethodPost {
				t.Errorf("upload method = %s, want POST", r.Method)
			}
			if r.Header.Get("x-ds-pow-response") == "" {
				t.Error("missing x-ds-pow-response header")
			}
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Fatalf("form file: %v", err)
			}
			defer file.Close()
			if header.Filename != filepath.Base(tmp.Name()) {
				t.Errorf("filename = %q, want %q", header.Filename, filepath.Base(tmp.Name()))
			}
			content, _ := io.ReadAll(file)
			if string(content) != "sample attachment content" {
				t.Errorf("content = %q, want sample attachment content", string(content))
			}
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"id":"file-998877"}}}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewAPIClient("test-token")
	client.baseURL = server.URL

	fileID, err := client.UploadFile(context.Background(), tmp.Name())
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if fileID != "file-998877" {
		t.Fatalf("fileID = %q, want file-998877", fileID)
	}

	// Verify error on non-existent file
	if _, err := client.UploadFile(context.Background(), "/non/existent/path.txt"); err == nil {
		t.Fatal("expected error on non-existent file, got nil")
	}

	// Verify error on directory
	if _, err := client.UploadFile(context.Background(), os.TempDir()); err == nil {
		t.Fatal("expected error on directory, got nil")
	}
}
