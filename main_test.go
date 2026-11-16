package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestWrapText(t *testing.T) {
	got := wrapText("alpha beta gamma delta", 11)
	want := "alpha beta\ngamma delta"
	if got != want {
		t.Fatalf("wrap = %q, want %q", got, want)
	}

	got = wrapText("abcdefghijklmnop", 5)
	if !strings.Contains(got, "\n") {
		t.Fatalf("long word was not hard-wrapped: %q", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if lipgloss.Width(line) > 5 {
			t.Fatalf("line %q exceeds width", line)
		}
	}
}

func TestModelResizeAndView(t *testing.T) {
	m := newModel("test-token")
	if !strings.Contains(m.View(), "starting") {
		t.Fatalf("unexpected pre-resize view: %q", m.View())
	}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*model)
	if view := m.View(); !strings.Contains(view, "deepseek-tui") || !strings.Contains(view, "ready") {
		t.Fatalf("unexpected view: %q", view)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	m = updated.(*model)
	if !strings.Contains(m.View(), "hello") {
		t.Fatalf("typed text missing from view")
	}

	m.messages = append(m.messages, chatMessage{kind: msgAssistant})
	m.streaming = true
	m.handleStreamEvent(streamEvent{kind: evReply, text: "streamed reply"})
	m.handleStreamEvent(streamEvent{kind: evDone})
	if view := m.View(); !strings.Contains(view, "streamed reply") {
		t.Fatalf("streamed reply missing from view: %q", view)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
	m = updated.(*model)
	if len(m.messages) != 0 || m.sessionID != "" || m.streaming {
		t.Fatalf("new chat did not reset state")
	}
}

func TestAuthAcceptsTypedAndPastedToken(t *testing.T) {
	m := newModel("")
	m.configPath = filepath.Join(t.TempDir(), "deepseek.token")

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(*model)
	if !m.authMode {
		t.Fatal("expected auth mode without a token")
	}
	if view := m.View(); !strings.Contains(view, "Connect to DeepSeek") {
		t.Fatalf("auth view missing: %q", view)
	}

	updated, _ = m.Update(tea.KeyMsg{
		Type:  tea.KeyRunes,
		Runes: []rune("pasted-token"),
		Paste: true,
	})
	m = updated.(*model)
	if got := m.authInput.Value(); got != "pasted-token" {
		t.Fatalf("pasted value = %q", got)
	}

	m.authInput.SetValue("")
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("typed-token")})
	m = updated.(*model)
	if got := m.authInput.Value(); got != "typed-token" {
		t.Fatalf("typed value = %q", got)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if m.authMode {
		t.Fatal("auth mode did not exit after enter")
	}
	if m.token != "typed-token" || m.client == nil {
		t.Fatalf("token/client not set")
	}

	data, err := os.ReadFile(m.configPath)
	if err != nil {
		t.Fatalf("token file: %v", err)
	}
	if strings.TrimSpace(string(data)) != "typed-token" {
		t.Fatalf("saved token = %q", data)
	}
}
