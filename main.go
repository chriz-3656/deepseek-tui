package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

var version = "dev"

// -----------------------------------------------------------------------------
// styles
// -----------------------------------------------------------------------------

var (
	accentColor  = lipgloss.AdaptiveColor{Light: "#1D4ED8", Dark: "#7AA2F7"}
	readyColor   = lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#9ECE6A"}
	workingColor = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#E0AF68"}
	errorColor   = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F7768E"}
	mutedColor   = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#737AA2"}

	titleStyle     = lipgloss.NewStyle().Bold(true).Foreground(accentColor)
	userLabelStyle = lipgloss.NewStyle().Bold(true).Foreground(accentColor)
	aiLabelStyle   = lipgloss.NewStyle().Bold(true).Foreground(readyColor)
	thinkStyle     = lipgloss.NewStyle().Foreground(mutedColor).Italic(true)
	helpStyle      = lipgloss.NewStyle().Foreground(mutedColor)
	errorStyle     = lipgloss.NewStyle().Foreground(errorColor)
	cursorStyle    = lipgloss.NewStyle().Foreground(accentColor)
	readyStyle     = lipgloss.NewStyle().Foreground(readyColor)
	workingStyle   = lipgloss.NewStyle().Foreground(workingColor)
	errorStatus    = lipgloss.NewStyle().Foreground(errorColor)
)

const (
	statusIdle = iota
	statusWorking
	statusError
)

// -----------------------------------------------------------------------------
// model
// -----------------------------------------------------------------------------

type messageKind int

const (
	msgUser messageKind = iota
	msgAssistant
	msgSystem
)

type chatMessage struct {
	kind     messageKind
	text     string
	thinking string
}

type streamEventKind int

const (
	evStatus streamEventKind = iota
	evSession
	evReady
	evThink
	evReply
	evPowProgress
	evDone
	evError
)

type streamEvent struct {
	kind      streamEventKind
	text      string
	messageID int64
	done      int64
	total     int64
}

type sessionsMsg struct {
	sessions []SessionInfo
	hasMore  bool
	cursor   *SessionCursor
	replace  bool
	seq      int
	err      error
}

type historyMsg struct {
	sessionID string
	history   *ChatHistory
	seq       int
	err       error
}

type deleteSessionMsg struct {
	sessionID string
	seq       int
	err       error
}

type model struct {
	token      string
	client     *APIClient
	configPath string
	events     chan streamEvent

	viewport  viewport.Model
	input     textinput.Model
	authInput textinput.Model
	spinner   spinner.Model

	authMode  bool
	authError string

	messages  []chatMessage
	sessionID string
	chatTitle string
	parentID  *int64

	recentOpen       bool
	sessions         []SessionInfo
	sessionIndex     int
	sessionsCursor   *SessionCursor
	sessionsHasMore  bool
	sessionsLoading  bool
	sessionsError    string
	recentNotice     string
	historyLoading   bool
	pendingSessionID string
	deleteConfirm    bool
	deleting         bool
	sessionsSeq      int
	historySeq       int
	deleteSeq        int

	streaming   bool
	status      string
	statusLevel int
	powPercent  int64

	width, height int
	ready         bool
}

