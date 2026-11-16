package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	accentColor = lipgloss.AdaptiveColor{Light: "#1D4ED8", Dark: "#7AA2F7"}
	readyColor  = lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#9ECE6A"}
	workingColor = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#E0AF68"}
	errorColor  = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F7768E"}
	mutedColor  = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#737AA2"}

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
	parentID  *int64

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

	spin := spinner.New()
	spin.Spinner = spinner.Dot
	spin.Style = lipgloss.NewStyle().Foreground(accentColor)

	m := &model{
		token:      token,
		configPath: tokenPath(),
		events:     make(chan streamEvent, 1024),
		viewport:   viewport.New(0, 0),
		input:      input,
		authInput:  authInput,
		spinner:    spin,
		status:     "ready",
		statusLevel: statusIdle,
		authMode:   token == "",
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
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
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
				FirstTurn:       m.sessionID == "",
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
		if !m.streaming {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

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

	header := m.headerView(m.status, m.streaming)
	inputLine := lipgloss.NewStyle().Foreground(accentColor).Render("› ") + m.input.View()
	help := helpStyle.Render("enter send · ctrl+n new chat · ctrl+t token · pgup/pgdown scroll · ctrl+c quit")

	return lipgloss.JoinVertical(lipgloss.Left, header, m.viewport.View(), inputLine, help)
}

func (m *model) headerView(status string, showSpinner bool) string {
	left := titleStyle.Render("◆ deepseek-tui")
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

	lines := []string{
		titleStyle.Render("Connect to DeepSeek"),
		"",
		helpStyle.Render("Paste your web client token below."),
		helpStyle.Render("DevTools → Application → Local Storage → userToken."),
		"",
		lipgloss.NewStyle().Foreground(accentColor).Render("› ") + m.authInput.View(),
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
	help := helpStyle.Render("enter save & start · paste with ctrl+v / right-click · ctrl+c quit")

	return lipgloss.JoinVertical(lipgloss.Left, m.headerView(m.status, false), body, help)
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
	m.messages = nil
	m.sessionID = ""
	m.parentID = nil
	m.streaming = false
	m.powPercent = 0
	m.status = "ready"
	m.statusLevel = statusIdle
	m.viewport.SetContent("")
	m.renderChat(false)
}

// beginAuth switches the TUI into token entry, used on first run and ctrl+t.
func (m *model) beginAuth() {
	m.authMode = true
	m.authError = ""
	m.authInput.SetValue("")
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
	m.authInput.Blur()
	_ = m.input.Focus()
	m.newChat()

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

func tokenPath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "deepseek.token")
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "deepseek.token")
	}
	return "deepseek.token"
}

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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token+"\n"), 0o600)
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
