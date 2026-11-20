package test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testBinary string

func TestMain(m *testing.M) {
	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to find repo root: %v\n", err)
		os.Exit(1)
	}

	tmpDir, err := os.MkdirTemp("", "deepseek-tui-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	bin := filepath.Join(tmpDir, "deepseek-tui")
	buildCmd := exec.Command("go", "build", "-trimpath", "-ldflags=-X main.version=v0.6.0-test", "-o", bin, ".")
	buildCmd.Dir = root
	if out, err := buildCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to compile test binary: %v\n%s\n", err, out)
		os.Exit(1)
	}
	testBinary = bin

	code := m.Run()
	os.Exit(code)
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found in parent hierarchy")
		}
		dir = parent
	}
}

func runCLI(t *testing.T, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, testBinary, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to execute binary: %v", err)
		}
	}

	return outBuf.String(), errBuf.String(), exitCode
}

func TestCLIVersion(t *testing.T) {
	stdout, stderr, code := runCLI(t, nil, "--version")
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", code, stderr)
	}
	want := "deepseek-tui v0.6.0-test\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestCLIHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		stdout, stderr, code := runCLI(t, nil, flag)
		if code != 0 {
			t.Fatalf("flag %q: expected exit code 0, got %d, stderr: %s", flag, code, stderr)
		}
		for _, want := range []string{
			"deepseek-tui",
			"-c, --code",
			"-1, --first",
			"-l, --lang",
			"-f, --file",
			"-e, --export",
			"--think",
			"--no-think",
			"--search",
			"--no-search",
			"--token",
			"ctrl+d",
			"ctrl+s",
			"esc",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("flag %q: output missing expected substring %q", flag, want)
			}
		}
	}
}

func TestCLIArgValidation(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantErrSub string
	}{
		{
			name:       "missing file arg",
			args:       []string{"--file"},
			wantCode:   2,
			wantErrSub: "--file requires a file path",
		},
		{
			name:       "missing export arg",
			args:       []string{"--export"},
			wantCode:   2,
			wantErrSub: "--export requires a session ID",
		},
		{
			name:       "missing lang arg",
			args:       []string{"--lang"},
			wantCode:   2,
			wantErrSub: "--lang requires a value",
		},
		{
			name:       "missing token arg",
			args:       []string{"--token"},
			wantCode:   2,
			wantErrSub: "--token requires a value",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := runCLI(t, nil, tc.args...)
			if code != tc.wantCode {
				t.Fatalf("args %v: exit code = %d, want %d (stderr: %s)", tc.args, code, tc.wantCode, stderr)
			}
			if !strings.Contains(stderr, tc.wantErrSub) {
				t.Fatalf("args %v: stderr %q does not contain %q", tc.args, stderr, tc.wantErrSub)
			}
		})
	}
}

func TestCLIExportNoToken(t *testing.T) {
	isolatedEnv := []string{
		"DEEPSEEK_TOKEN=",
		"HOME=" + t.TempDir(),
		"XDG_CONFIG_HOME=" + t.TempDir(),
	}

	_, stderr, code := runCLI(t, isolatedEnv, "--export", "sess-test-123")
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "no DeepSeek token found") && !strings.Contains(stderr, "Set DEEPSEEK_TOKEN") {
		t.Fatalf("stderr %q missing token warning", stderr)
	}
}

func TestCLIAttachFileMissing(t *testing.T) {
	_, stderr, code := runCLI(t, nil, "--file", "/nonexistent/path/for/sure/file.txt", "hello")
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "stat /nonexistent/path/for/sure/file.txt: no such file or directory") {
		t.Fatalf("stderr %q missing expected file error", stderr)
	}
}

func TestCLIStealthCodeExtraction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat_session/create":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"biz_code":0,"biz_msg":"","biz_data":{"id":"sess-e2e-1"}}}`)
		case "/chat/create_pow_challenge":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{
				"code": 0,
				"msg": "",
				"data": {
					"biz_code": 0,
					"biz_msg": "",
					"biz_data": {
						"algorithm": "DeepSeekHashV1",
						"challenge": "168efa309e5a73033fc82dde4333bbc6515da440ef961574f73f99cbe7b397ad",
						"salt": "a40318e6b7af56bf27cf",
						"expire_at": 1790959021012,
						"difficulty": 60000,
						"signature": "test-signature",
						"target_path": "/api/v0/chat/completion"
					}
				}
			}`)
		case "/chat/completion":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "no flusher", http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(w, "data: {\"p\":\"response/content\",\"o\":\"APPEND\",\"v\":\"Here is the code:\\n```python\\nprint('hello from e2e')\\n```\\nDone.\"}\n\n")
			flusher.Flush()
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	env := []string{
		"DEEPSEEK_BASE_URL=" + server.URL,
		"DEEPSEEK_TOKEN=test-token-valid",
	}

	stdout, stderr, code := runCLI(t, env, "-c", "--first", "--lang", "python", "generate python script")
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", code, stderr)
	}
	want := "print('hello from e2e')\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}

	// Test no matching block exits with code 3
	_, stderr, code = runCLI(t, env, "-c", "--lang", "rust", "generate python script")
	if code != 3 {
		t.Fatalf("expected exit code 3 for unmatched lang, got %d, stderr: %s", code, stderr)
	}
}

func TestCLIExportSessionMarkdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/history_messages" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{
				"code": 0,
				"msg": "",
				"data": {
					"biz_code": 0,
					"biz_msg": "",
					"biz_data": {
						"chat_session": {
							"id": "sess-export-999",
							"title": "E2E Export Test"
						},
						"chat_messages": [
							{
								"message_id": 1,
								"role": "USER",
								"fragments": [{"type": "REQUEST", "content": "How do I test CLIs in Go?"}]
							},
							{
								"message_id": 2,
								"role": "ASSISTANT",
								"fragments": [
									{"type": "THINK", "content": "Thinking about exec.Command..."},
									{"type": "RESPONSE", "content": "Use os/exec and compile the binary in TestMain."}
								]
							}
						]
					}
				}
			}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	env := []string{
		"DEEPSEEK_BASE_URL=" + server.URL,
		"DEEPSEEK_TOKEN=test-token-valid",
	}

	stdout, stderr, code := runCLI(t, env, "--export", "sess-export-999")
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d, stderr: %s", code, stderr)
	}

	for _, sub := range []string{
		"# E2E Export Test",
		"### User",
		"How do I test CLIs in Go?",
		"<summary>Thinking Process</summary>",
		"Thinking about exec.Command...",
		"### DeepSeek",
		"Use os/exec and compile the binary in TestMain.",
	} {
		if !strings.Contains(stdout, sub) {
			t.Errorf("export output missing %q", sub)
		}
	}
}