func newModel(token string) *model {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = "message DeepSeek…"
	input.CharLimit = 8192

	authInput := textinput.New()
	authInput.Prompt = ""
	authInput.Placeholder = "paste your userToken…"
	authInput.CharLimit = 4096
	authInput.Width = 60
	authInput.EchoMode = textinput.EchoPassword
	authInput.EchoCharacter = '•'

	spin := spinner.New()
	spin.Spinner = spinner.Dot
	spin.Style = lipgloss.NewStyle().Foreground(accentColor)

	m := &model{
		token:       token,
		configPath:  tokenPath(),
		events:      make(chan streamEvent, 1024),
		viewport:    viewport.New(0, 0),
		input:       input,
		authInput:   authInput,
		spinner:     spin,
		status:      "ready",
		statusLevel: statusIdle,
		authMode:    token == "",
	}
	if token != "" {
		m.client = NewAPIClient(token)
	} else {
		m.status = "token required"
		m.statusLevel = statusWorking
	}
	if m.authMode {
		_ = m.authInput.Focus()
	} else {
		_ = m.input.Focus()
	}
	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, textinput.Blink)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.viewport.Width = max(10, msg.Width-2)
		m.viewport.Height = max(3, msg.Height-4)
		m.input.Width = max(10, msg.Width-4)
		m.authInput.Width = max(20, min(64, msg.Width-16))
		if m.authMode {
			return m, nil
		}
		m.renderChat(false)
		return m, nil

	case tea.KeyMsg:
		if m.authMode {
			return m.updateAuth(msg)
		}
		if m.recentOpen {
			return m.updateRecent(msg)
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+r":
			if m.streaming {
				return m, nil
			}
			return m, m.openRecent()
		case "ctrl+t":
			if !m.streaming {
				m.beginAuth()
				return m, textinput.Blink
			}
			return m, nil
		case "ctrl+n":
			if !m.streaming {
				m.newChat()
			}
			return m, nil
		case "enter":
			if m.streaming {
				return m, nil
			}
			prompt := strings.TrimSpace(m.input.Value())
			if prompt == "" {
				return m, nil
			}
			m.input.SetValue("")
			m.messages = append(m.messages,
				chatMessage{kind: msgUser, text: prompt},
				chatMessage{kind: msgAssistant},
			)
			m.streaming = true
			m.powPercent = 0
			m.status = "connecting…"
			m.statusLevel = statusWorking
			m.renderChat(true)

			req := CompletionRequest{
				SessionID:       m.sessionID,
				ParentMessageID: m.parentID,
				Prompt:          prompt,
				FirstTurn:       m.parentID == nil,
			}
			return m, tea.Batch(m.runCompletion(req), m.spinner.Tick, waitForEvent(m.events))
		case "pgup", "pgdown", "up", "down", "home", "end":
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case spinner.TickMsg:
		if !m.streaming && !m.sessionsLoading && !m.historyLoading && !m.deleting {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case sessionsMsg:
		m.handleSessions(msg)
		return m, nil

	case historyMsg:
		m.handleHistory(msg)
		return m, nil

	case deleteSessionMsg:
		m.handleDelete(msg)
		return m, nil

	case streamEvent:
		m.handleStreamEvent(msg)
		if m.streaming {
			return m, waitForEvent(m.events)
		}
		return m, nil
	}

	var cmd tea.Cmd
	if m.authMode {
		m.authInput, cmd = m.authInput.Update(msg)
		return m, cmd
	}
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) View() string {
	if !m.ready {
		return "deepseek-tui\nstarting…"
	}
	if m.authMode {
		return m.authView()
	}
	if m.recentOpen {
		return m.recentView()
	}

	header := m.headerView(m.status, m.streaming)
	inputLine := lipgloss.NewStyle().Foreground(accentColor).Render("› ") + m.input.View()
	help := helpStyle.Render("enter send · ctrl+r chats · ctrl+n new chat · ctrl+t token · ctrl+c quit")

	return lipgloss.JoinVertical(lipgloss.Left, header, m.viewport.View(), inputLine, help)
}

func (m *model) headerView(status string, showSpinner bool) string {
	left := titleStyle.Render("◆ deepseek-tui")
	if m.chatTitle != "" {
		titleWidth := max(6, min(40, m.width/2))
		left += helpStyle.Render(" · " + truncateText(m.chatTitle, titleWidth))
	}
	if showSpinner {
		status = m.spinner.View() + " " + status
	}
	right := m.statusStyle().Render(status)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	return " " + left + strings.Repeat(" ", gap) + right + " "
}

func (m *model) authView() string {
	panelWidth := max(16, min(66, m.width-8))
	m.authInput.Width = max(10, panelWidth-8)

	echoHint := "ctrl+r reveal token"
	if m.authInput.EchoMode == textinput.EchoNormal {
		echoHint = "ctrl+r hide token"
	}

	lines := []string{
		titleStyle.Render("Connect to DeepSeek"),
		"",
		helpStyle.Render("Paste your web client token below."),
		helpStyle.Render("DevTools → Application → Local Storage → userToken."),
		"",
		lipgloss.NewStyle().Foreground(accentColor).Render("› ") + m.authInput.View(),
		helpStyle.Render(echoHint),
	}
	if m.authError != "" {
		lines = append(lines, "", errorStyle.Render(m.authError))
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accentColor).
		Padding(1, 2).
		Width(panelWidth).
		Render(strings.Join(lines, "\n"))

	bodyHeight := max(5, m.height-4)
	body := lipgloss.Place(m.width, bodyHeight, lipgloss.Center, lipgloss.Center, box)
	help := helpStyle.Render("enter save & start · ctrl+r show/hide · paste ctrl+v · ctrl+c quit")

	return lipgloss.JoinVertical(lipgloss.Left, m.headerView(m.status, false), body, help)
}

