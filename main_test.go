package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
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

func TestRecentChatsSwitch(t *testing.T) {
	m := newModel("test-token")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(*model)

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = updated.(*model)
	if !m.recentOpen || !m.sessionsLoading {
		t.Fatalf("recent chats did not open: open=%v loading=%v", m.recentOpen, m.sessionsLoading)
	}
	if view := m.View(); !strings.Contains(view, "Recent chats") {
		t.Fatalf("recent view missing: %q", view)
	}

	updated, _ = m.Update(sessionsMsg{
		seq:     m.sessionsSeq,
		replace: true,
		sessions: []SessionInfo{
			{ID: "s1", Title: "First chat", UpdatedAt: 1782741816.158},
			{ID: "s2", Title: "Second chat", UpdatedAt: 1782740000},
		},
	})
	m = updated.(*model)
	if m.sessionsLoading || len(m.sessions) != 2 || m.sessionIndex != 0 {
		t.Fatalf("sessions not applied: %+v", m.sessions)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = updated.(*model)
	if m.sessionIndex != 1 {
		t.Fatalf("selection = %d, want 1", m.sessionIndex)
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if !m.historyLoading || m.pendingSessionID != "s2" || cmd == nil {
		t.Fatalf("history did not start: loading=%v pending=%q cmd=%v", m.historyLoading, m.pendingSessionID, cmd)
	}

	updated, _ = m.Update(historyMsg{
		seq:       m.historySeq,
		sessionID: "s2",
		history: &ChatHistory{
			SessionID:        "s2",
			Title:            "Second chat",
			CurrentMessageID: 42,
			Messages: []HistoryMessage{
				{
					MessageID: 40,
					Role:      "USER",
					Fragments: []HistoryFragment{{Type: "REQUEST", Content: "first question"}},
				},
				{
					MessageID:       42,
					Role:            "ASSISTANT",
					Content:         "first answer",
					ThinkingContent: "first reasoning",
					Status:          "FINISHED",
				},
			},
		},
	})
	m = updated.(*model)
	if m.recentOpen || m.historyLoading {
		t.Fatal("recent chats did not close after loading history")
	}
	if m.sessionID != "s2" || m.chatTitle != "Second chat" {
		t.Fatalf("session state = %q title=%q", m.sessionID, m.chatTitle)
	}
	if m.parentID == nil || *m.parentID != 42 {
		t.Fatalf("parent id = %v", m.parentID)
	}
	if len(m.messages) != 2 || m.messages[0].text != "first question" || m.messages[1].text != "first answer" {
		t.Fatalf("messages = %+v", m.messages)
	}
	if m.messages[1].thinking != "first reasoning" {
		t.Fatalf("thinking = %q", m.messages[1].thinking)
	}
	view := m.View()
	for _, want := range []string{"first question", "first answer", "first reasoning", "Second chat"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q: %q", want, view)
		}
	}
}

func TestAuthTokenMaskedByDefaultAndTogglable(t *testing.T) {
	m := newModel("")
	m.configPath = filepath.Join(t.TempDir(), "deepseek.token")

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	m = updated.(*model)

	if m.authInput.EchoMode != textinput.EchoPassword {
		t.Fatal("token input should start masked")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("secret-token")})
	m = updated.(*model)
	if got := m.authInput.Value(); got != "secret-token" {
		t.Fatalf("input value = %q", got)
	}

	masked := m.View()
	if strings.Contains(masked, "secret-token") {
		t.Fatal("masked view leaked the raw token")
	}
	if !strings.Contains(masked, strings.Repeat("•", 3)) {
		t.Fatal("masked view is missing the mask characters")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = updated.(*model)
	if m.authInput.EchoMode != textinput.EchoNormal {
		t.Fatal("ctrl+r did not reveal the token")
	}
	if view := m.View(); !strings.Contains(view, "secret-token") {
		t.Fatal("revealed view is missing the raw token")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	m = updated.(*model)
	if m.authInput.EchoMode != textinput.EchoPassword {
		t.Fatal("ctrl+r did not hide the token again")
	}
	if view := m.View(); strings.Contains(view, "secret-token") {
		t.Fatal("re-masked view leaked the raw token")
	}

	m.authInput.SetValue("")
	updated, _ = m.Update(tea.KeyMsg{
		Type:  tea.KeyRunes,
		Runes: []rune("pasted-secret"),
		Paste: true,
	})
	m = updated.(*model)
	if got := m.authInput.Value(); got != "pasted-secret" {
		t.Fatalf("pasted value = %q", got)
	}
	if view := m.View(); strings.Contains(view, "pasted-secret") {
		t.Fatal("masked view leaked a pasted token")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if m.authInput.EchoMode != textinput.EchoPassword {
		t.Fatal("echo mode was not reset after submit")
	}
}

func TestRecentChatsDeleteCurrent(t *testing.T) {
	m := newModel("test-token")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(*model)
	m.recentOpen = true
	m.sessions = []SessionInfo{{ID: "s1", Title: "First"}, {ID: "s2", Title: "Second"}}
	m.sessionIndex = 0
	m.sessionID = "s1"
	m.messages = []chatMessage{{kind: msgUser, text: "hello"}}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = updated.(*model)
	if !m.deleteConfirm {
		t.Fatal("delete confirmation did not open")
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = updated.(*model)
	if !m.deleting || m.pendingSessionID != "s1" || cmd == nil {
		t.Fatalf("delete did not start: deleting=%v pending=%q", m.deleting, m.pendingSessionID)
	}

	updated, _ = m.Update(deleteSessionMsg{seq: m.deleteSeq, sessionID: "s1"})
	m = updated.(*model)
	if m.deleting || len(m.sessions) != 1 || m.sessions[0].ID != "s2" {
		t.Fatalf("session not removed: %+v", m.sessions)
	}
	if m.sessionID != "" || len(m.messages) != 0 {
		t.Fatalf("current chat was not reset: session=%q messages=%d", m.sessionID, len(m.messages))
	}
	if !m.recentOpen || m.recentNotice != "chat deleted" {
		t.Fatalf("unexpected recent state: open=%v notice=%q", m.recentOpen, m.recentNotice)
	}
}
