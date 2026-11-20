package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParseStealthArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    stealthOptions
		wantErr bool
	}{
		{
			name: "plain prompt",
			args: []string{"hello", "world"},
			want: stealthOptions{prompt: "hello world"},
		},
		{
			name: "short code flag",
			args: []string{"-c", "print", "code"},
			want: stealthOptions{prompt: "print code", codeOnly: true},
		},
		{
			name: "long code flag after prompt",
			args: []string{"print", "--code"},
			want: stealthOptions{prompt: "print", codeOnly: true},
		},
		{
			name: "first flag",
			args: []string{"-1", "print", "code"},
			want: stealthOptions{prompt: "print code", codeOnly: true, first: true},
		},
		{
			name: "first after prompt",
			args: []string{"print", "--first"},
			want: stealthOptions{prompt: "print", codeOnly: true, first: true},
		},
		{
			name: "lang flag",
			args: []string{"--lang", "bash", "print"},
			want: stealthOptions{prompt: "print", codeOnly: true, lang: "bash"},
		},
		{
			name: "short lang flag",
			args: []string{"-l", "sh", "print"},
			want: stealthOptions{prompt: "print", codeOnly: true, lang: "sh"},
		},
		{
			name: "lang list",
			args: []string{"--lang=go,sh", "print"},
			want: stealthOptions{prompt: "print", codeOnly: true, lang: "go,sh"},
		},
		{
			name: "first and lang",
			args: []string{"--first", "--lang", "bash", "print"},
			want: stealthOptions{prompt: "print", codeOnly: true, first: true, lang: "bash"},
		},
		{
			name: "no hint",
			args: []string{"-c", "--no-hint", "print"},
			want: stealthOptions{prompt: "print", codeOnly: true, noHint: true},
		},
		{
			name: "token with value",
			args: []string{"--token", "tok", "hi"},
			want: stealthOptions{prompt: "hi", token: "tok"},
		},
		{
			name: "token with equals",
			args: []string{"--token=tok", "hi"},
			want: stealthOptions{prompt: "hi", token: "tok"},
		},
		{
			name: "think flag",
			args: []string{"--think", "hi"},
			want: stealthOptions{prompt: "hi", think: true},
		},
		{
			name: "no-think flag",
			args: []string{"--no-think", "hi"},
			want: stealthOptions{prompt: "hi", think: false},
		},
		{
			name: "search flag",
			args: []string{"--search", "hi"},
			want: stealthOptions{prompt: "hi", noSearch: false},
		},
		{
			name: "no-search flag",
			args: []string{"--no-search", "hi"},
			want: stealthOptions{prompt: "hi", noSearch: true},
		},
		{
			name: "file flag",
			args: []string{"-f", "doc.txt", "summarize"},
			want: stealthOptions{prompt: "summarize", filePath: "doc.txt"},
		},
		{
			name: "file flag equals",
			args: []string{"--file=doc.txt", "summarize"},
			want: stealthOptions{prompt: "summarize", filePath: "doc.txt"},
		},
		{
			name: "export flag",
			args: []string{"--export", "session-123"},
			want: stealthOptions{exportID: "session-123"},
		},
		{
			name: "dash prompt after terminator",
			args: []string{"--", "-c", "literal"},
			want: stealthOptions{prompt: "-c literal"},
		},
		{
			name: "empty",
			want: stealthOptions{},
		},
		{
			name: "help",
			args: []string{"--help"},
			want: stealthOptions{help: true},
		},
		{
			name:    "unknown flag",
			args:    []string{"--nope"},
			wantErr: true,
		},
		{
			name:    "missing token value",
			args:    []string{"--token"},
			wantErr: true,
		},
		{
			name:    "missing lang value",
			args:    []string{"--lang"},
			wantErr: true,
		},
		{
			name:    "empty lang value",
			args:    []string{"--lang="},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStealthArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseStealthArgs: %v", err)
			}
			if got != tt.want {
				t.Fatalf("options = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestExtractCodeBlocks(t *testing.T) {
	text := "Here is how:\r\n```go\r\nfmt.Println(\"hi\")\r\n```\r\n" +
		"and a plain block:\n```\nplain text\n```\n" +
		"and shell:\n```bash title=\"demo\"\necho hi\n```\nDone."

	got := extractCodeBlocks(text)
	want := []codeBlock{
		{language: "go", body: `fmt.Println("hi")`},
		{language: "", body: "plain text"},
		{language: "bash", body: "echo hi"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks = %#v, want %#v", got, want)
	}
}

func TestSelectCodeBlocks(t *testing.T) {
	text := "```bash\necho bash\n```\n```go\nfmt.Println(1)\n```\n```sh\necho sh\n```"

	tests := []struct {
		name   string
		filter codeFilter
		want   []string
	}{
		{name: "all", want: []string{"echo bash", "fmt.Println(1)", "echo sh"}},
		{name: "first", filter: codeFilter{first: true}, want: []string{"echo bash"}},
		{name: "language", filter: codeFilter{lang: "go"}, want: []string{"fmt.Println(1)"}},
		{name: "shell alias", filter: codeFilter{lang: "shell"}, want: []string{"echo bash", "echo sh"}},
		{name: "language list", filter: codeFilter{lang: "go,sh"}, want: []string{"fmt.Println(1)", "echo sh"}},
		{name: "first after filter", filter: codeFilter{first: true, lang: "shell"}, want: []string{"echo bash"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, block := range selectCodeBlocks(text, tt.filter) {
				got = append(got, block.body)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("blocks = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestPrintOnlyCodeTo(t *testing.T) {
	text := "```go\nfmt.Println(1)\n```\n```bash\necho hi\n```"

	var out bytes.Buffer
	if err := printOnlyCodeTo(&out, text, codeFilter{lang: "go"}); err != nil {
		t.Fatalf("printOnlyCodeTo: %v", err)
	}
	if got, want := out.String(), "fmt.Println(1)\n"; got != want {
		t.Fatalf("printed code = %q, want %q", got, want)
	}

	out.Reset()
	err := printOnlyCodeTo(&out, text, codeFilter{lang: "rust"})
	if !errors.Is(err, errNoCodeBlock) {
		t.Fatalf("error = %v, want errNoCodeBlock", err)
	}
	if out.Len() != 0 {
		t.Fatalf("printed %q, want no output", out.String())
	}
}

func TestBuildRequestPrompt(t *testing.T) {
	const prompt = "write code"

	if got := buildRequestPrompt(prompt, stealthOptions{}); got != prompt {
		t.Fatalf("prompt = %q, want unchanged", got)
	}
	if got := buildRequestPrompt(prompt, stealthOptions{codeOnly: true, noHint: true}); got != prompt {
		t.Fatalf("prompt = %q, want unchanged with --no-hint", got)
	}

	got := buildRequestPrompt(prompt, stealthOptions{codeOnly: true})
	if !strings.HasPrefix(got, prompt+"\n\n") {
		t.Fatalf("prompt = %q, want original prompt followed by instruction", got)
	}
	if !strings.Contains(got, codeOnlyInstruction) {
		t.Fatalf("prompt = %q, want code-only instruction", got)
	}
}

func TestRunStealthModeEmptyPromptExitsZero(t *testing.T) {
	if code := runStealthMode([]string{"-c"}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestRunStealthQueryUsesThinkingOffAndPrintsOnlyCode(t *testing.T) {
	const (
		salt     = "test-salt"
		expireAt = int64(1700000000000)
	)
	digest := deepSeekHashV1([]byte(fmt.Sprintf("%s_%d_0", salt, expireAt)))
	challenge := hex.EncodeToString(digest[:])
	payloadCh := make(chan map[string]any, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat_session/create":
			fmt.Fprint(w, `{"id":"session-test"}`)
		case "/chat/create_pow_challenge":
			fmt.Fprintf(w, `{"algorithm":"DeepSeekHashV1","challenge":%q,"salt":%q,"expire_at":%d,"difficulty":1,"signature":"sig","target_path":"/api/v0/chat/completion"}`,
				challenge, salt, expireAt)
		case "/chat/completion":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode completion payload: %v", err)
			}
			payloadCh <- payload
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"p\":\"response/content\",\"o\":\"APPEND\",\"v\":\"Here is the code:\\n```go\\nfmt.Println(1)\\n```\\n\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewAPIClient("test-token")
	client.baseURL = server.URL

	var out bytes.Buffer
	if err := runStealthQuery(context.Background(), client, "write code", "", true, codeFilter{}, false, true, &out); err != nil {
		t.Fatalf("runStealthQuery: %v", err)
	}
	if got, want := out.String(), "fmt.Println(1)\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}

	select {
	case payload := <-payloadCh:
		if payload["thinking_enabled"] != false {
			t.Fatalf("thinking_enabled = %v, want false", payload["thinking_enabled"])
		}
		if payload["search_enabled"] != true {
			t.Fatalf("search_enabled = %v, want true", payload["search_enabled"])
		}
		if payload["prompt"] != "write code" {
			t.Fatalf("prompt = %v, want %q", payload["prompt"], "write code")
		}
		if payload["model_type"] != "default" {
			t.Fatalf("model_type = %v, want default", payload["model_type"])
		}
	default:
		t.Fatal("completion payload was not captured")
	}
}

func TestRunStealthQueryWithFileUpload(t *testing.T) {
	tmp, err := os.CreateTemp("", "stealth-file-*.txt")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString("file data for query")
	tmp.Close()

	payloadCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat_session/create":
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"id":"session-f1"}}}`)
		case "/chat/create_pow_challenge":
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			target := req["target_path"]
			fmt.Fprintf(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"algorithm":"DeepSeekHashV1","challenge":"168efa309e5a73033fc82dde4333bbc6515da440ef961574f73f99cbe7b397ad","salt":"a40318e6b7af56bf27cf","expire_at":1790959021012,"difficulty":144000,"signature":"sig","target_path":%q}}}`, target)
		case "/file/upload_file":
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"id":"file-id-abc"}}}`)
		case "/chat/completion":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode completion payload: %v", err)
			}
			payloadCh <- payload
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"p\":\"response/content\",\"o\":\"APPEND\",\"v\":\"analyzed file\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewAPIClient("test-token")
	client.baseURL = server.URL

	var out bytes.Buffer
	if err := runStealthQuery(context.Background(), client, "summarize", tmp.Name(), false, codeFilter{}, true, true, &out); err != nil {
		t.Fatalf("runStealthQuery with file: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "analyzed file") {
		t.Fatalf("output = %q, want analyzed file", got)
	}

	select {
	case payload := <-payloadCh:
		refIDs, ok := payload["ref_file_ids"].([]any)
		if !ok || len(refIDs) != 1 || refIDs[0] != "file-id-abc" {
			t.Fatalf("ref_file_ids = %v, want ['file-id-abc']", payload["ref_file_ids"])
		}
		if payload["thinking_enabled"] != true {
			t.Fatalf("thinking_enabled = %v, want true", payload["thinking_enabled"])
		}
	default:
		t.Fatal("completion payload was not captured")
	}
}