func (m *model) recentView() string {
	header := m.headerView(m.status, m.sessionsLoading || m.historyLoading || m.deleting)
	bodyHeight := max(5, m.viewport.Height+1)
	body := lipgloss.Place(m.width, bodyHeight, lipgloss.Center, lipgloss.Center, m.recentPanel())
	help := helpStyle.Render("↑/↓ select · enter open · n new chat · d delete · r refresh · esc close")
	return lipgloss.JoinVertical(lipgloss.Left, header, body, help)
}

func (m *model) recentRows() int {
	return max(1, max(5, m.viewport.Height+1)-8)
}

func (m *model) recentPanel() string {
	width := max(14, min(72, m.width-8))
	lines := make([]string, 0, m.recentRows()+6)

	title := truncateText("Recent chats", max(4, width-10))
	count := fmt.Sprintf("%d", len(m.sessions))
	gap := max(1, width-lipgloss.Width(title)-lipgloss.Width(count))
	lines = append(lines, titleStyle.Render(title)+strings.Repeat(" ", gap)+helpStyle.Render(count))
	lines = append(lines, "")

	if len(m.sessions) == 0 {
		if m.sessionsLoading {
			lines = append(lines, helpStyle.Render("loading chats…"))
		} else {
			lines = append(lines, helpStyle.Render("No chats yet."))
		}
	} else {
		rows := m.recentRows()
		start := 0
		if m.sessionIndex >= rows {
			start = m.sessionIndex - rows + 1
		}
		end := min(len(m.sessions), start+rows)
		for i := start; i < end; i++ {
			lines = append(lines, m.sessionRowView(m.sessions[i], i == m.sessionIndex, width))
		}
	}

	if footer := m.recentFooter(); footer != "" {
		lines = append(lines, "", footer)
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accentColor).
		Padding(1, 2).
		Width(width).
		Render(strings.Join(lines, "\n"))
}

func (m *model) recentFooter() string {
	switch {
	case m.deleteConfirm:
		name := "this chat"
		if len(m.sessions) > 0 {
			name = "“" + sessionTitle(m.sessions[m.sessionIndex]) + "”"
		}
		limit := max(12, min(72, m.width-8)-2)
		return errorStyle.Render("Delete " + truncateText(name, max(8, limit-28)) + "? y confirm · n cancel")
	case m.sessionsError != "":
		return errorStyle.Render(truncateText(m.sessionsError, max(10, min(72, m.width-12))))
	case m.historyLoading:
		return m.spinner.View() + " opening chat…"
	case m.deleting:
		return m.spinner.View() + " deleting chat…"
	case m.sessionsLoading && len(m.sessions) > 0:
		return m.spinner.View() + " refreshing chats…"
	case m.recentNotice != "":
		return readyStyle.Render(m.recentNotice)
	case m.sessionsHasMore:
		return helpStyle.Render("l load more")
	}
	return ""
}

func (m *model) sessionRowView(session SessionInfo, selected bool, width int) string {
	prefixPlain := "  "
	if selected {
		prefixPlain = "▸ "
	}
	marker := " "
	markerStyle := helpStyle
	switch {
	case session.ID == m.sessionID:
		marker = "●"
		markerStyle = readyStyle
	case session.Pinned:
		marker = "◆"
		markerStyle = cursorStyle
	}
	prefix := prefixPlain + markerStyle.Render(marker) + " "

	title := sessionTitle(session)
	stamp := ""
	if width >= 30 {
		stamp = relativeTime(float64(session.UpdatedAt))
	}
	titleWidth := width - lipgloss.Width(prefix) - lipgloss.Width(stamp)
	if stamp != "" {
		titleWidth -= 2
	}
	title = truncateText(title, max(4, titleWidth))
	titleStyled := title
	if selected {
		titleStyled = lipgloss.NewStyle().Foreground(accentColor).Bold(true).Render(title)
	}

	gap := width - lipgloss.Width(prefix) - lipgloss.Width(title) - lipgloss.Width(stamp)
	if gap < 1 {
		gap = 1
	}
	line := prefix + titleStyled + strings.Repeat(" ", gap)
	if stamp != "" {
		line += helpStyle.Render(stamp)
	}
	return line
}

func sessionTitle(session SessionInfo) string {
	title := strings.TrimSpace(session.Title)
	if title == "" {
		return "untitled chat"
	}
	return title
}

func relativeTime(epoch float64) string {
	if epoch <= 0 {
		return ""
	}
	timestamp := time.Unix(int64(epoch), 0)
	now := time.Now()
	if timestamp.After(now.Add(time.Minute)) {
		return "now"
	}
	age := now.Sub(timestamp)
	switch {
	case age < time.Minute:
		return "now"
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	case age < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(age.Hours()/24))
	case timestamp.Year() == now.Year():
		return timestamp.Format("Jan 2")
	default:
		return timestamp.Format("Jan 2 2006")
	}
}

