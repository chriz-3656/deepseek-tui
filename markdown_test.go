package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestRenderMarkdownHeadingsAndInline(t *testing.T) {
	input := "# Release notes\n\n" +
		"This is **bold**, *italic*, ~~old~~, `code`, and [docs](https://example.test).\n\n" +
		"## Details\n\nA second paragraph with a soft\nline break."
	out := renderMarkdown(input, 64)

	for _, want := range []string{"Release notes", "Details", "This is bold", "italic", "old", "code", "docs", "line break"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	for _, marker := range []string{"**", "~~", "`code`", "##", "[docs]"} {
		if strings.Contains(out, marker) {
			t.Fatalf("raw marker %q leaked into:\n%s", marker, out)
		}
	}
	assertWithinWidth(t, out, 64)
}

func TestRenderMarkdownCodeFencePreservesWhitespace(t *testing.T) {
	input := "Before\n\n```go\nfunc main() {\n\tif true {\n\t\tfmt.Println(\"hi\")\n\t}\n}\n```\n\nAfter"
	out := renderMarkdown(input, 60)

	for _, want := range []string{
		"go",
		"│ func main() {",
		"│     if true {",
		"│         fmt.Println(\"hi\")",
		"│ }",
		"After",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "```") {
		t.Fatalf("fence marker leaked:\n%s", out)
	}
	assertWithinWidth(t, out, 60)
}

func TestRenderMarkdownUnclosedFenceWhileStreaming(t *testing.T) {
	out := renderMarkdown("Here is the code:\n\n```python\nprint('hi')", 50)
	if !strings.Contains(out, "python") || !strings.Contains(out, "│ print('hi')") {
		t.Fatalf("open fence not rendered as code:\n%s", out)
	}
	if strings.Contains(out, "```") {
		t.Fatalf("fence marker leaked:\n%s", out)
	}
}

func TestRenderMarkdownListsQuotesAndTasks(t *testing.T) {
	input := "- one\n- two\n  - nested\n\n" +
		"1. first\n2. second\n\n" +
		"- [x] shipped\n- [ ] pending\n\n" +
		"> quoted **text**"
	out := renderMarkdown(input, 60)

	for _, want := range []string{"• one", "◦ nested", "1. first", "2. second", "✓ shipped", "○ pending", "│ quoted text"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "**") {
		t.Fatalf("inline marker leaked:\n%s", out)
	}
	assertWithinWidth(t, out, 60)
}

func TestRenderMarkdownTable(t *testing.T) {
	input := "| Name | Status |\n| :--- | ---: |\n| CI | **green** |\n| Docs | ready |"
	out := renderMarkdown(input, 60)

	for _, want := range []string{"┌", "┬", "┐", "├", "┼", "┤", "└", "┴", "┘", "Name", "Status", "CI", "green", "Docs"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "|") {
		t.Fatalf("raw pipe leaked into:\n%s", out)
	}
	assertWithinWidth(t, out, 60)
}

func TestRenderMarkdownWrapsStyledTextToWidth(t *testing.T) {
	input := "A **long bold sentence** that should wrap cleanly at narrow widths, " +
		"with `inline code` and a [link](https://example.test/very/long/path) mixed in."
	for width := 20; width <= 60; width += 5 {
		out := renderMarkdown(input, width)
		assertWithinWidth(t, out, width)
	}
}

func TestRenderMarkdownStripsControlCharacters(t *testing.T) {
	out := renderMarkdown("hello \x1b[31mred\x1b[0m world\x07", 40)
	if strings.ContainsRune(out, '\x1b') || strings.ContainsRune(out, '\x07') {
		t.Fatalf("control character survived: %q", out)
	}
	if !strings.Contains(out, "red") {
		t.Fatalf("text lost during sanitization: %q", out)
	}
}

func TestRenderMarkdownTruncatesLongCodeLines(t *testing.T) {
	line := strings.Repeat("x", 100)
	out := renderMarkdown("```\n"+line+"\n```", 40)
	for _, got := range strings.Split(out, "\n") {
		if lipgloss.Width(got) > 40 {
			t.Fatalf("code line exceeds width: %q", got)
		}
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("long code line was not truncated:\n%s", out)
	}
}

func TestRenderMarkdownNarrowTableFallback(t *testing.T) {
	input := "| A | B |\n| --- | --- |\n| long value here | another value |"
	out := renderMarkdown(input, 11)
	if out == "" {
		t.Fatal("narrow table rendered nothing")
	}
	assertWithinWidth(t, out, 11)
}

func TestAssistantMarkdownRenderedInView(t *testing.T) {
	m := newModel("test-token")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(*model)

	m.messages = []chatMessage{{
		kind: msgAssistant,
		text: "# Title\n\n**bold** and `code`\n\n```go\nfmt.Println(\"x\")\n```",
	}}
	m.renderChat(false)
	view := m.viewport.View()

	for _, want := range []string{"Title", "bold", "code", "fmt.Println"} {
		if !strings.Contains(view, want) {
			t.Fatalf("viewport missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "**") || strings.Contains(view, "```") {
		t.Fatalf("raw markdown leaked into viewport:\n%s", view)
	}
}

func TestMessageRenderCacheInvalidatesOnChange(t *testing.T) {
	msg := chatMessage{kind: msgAssistant, text: "# One"}
	first := msg.renderedReply(60)
	if !strings.Contains(first, "One") {
		t.Fatalf("initial render = %q", first)
	}
	msg.text += "\n\nTwo **bold**"
	second := msg.renderedReply(60)
	if second == first || !strings.Contains(second, "Two") || !strings.Contains(second, "bold") {
		t.Fatalf("cache did not invalidate: %q", second)
	}
}

func assertWithinWidth(t *testing.T, rendered string, width int) {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("line width %d exceeds %d: %q\nfull:\n%s", got, width, line, rendered)
		}
	}
}
