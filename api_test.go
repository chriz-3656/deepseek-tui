package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