func truncateText(text string, width int) string {
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(text) <= width {
		return text
	}
	if width == 1 {
		return "…"
	}
	var out strings.Builder
	used := 0
	for _, r := range text {
		rw := lipgloss.Width(string(r))
		if used+rw > width-1 {
			break
		}
		out.WriteRune(r)
		used += rw
	}
	return out.String() + "…"
}

func (m *model) statusStyle() lipgloss.Style {
	switch m.statusLevel {
	case statusWorking:
		return workingStyle
	case statusError:
		return errorStatus
	default:
		return readyStyle
	}
}

func (m *model) newChat() {
	m.closeRecent()
	m.messages = nil
	m.sessionID = ""
	m.chatTitle = ""
	m.parentID = nil
	m.streaming = false
	m.powPercent = 0
	m.status = "ready"
	m.statusLevel = statusIdle
	m.viewport.SetContent("")
	m.renderChat(false)
}

// closeRecent leaves the recent-chats screen and invalidates any in-flight
// list/history/delete replies.
func (m *model) closeRecent() {
	m.recentOpen = false
	m.sessionsLoading = false
	m.historyLoading = false
	m.deleting = false
	m.deleteConfirm = false
	m.pendingSessionID = ""
	m.sessionsError = ""
	m.recentNotice = ""
	m.sessionsSeq++
	m.historySeq++
	m.deleteSeq++
	_ = m.input.Focus()
	if !m.streaming {
		m.status = "ready"
		m.statusLevel = statusIdle
	}
}

// openRecent shows the chat drawer and refreshes its first page. The previous
// list stays visible while the refresh runs.
func (m *model) openRecent() tea.Cmd {
	if m.client == nil {
		return nil
	}
	m.recentOpen = true
	m.sessionsError = ""
	m.recentNotice = ""
	m.deleteConfirm = false
	m.deleting = false
	m.pendingSessionID = ""
	m.historyLoading = false
	m.sessionsLoading = true
	m.input.Blur()
	m.status = "loading chats…"
	m.statusLevel = statusWorking
	m.selectCurrentSession()
	m.sessionsSeq++
	seq := m.sessionsSeq
	return tea.Batch(m.loadSessions(true, seq), m.spinner.Tick)
}

func (m *model) selectCurrentSession() {
	if m.sessionID != "" {
		for i := range m.sessions {
			if m.sessions[i].ID == m.sessionID {
				m.sessionIndex = i
				return
			}
		}
	}
	m.clampSessionIndex()
}

func (m *model) clampSessionIndex() {
	if len(m.sessions) == 0 {
		m.sessionIndex = 0
		return
	}
	if m.sessionIndex < 0 {
		m.sessionIndex = 0
	}
	if m.sessionIndex >= len(m.sessions) {
		m.sessionIndex = len(m.sessions) - 1
	}
}

func (m *model) moveSession(delta int) {
	if len(m.sessions) == 0 {
		return
	}
	m.sessionIndex += delta
	m.clampSessionIndex()
}

func (m *model) updateRecent(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}

	if m.deleteConfirm {
		switch msg.String() {
		case "y":
			if len(m.sessions) == 0 || m.deleting {
				return m, nil
			}
			target := m.sessions[m.sessionIndex]
			m.deleteConfirm = false
			m.deleting = true
			m.pendingSessionID = target.ID
			m.sessionsError = ""
			m.recentNotice = ""
			m.status = "deleting chat…"
			m.statusLevel = statusWorking
			m.deleteSeq++
			seq := m.deleteSeq
			return m, tea.Batch(m.loadDelete(target.ID, seq), m.spinner.Tick)
		case "n", "esc":
			m.deleteConfirm = false
			return m, nil
		}
		return m, nil
	}

	switch msg.String() {
	case "esc":
		m.closeRecent()
		return m, nil
	case "ctrl+t":
		m.closeRecent()
		m.beginAuth()
		return m, textinput.Blink
	case "ctrl+n", "n":
		m.newChat()
		return m, textinput.Blink
	}

	if m.historyLoading || m.deleting {
		return m, nil
	}

	switch msg.String() {
	case "up", "k":
		m.moveSession(-1)
	case "down", "j":
		m.moveSession(1)
	case "pgup":
		m.moveSession(-max(1, m.recentRows()))
	case "pgdown":
		m.moveSession(max(1, m.recentRows()))
	case "home":
		m.sessionIndex = 0
	case "end":
		if len(m.sessions) > 0 {
			m.sessionIndex = len(m.sessions) - 1
		}
	case "r":
		if !m.sessionsLoading {
			m.sessionsLoading = true
			m.sessionsError = ""
			m.recentNotice = ""
			m.status = "loading chats…"
			m.statusLevel = statusWorking
			m.sessionsSeq++
			seq := m.sessionsSeq
			return m, tea.Batch(m.loadSessions(true, seq), m.spinner.Tick)
		}
	case "l":
		if m.sessionsHasMore && !m.sessionsLoading && m.sessionsCursor != nil {
			m.sessionsLoading = true
			m.sessionsError = ""
			m.recentNotice = ""
			m.status = "loading more chats…"
			m.statusLevel = statusWorking
			m.sessionsSeq++
			seq := m.sessionsSeq
			return m, tea.Batch(m.loadSessions(false, seq), m.spinner.Tick)
		}
	case "enter":
		if len(m.sessions) == 0 {
			return m, nil
		}
		selected := m.sessions[m.sessionIndex]
		if selected.ID == m.sessionID {
			m.closeRecent()
			return m, nil
		}
		m.historyLoading = true
		m.pendingSessionID = selected.ID
		m.sessionsError = ""
		m.recentNotice = ""
		m.status = "opening chat…"
		m.statusLevel = statusWorking
		m.historySeq++
		seq := m.historySeq
		return m, tea.Batch(m.loadHistory(selected.ID, seq), m.spinner.Tick)
	case "d":
		if len(m.sessions) > 0 {
			m.deleteConfirm = true
			m.sessionsError = ""
			m.recentNotice = ""
		}
	}
	return m, nil
}

func (m *model) loadSessions(replace bool, seq int) tea.Cmd {
	client := m.client
	if client == nil {
		return nil
	}
	var cursor *SessionCursor
	if !replace {
		cursor = m.sessionsCursor
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		page, err := client.FetchSessions(ctx, cursor, 50)
		if err != nil {
			return sessionsMsg{seq: seq, replace: replace, cursor: cursor, err: err}
		}
		return sessionsMsg{
			sessions: page.Sessions,
			hasMore:  page.HasMore,
			cursor:   cursor,
			replace:  replace,
			seq:      seq,
		}
	}
}

func (m *model) loadHistory(sessionID string, seq int) tea.Cmd {
	client := m.client
	if client == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		history, err := client.FetchHistory(ctx, sessionID)
		return historyMsg{sessionID: sessionID, history: history, seq: seq, err: err}
	}
}

func (m *model) loadDelete(sessionID string, seq int) tea.Cmd {
	client := m.client
	if client == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return deleteSessionMsg{sessionID: sessionID, seq: seq, err: client.DeleteSession(ctx, sessionID)}
	}
}

func (m *model) handleSessions(msg sessionsMsg) {
	if msg.seq != m.sessionsSeq || !m.recentOpen {
		return
	}
	m.sessionsLoading = false
	if msg.err != nil {
		m.sessionsError = friendlyError(msg.err)
		m.status = "chats error"
		m.statusLevel = statusError
		return
	}

	selectedID := ""
	if m.sessionIndex >= 0 && m.sessionIndex < len(m.sessions) {
		selectedID = m.sessions[m.sessionIndex].ID
	}
	if msg.replace {
		m.sessions = dedupeSessions(msg.sessions)
	} else {
		m.sessions = dedupeSessions(append(m.sessions, msg.sessions...))
	}

	if selectedID != "" {
		m.sessionIndex = -1
		for i := range m.sessions {
			if m.sessions[i].ID == selectedID {
				m.sessionIndex = i
				break
			}
		}
		if m.sessionIndex < 0 {
			m.selectCurrentSession()
		}
	} else {
		m.selectCurrentSession()
	}
	m.clampSessionIndex()

	next := sessionCursorAfter(msg.sessions, msg.hasMore)
	if next != nil && msg.cursor != nil && next.Pinned == msg.cursor.Pinned && next.UpdatedAt == msg.cursor.UpdatedAt {
		next = nil
	}
	m.sessionsCursor = next
	m.sessionsHasMore = next != nil
	m.sessionsError = ""
	if m.historyLoading || m.deleting {
		return
	}
	if len(m.sessions) == 0 {
		m.status = "no chats"
		m.statusLevel = statusIdle
		return
	}
	m.status = fmt.Sprintf("%d chats", len(m.sessions))
	m.statusLevel = statusIdle
}

func (m *model) handleHistory(msg historyMsg) {
	if msg.seq != m.historySeq || !m.recentOpen {
		return
	}
	m.historyLoading = false
	m.pendingSessionID = ""
	if msg.err != nil {
		m.sessionsError = friendlyError(msg.err)
		m.status = "chat error"
		m.statusLevel = statusError
		return
	}
	if msg.history == nil {
		m.sessionsError = "deepseek: empty history response"
		m.status = "chat error"
		m.statusLevel = statusError
		return
	}

	m.messages = historyToChatMessages(msg.history)
	m.sessionID = msg.sessionID
	m.chatTitle = msg.history.Title
	m.parentID = historyParentID(msg.history)
	m.streaming = false
	m.powPercent = 0
	m.recentOpen = false
	m.deleteConfirm = false
	m.sessionsError = ""
	m.recentNotice = ""
	m.status = "ready"
	m.statusLevel = statusIdle
	_ = m.input.Focus()
	m.renderChat(true)
}

func (m *model) handleDelete(msg deleteSessionMsg) {
	if msg.seq != m.deleteSeq || !m.recentOpen {
		return
	}
	m.deleting = false
	m.pendingSessionID = ""
	if msg.err != nil {
		m.sessionsError = friendlyError(msg.err)
		m.status = "delete failed"
		m.statusLevel = statusError
		return
	}

	for i := range m.sessions {
		if m.sessions[i].ID == msg.sessionID {
			m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
			break
		}
	}
	if msg.sessionID == m.sessionID {
		m.messages = nil
		m.sessionID = ""
		m.chatTitle = ""
		m.parentID = nil
		m.streaming = false
		m.powPercent = 0
		m.viewport.SetContent("")
		m.renderChat(false)
	}
	m.clampSessionIndex()
	m.sessionsError = ""
	m.recentNotice = "chat deleted"
	m.status = "chat deleted"
	m.statusLevel = statusIdle
}

func dedupeSessions(sessions []SessionInfo) []SessionInfo {
	seen := make(map[string]bool, len(sessions))
	out := make([]SessionInfo, 0, len(sessions))
	for _, session := range sessions {
		if session.ID == "" || seen[session.ID] {
			continue
		}
		seen[session.ID] = true
		out = append(out, session)
	}
	return out
}

func sessionCursorAfter(sessions []SessionInfo, hasMore bool) *SessionCursor {
	if !hasMore || len(sessions) == 0 {
		return nil
	}
	last := sessions[len(sessions)-1]
	if float64(last.UpdatedAt) == 0 {
		return nil
	}
	return &SessionCursor{Pinned: last.Pinned, UpdatedAt: float64(last.UpdatedAt)}
}

func historyToChatMessages(history *ChatHistory) []chatMessage {
	if history == nil {
		return nil
	}
	messages := make([]chatMessage, 0, len(history.Messages))
	for _, message := range history.Messages {
		switch strings.ToUpper(strings.TrimSpace(message.Role)) {
		case "USER":
			text := message.UserText()
			if text == "" {
				continue
			}
			messages = append(messages, chatMessage{kind: msgUser, text: text})
		case "ASSISTANT":
			text := message.AssistantText()
			thinking := message.AssistantThinking()
			if text == "" && thinking == "" {
				continue
			}
			messages = append(messages, chatMessage{
				kind:     msgAssistant,
				text:     text,
				thinking: thinking,
			})
		}
	}
	return messages
}

func historyParentID(history *ChatHistory) *int64 {
	if history == nil {
		return nil
	}
	if history.CurrentMessageID != 0 {
		for _, message := range history.Messages {
			if int64(message.MessageID) == history.CurrentMessageID {
				id := history.CurrentMessageID
				return &id
			}
		}
	}
	for i := len(history.Messages) - 1; i >= 0; i-- {
		if history.Messages[i].MessageID != 0 {
			id := int64(history.Messages[i].MessageID)
			return &id
		}
	}
	return nil
}

// beginAuth switches the TUI into token entry, used on first run and ctrl+t.
func (m *model) beginAuth() {
	m.authMode = true
	m.authError = ""
	m.authInput.SetValue("")
	m.authInput.EchoMode = textinput.EchoPassword
	_ = m.authInput.Focus()
	m.input.Blur()
	m.status = "token required"
	m.statusLevel = statusWorking
}

func (m *model) updateAuth(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.token != "" {
			m.authMode = false
			m.authError = ""
			m.authInput.Blur()
			_ = m.input.Focus()
			m.status = "ready"
			m.statusLevel = statusIdle
			return m, textinput.Blink
		}
		return m, tea.Quit
	case "enter":
		return m.submitAuth()
	case "ctrl+r":
		if m.authInput.EchoMode == textinput.EchoPassword {
			m.authInput.EchoMode = textinput.EchoNormal
		} else {
			m.authInput.EchoMode = textinput.EchoPassword
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.authInput, cmd = m.authInput.Update(msg)
	return m, cmd
}

func (m *model) submitAuth() (tea.Model, tea.Cmd) {
	token := normalizeToken(m.authInput.Value())
	if token == "" {
		m.authError = "token is empty"
		return m, nil
	}

	m.authError = ""
	m.token = token
	m.client = NewAPIClient(token)
	saveErr := saveToken(m.configPath, token)

	m.authMode = false
	m.authInput.SetValue("")
	m.authInput.EchoMode = textinput.EchoPassword
	m.authInput.Blur()
	_ = m.input.Focus()
	m.newChat()
	m.sessions = nil
	m.sessionIndex = 0
	m.sessionsCursor = nil
	m.sessionsHasMore = false

	if saveErr != nil {
		m.messages = append(m.messages, chatMessage{
			kind: msgSystem,
			text: "token is active, but saving " + m.configPath + " failed: " + saveErr.Error(),
		})
		m.renderChat(true)
	}
	return m, textinput.Blink
}

func (m *model) handleStreamEvent(event streamEvent) {
	switch event.kind {
	case evStatus:
		m.status = event.text
		m.statusLevel = statusWorking

	case evSession:
		m.sessionID = event.text

	case evReady:
		if event.messageID != 0 {
			id := event.messageID
			m.parentID = &id
		}

	case evPowProgress:
		if event.total > 0 {
			pct := event.done * 100 / event.total
			if pct > 100 {
				pct = 100
			}
			if pct < m.powPercent {
				pct = m.powPercent
			}
			m.powPercent = pct
			m.status = fmt.Sprintf("solving proof-of-work %d%%", pct)
		}

	case evThink, evReply:
		m.appendChunk(event.kind, event.text)
		m.renderChat(true)

	case evDone:
		m.streaming = false
		m.status = "ready"
		m.statusLevel = statusIdle
		if last := m.lastAssistant(); last != nil && last.text == "" && last.thinking == "" {
			last.text = "(no content returned)"
		}
		m.renderChat(true)

	case evError:
		m.streaming = false
		m.status = "error"
		m.statusLevel = statusError
		if last := m.lastAssistant(); last != nil && last.text == "" && last.thinking == "" && len(m.messages) > 0 {
			m.messages = m.messages[:len(m.messages)-1]
		}
		m.messages = append(m.messages, chatMessage{kind: msgSystem, text: event.text})
		m.renderChat(true)
	}
}

func (m *model) appendChunk(kind streamEventKind, text string) {
	if text == "" {
		return
	}
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].kind != msgAssistant {
		m.messages = append(m.messages, chatMessage{kind: msgAssistant})
	}
	last := &m.messages[len(m.messages)-1]
	if kind == evThink {
		last.thinking += text
	} else {
		last.text += text
	}
}

func (m *model) lastAssistant() *chatMessage {
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].kind == msgAssistant {
			return &m.messages[i]
		}
	}
	return nil
}

func (m *model) renderChat(scrollToBottom bool) {
	width := m.viewport.Width
	if width <= 0 {
		width = 80
	}

	var b strings.Builder
	if len(m.messages) == 0 {
		b.WriteString(helpStyle.Render("DeepSeek chat in your terminal."))
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("Type a message and press enter."))
	}

	for i, message := range m.messages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		switch message.kind {
		case msgUser:
			b.WriteString(userLabelStyle.Render("you"))
			b.WriteString("\n")
			b.WriteString(wrapText(message.text, width))

		case msgAssistant:
			b.WriteString(aiLabelStyle.Render("deepseek"))
			if message.thinking != "" {
				b.WriteString("\n")
				b.WriteString(thinkStyle.Render(wrapText(message.thinking, width)))
			}
			if message.text != "" {
				b.WriteString("\n")
				b.WriteString(wrapText(message.text, width))
			}
			if m.streaming && i == len(m.messages)-1 {
				if message.text == "" && message.thinking == "" {
					b.WriteString("\n")
					b.WriteString(thinkStyle.Render("…"))
				} else {
					b.WriteString(cursorStyle.Render("▍"))
				}
			}

		case msgSystem:
			b.WriteString(errorStyle.Render(wrapText(message.text, width)))
		}
	}

	wasAtBottom := m.viewport.AtBottom()
	m.viewport.SetContent(strings.TrimRight(b.String(), "\n"))
	if scrollToBottom || (m.streaming && wasAtBottom) {
		m.viewport.GotoBottom()
	}
}

func (m *model) runCompletion(req CompletionRequest) tea.Cmd {
	client := m.client
	events := m.events
	if client == nil {
		return func() tea.Msg {
			return streamEvent{kind: evError, text: "no DeepSeek token configured"}
		}
	}
	return func() tea.Msg {
		go func() {
			ctx := context.Background()

			if req.SessionID == "" {
				events <- streamEvent{kind: evStatus, text: "creating chat session…"}
				sessionID, err := client.CreateSession(ctx)
				if err != nil {
					events <- streamEvent{kind: evError, text: friendlyError(err)}
					return
				}
				req.SessionID = sessionID
				events <- streamEvent{kind: evSession, text: sessionID}
			}

			err := client.Complete(ctx, req, CompletionHooks{
				OnStatus: func(text string) {
					events <- streamEvent{kind: evStatus, text: text}
				},
				OnPowProgress: func(done, total int64) {
					events <- streamEvent{kind: evPowProgress, done: done, total: total}
				},
				OnEvent: func(event StreamEvent) {
					switch event.Kind {
					case StreamReady:
						events <- streamEvent{kind: evReady, messageID: event.MessageID}
					case StreamThink:
						events <- streamEvent{kind: evThink, text: event.Text}
					case StreamReply:
						events <- streamEvent{kind: evReply, text: event.Text}
					}
				},
			})
			if err != nil {
				events <- streamEvent{kind: evError, text: friendlyError(err)}
				return
			}
			events <- streamEvent{kind: evDone}
		}()
		return nil
	}
}

func waitForEvent(events <-chan streamEvent) tea.Cmd {
	return func() tea.Msg {
		return <-events
	}
}

func friendlyError(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case 401, 403:
			return "token rejected by DeepSeek — refresh DEEPSEEK_TOKEN or " + tokenPath()
		case 429:
			return "rate limited by DeepSeek — wait a moment and try again"
		}
		return apiErr.Error()
	}
	return err.Error()
}

// -----------------------------------------------------------------------------
// text layout
// -----------------------------------------------------------------------------

func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}
	var out strings.Builder
	paragraphs := strings.Split(text, "\n")
	for pi, paragraph := range paragraphs {
		if pi > 0 {
			out.WriteByte('\n')
		}
		if paragraph == "" {
			continue
		}
		lineWidth := 0
		for _, word := range strings.Fields(paragraph) {
			wordWidth := lipgloss.Width(word)
			if lineWidth > 0 && lineWidth+1+wordWidth > width {
				out.WriteByte('\n')
				lineWidth = 0
			}
			if lineWidth > 0 {
				out.WriteByte(' ')
				lineWidth++
			}
			if wordWidth > width && width > 1 {
				runes := []rune(word)
				chunk := make([]rune, 0, len(runes))
				chunkWidth := 0
				for _, r := range runes {
					rw := lipgloss.Width(string(r))
					if chunkWidth > 0 && chunkWidth+rw > width {
						out.WriteString(string(chunk))
						out.WriteByte('\n')
						chunk = chunk[:0]
						chunkWidth = 0
					}
					chunk = append(chunk, r)
					chunkWidth += rw
				}
				out.WriteString(string(chunk))
				lineWidth = chunkWidth
				continue
			}
			out.WriteString(word)
			lineWidth += wordWidth
		}
	}
	return out.String()
}

// -----------------------------------------------------------------------------
// auth
// -----------------------------------------------------------------------------

func normalizeToken(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	if len(s) >= 7 && strings.EqualFold(s[:7], "bearer ") {
		s = strings.TrimSpace(s[7:])
	}
	return s
}

func storedToken() string {
	if token := normalizeToken(os.Getenv("DEEPSEEK_TOKEN")); token != "" {
		return token
	}
	data, err := os.ReadFile(tokenPath())
	if err != nil {
		return ""
	}
	return normalizeToken(string(data))
}

func saveToken(path, token string) error {
	return writePrivateFile(path, token+"\n")
}

// -----------------------------------------------------------------------------
// entry point
// -----------------------------------------------------------------------------

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	tokenFlag := flag.String("token", "", "DeepSeek web userToken (overrides env and config file)")
	flag.Parse()

	if *showVersion {
		fmt.Println("deepseek-tui " + version)
		return
	}

	token := normalizeToken(*tokenFlag)
	if token == "" {
		token = storedToken()
	}
	if token == "" && !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "deepseek-tui: no DeepSeek token found.")
		fmt.Fprintln(os.Stderr, "Set DEEPSEEK_TOKEN, write ~/.config/deepseek.token, pass --token, or run from a terminal.")
		os.Exit(1)
	}

	program := tea.NewProgram(newModel(token), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "deepseek-tui:", err)
		os.Exit(1)
	}
}
