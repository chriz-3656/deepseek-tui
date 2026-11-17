package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
)

// -----------------------------------------------------------------------------
// markdown styles
// -----------------------------------------------------------------------------

var (
	mdH1Style = lipgloss.NewStyle().Bold(true).Foreground(accentColor)
	mdH2Style = lipgloss.NewStyle().Bold(true).Foreground(accentColor)
	mdH3Style = lipgloss.NewStyle().Bold(true)
	mdH4Style = lipgloss.NewStyle().Bold(true).Foreground(mutedColor)

	mdInlineCodeStyle = lipgloss.NewStyle().
		Foreground(accentColor).
		Background(lipgloss.AdaptiveColor{Light: "#EEF2FF", Dark: "#2A2F45"})
	mdLinkStyle = lipgloss.NewStyle().Foreground(accentColor).Underline(true)

	mdCodeTextStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#1F2937", Dark: "#E6EDF3"})
	mdCodeRuleStyle  = lipgloss.NewStyle().Foreground(mutedColor)
	mdCodeLabelStyle = lipgloss.NewStyle().Foreground(mutedColor).Italic(true)

	mdRuleStyle        = lipgloss.NewStyle().Foreground(mutedColor)
	mdQuoteRuleStyle   = lipgloss.NewStyle().Foreground(mutedColor)
	mdBulletStyle      = lipgloss.NewStyle().Foreground(accentColor)
	mdTaskDoneStyle    = lipgloss.NewStyle().Foreground(readyColor)
	mdTaskTodoStyle    = lipgloss.NewStyle().Foreground(mutedColor)
	mdTableBorderStyle = lipgloss.NewStyle().Foreground(mutedColor)
	mdTableHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(accentColor)
)

// -----------------------------------------------------------------------------
// markdown entry point
// -----------------------------------------------------------------------------

// renderMarkdown renders assistant text as a compact terminal Markdown view.
// It supports headings, emphasis, inline code, links, lists, task lists,
// blockquotes, rules, tables, and fenced code blocks. Images and raw HTML are
// shown as text; syntax highlighting is intentionally out of scope.
func renderMarkdown(text string, width int) string {
	if width < 8 {
		width = 8
	}
	text = sanitizeText(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = expandTabs(lines[i], 4)
	}

	var out []string
	gap := func() {
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
	}

	for i := 0; i < len(lines); {
		raw := lines[i]
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			i++
			continue
		}

		if marker, lang, ok := mdFenceStart(raw); ok {
			gap()
			code, next := mdFenceBody(lines, i, marker)
			out = append(out, mdCodeBlock(lang, code, width)...)
			i = next
			continue
		}
		if mdHorizontalRule(trimmed) {
			gap()
			out = append(out, mdRuleStyle.Render(strings.Repeat("─", min(width, 40))))
			i++
			continue
		}
		if level, heading, ok := mdHeading(raw); ok {
			gap()
			out = append(out, mdHeadingLines(level, heading, width)...)
			i++
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			gap()
			joined, next := mdCollectQuote(lines, i)
			prefix := mdQuoteRuleStyle.Render("│ ")
			out = append(out, mdWrap(parseInline(joined), width, prefix, prefix)...)
			i = next
			continue
		}
		if mdTableStart(lines, i) {
			gap()
			table, next := mdRenderTable(lines, i, width)
			out = append(out, table...)
			i = next
			continue
		}
		if _, _, _, _, ok := mdListMarker(raw); ok {
			gap()
			items, next := mdCollectList(lines, i)
			out = append(out, mdRenderList(items, width)...)
			i = next
			continue
		}

		gap()
		para, next := mdCollectParagraph(lines, i)
		out = append(out, mdWrap(parseInline(para), width, "", "")...)
		i = next
	}

	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// -----------------------------------------------------------------------------
// fenced code blocks
// -----------------------------------------------------------------------------

func mdFenceStart(line string) (rune, string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return 0, "", false
	}
	runes := []rune(trimmed)
	if len(runes) < 3 {
		return 0, "", false
	}
	marker := runes[0]
	if marker != '`' && marker != '~' {
		return 0, "", false
	}
	n := runLength(runes, 0, marker)
	if n < 3 {
		return 0, "", false
	}
	return marker, mdFenceLanguage(string(runes[n:])), true
}

func mdFenceLanguage(info string) string {
	fields := strings.Fields(strings.TrimSpace(info))
	if len(fields) == 0 {
		return ""
	}
	lang := strings.Trim(fields[0], "{}.")
	var b strings.Builder
	for _, r := range lang {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '+', r == '#':
			b.WriteRune(r)
		}
	}
	if b.Len() > 16 {
		return b.String()[:16]
	}
	return b.String()
}

func mdFenceBody(lines []string, start int, marker rune) ([]string, int) {
	var code []string
	for i := start + 1; i < len(lines); i++ {
		if mdFenceClose(lines[i], marker) {
			return code, i + 1
		}
		code = append(code, strings.TrimRight(lines[i], " \t"))
	}
	return code, len(lines)
}

func mdFenceClose(line string, marker rune) bool {
	runes := []rune(strings.TrimSpace(line))
	n := runLength(runes, 0, marker)
	if n < 3 {
		return false
	}
	return strings.TrimSpace(string(runes[n:])) == ""
}

func mdCodeBlock(lang string, code []string, width int) []string {
	var out []string
	if lang != "" {
		out = append(out, " "+mdCodeLabelStyle.Render(lang))
	}
	inner := width - 2
	if inner < 4 {
		inner = 4
	}
	rule := mdCodeRuleStyle.Render("│")
	for _, line := range code {
		line = strings.ReplaceAll(line, "\r", "")
		if line == "" {
			out = append(out, rule)
			continue
		}
		if lipgloss.Width(line) > inner {
			line = truncateText(line, inner)
		}
		out = append(out, rule+" "+mdCodeTextStyle.Render(line))
	}
	if len(code) == 0 {
		out = append(out, rule)
	}
	return out
}

// -----------------------------------------------------------------------------
// headings, rules, quotes
// -----------------------------------------------------------------------------

func mdHeading(line string) (int, string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return 0, "", false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == '#' {
		n++
	}
	if n < 1 || n > 6 {
		return 0, "", false
	}
	if n < len(trimmed) && trimmed[n] != ' ' && trimmed[n] != '\t' {
		return 0, "", false
	}
	text := strings.TrimSpace(trimmed[n:])
	if strings.HasSuffix(text, "#") {
		without := strings.TrimRight(text, "#")
		if without == "" || strings.HasSuffix(without, " ") || strings.HasSuffix(without, "\t") {
			text = strings.TrimSpace(without)
		}
	}
	return n, text, true
}

func mdHeadingLines(level int, text string, width int) []string {
	plain := strings.TrimSpace(mdPlainInline(text))
	if plain == "" {
		return nil
	}
	style := mdH4Style
	switch level {
	case 1:
		style = mdH1Style
	case 2:
		style = mdH2Style
	case 3:
		style = mdH3Style
	}
	wrapped := strings.Split(wrapText(plain, width), "\n")
	out := make([]string, 0, len(wrapped)+1)
	maxw := 0
	for _, line := range wrapped {
		if w := lipgloss.Width(line); w > maxw {
			maxw = w
		}
		out = append(out, style.Render(line))
	}
	if level == 1 {
		out = append(out, mdRuleStyle.Render(strings.Repeat("─", min(width, max(4, maxw)))))
	}
	return out
}

func mdHorizontalRule(trimmed string) bool {
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, trimmed)
	if len(compact) < 3 {
		return false
	}
	marker := compact[0]
	if marker != '-' && marker != '*' && marker != '_' {
		return false
	}
	for i := 1; i < len(compact); i++ {
		if compact[i] != marker {
			return false
		}
	}
	return true
}

func mdCollectQuote(lines []string, start int) (string, int) {
	var parts []string
	i := start
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, ">") {
			break
		}
		q := trimmed
		for strings.HasPrefix(q, ">") {
			q = strings.TrimSpace(strings.TrimPrefix(q, ">"))
		}
		parts = append(parts, q)
		i++
	}
	return strings.Join(parts, " "), i
}

// -----------------------------------------------------------------------------
// lists
// -----------------------------------------------------------------------------

type mdListItem struct {
	indent  int
	ordered bool
	number  int
	text    string
}

func mdListMarker(line string) (int, bool, int, string, bool) {
	indent := 0
	for indent < len(line) && line[indent] == ' ' {
		indent++
	}
	if indent > 8 {
		return 0, false, 0, "", false
	}
	rest := line[indent:]
	if len(rest) == 0 {
		return 0, false, 0, "", false
	}

	if rest[0] == '-' || rest[0] == '*' || rest[0] == '+' {
		if len(rest) == 1 {
			return indent, false, 0, "", true
		}
		if rest[1] == ' ' || rest[1] == '\t' {
			return indent, false, 0, strings.TrimSpace(rest[2:]), true
		}
		return 0, false, 0, "", false
	}

	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 || j > 3 || j >= len(rest) || (rest[j] != '.' && rest[j] != ')') {
		return 0, false, 0, "", false
	}
	if j+1 < len(rest) && rest[j+1] != ' ' && rest[j+1] != '\t' {
		return 0, false, 0, "", false
	}
	n, _ := strconv.Atoi(rest[:j])
	text := ""
	if j+2 <= len(rest) {
		text = strings.TrimSpace(rest[j+2:])
	}
	return indent, true, n, text, true
}

func mdCollectList(lines []string, start int) ([]mdListItem, int) {
	var items []mdListItem
	i := start
	for i < len(lines) {
		if strings.TrimSpace(lines[i]) == "" {
			break
		}
		indent, ordered, number, text, ok := mdListMarker(lines[i])
		if !ok {
			break
		}
		item := mdListItem{indent: indent, ordered: ordered, number: number, text: text}
		i++
		for i < len(lines) {
			if strings.TrimSpace(lines[i]) == "" {
				break
			}
			if _, _, _, _, isItem := mdListMarker(lines[i]); isItem {
				break
			}
			leading := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
			if leading < indent+2 {
				break
			}
			if mdStartsBlock(lines, i) {
				break
			}
			item.text += " " + strings.TrimSpace(lines[i])
			i++
		}
		items = append(items, item)
	}
	return items, i
}

func mdRenderList(items []mdListItem, width int) []string {
	var out []string
	counters := map[int]int{}

	for _, item := range items {
		level := item.indent / 2
		if level < 0 {
			level = 0
		}
		if level > 3 {
			level = 3
		}
		lead := strings.Repeat("  ", level)

		task := ""
		text := item.text
		switch {
		case strings.HasPrefix(text, "[ ] "):
			task = "todo"
			text = text[4:]
		case strings.HasPrefix(text, "[x] "), strings.HasPrefix(text, "[X] "):
			task = "done"
			text = text[4:]
		}

		marker := "•"
		markerStyle := mdBulletStyle
		switch {
		case item.ordered:
			n := counters[item.indent]
			if n == 0 {
				n = item.number
				if n <= 0 {
					n = 1
				}
			}
			counters[item.indent] = n + 1
			marker = fmt.Sprintf("%d.", n)
		case task == "todo":
			marker = "○"
			markerStyle = mdTaskTodoStyle
		case task == "done":
			marker = "✓"
			markerStyle = mdTaskDoneStyle
		default:
			if level%2 == 1 {
				marker = "◦"
			}
		}
		if !item.ordered {
			counters[item.indent] = 0
		}

		prefix := lead + markerStyle.Render(marker) + " "
		cont := strings.Repeat(" ", lipgloss.Width(prefix))
		spans := parseInline(text)
		if task == "done" {
			for j := range spans {
				spans[j].strike = true
				spans[j].dim = true
			}
		}
		lines := mdWrap(spans, width, prefix, cont)
		if len(lines) == 0 {
			lines = []string{prefix}
		}
		out = append(out, lines...)
	}
	return out
}

// -----------------------------------------------------------------------------
// tables
// -----------------------------------------------------------------------------

type mdAlign int

const (
	mdAlignLeft mdAlign = iota
	mdAlignCenter
	mdAlignRight
)

func mdTableStart(lines []string, i int) bool {
	if i+1 >= len(lines) {
		return false
	}
	if !strings.Contains(lines[i], "|") || !strings.Contains(lines[i+1], "|") {
		return false
	}
	if !mdTableSeparator(lines[i+1]) {
		return false
	}
	return len(mdSplitRow(lines[i])) == len(mdSplitRow(lines[i+1]))
}

func mdTableSeparator(line string) bool {
	cells := mdSplitRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		cell = strings.TrimPrefix(cell, ":")
		cell = strings.TrimSuffix(cell, ":")
		if cell == "" {
			return false
		}
		for _, r := range cell {
			if r != '-' {
				return false
			}
		}
	}
	return true
}

func mdSplitRow(line string) []string {
	s := strings.TrimSpace(line)
	if strings.HasPrefix(s, "|") {
		s = s[1:]
	}
	if strings.HasSuffix(s, "|") {
		s = s[:len(s)-1]
	}
	var cells []string
	var cur strings.Builder
	escaped := false
	inCode := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			cur.WriteRune(r)
			escaped = true
		case r == '`':
			inCode = !inCode
			cur.WriteRune(r)
		case r == '|' && !inCode:
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	cells = append(cells, strings.TrimSpace(cur.String()))
	return cells
}

func mdTableAligns(separator string) []mdAlign {
	cells := mdSplitRow(separator)
	aligns := make([]mdAlign, len(cells))
	for i, cell := range cells {
		cell = strings.TrimSpace(cell)
		left := strings.HasPrefix(cell, ":")
		right := strings.HasSuffix(cell, ":")
		switch {
		case left && right:
			aligns[i] = mdAlignCenter
		case right:
			aligns[i] = mdAlignRight
		default:
			aligns[i] = mdAlignLeft
		}
	}
	return aligns
}

func mdRenderTable(lines []string, start, width int) ([]string, int) {
	header := mdSplitRow(lines[start])
	aligns := mdTableAligns(lines[start+1])
	cols := len(header)
	if cols == 0 {
		return nil, start + 1
	}
	for len(aligns) < cols {
		aligns = append(aligns, mdAlignLeft)
	}

	var rows [][]string
	i := start + 2
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || !strings.Contains(lines[i], "|") {
			break
		}
		if _, _, ok := mdFenceStart(lines[i]); ok {
			break
		}
		if _, _, ok := mdHeading(lines[i]); ok {
			break
		}
		if mdHorizontalRule(trimmed) || strings.HasPrefix(trimmed, ">") {
			break
		}
		if _, _, _, _, ok := mdListMarker(lines[i]); ok {
			break
		}
		row := mdSplitRow(lines[i])
		if len(row) > cols {
			row = row[:cols]
		}
		for len(row) < cols {
			row = append(row, "")
		}
		rows = append(rows, row)
		i++
	}

	natural := make([]int, cols)
	for c := 0; c < cols; c++ {
		natural[c] = spansWidth(parseInline(header[c]))
	}
	for _, row := range rows {
		for c := 0; c < cols; c++ {
			if w := spansWidth(parseInline(row[c])); w > natural[c] {
				natural[c] = w
			}
		}
	}
	const maxCol = 36
	for c := range natural {
		natural[c] = max(3, min(maxCol, natural[c]))
	}

	available := width - (3*cols + 1)
	if available < cols*3 {
		return mdTableFallback(header, rows, width), i
	}
	widths := append([]int(nil), natural...)
	total := 0
	for _, w := range widths {
		total += w
	}
	for total > available {
		idx := -1
		best := 3
		for c, w := range widths {
			if w > best {
				best = w
				idx = c
			}
		}
		if idx < 0 {
			return mdTableFallback(header, rows, width), i
		}
		widths[idx]--
		total--
	}

	out := []string{
		mdTableBorder("┌", "┬", "┐", widths),
		mdTableRow(header, widths, aligns, true),
		mdTableBorder("├", "┼", "┤", widths),
	}
	for _, row := range rows {
		out = append(out, mdTableRow(row, widths, aligns, false))
	}
	out = append(out, mdTableBorder("└", "┴", "┘", widths))
	return out, i
}

func mdTableBorder(left, mid, right string, widths []int) string {
	var b strings.Builder
	b.WriteString(mdTableBorderStyle.Render(left))
	for i, w := range widths {
		if i > 0 {
			b.WriteString(mdTableBorderStyle.Render(mid))
		}
		b.WriteString(mdTableBorderStyle.Render(strings.Repeat("─", w+2)))
	}
	b.WriteString(mdTableBorderStyle.Render(right))
	return b.String()
}

func mdTableRow(cells []string, widths []int, aligns []mdAlign, header bool) string {
	var b strings.Builder
	b.WriteString(mdTableBorderStyle.Render("│"))
	for i, w := range widths {
		if i > 0 {
			b.WriteString(mdTableBorderStyle.Render("│"))
		}
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		align := mdAlignLeft
		if i < len(aligns) {
			align = aligns[i]
		}
		b.WriteString(" ")
		if header {
			plain := truncateText(mdPlainInline(cell), w)
			b.WriteString(mdTableHeaderStyle.Render(padCell(plain, lipgloss.Width(plain), w, align)))
		} else {
			spans := truncateSpans(parseInline(cell), w)
			b.WriteString(padCell(renderSpans(spans), spansWidth(spans), w, align))
		}
		b.WriteString(" ")
	}
	b.WriteString(mdTableBorderStyle.Render("│"))
	return b.String()
}

func padCell(content string, used, width int, align mdAlign) string {
	gap := width - used
	if gap < 0 {
		gap = 0
	}
	switch align {
	case mdAlignRight:
		return strings.Repeat(" ", gap) + content
	case mdAlignCenter:
		left := gap / 2
		return strings.Repeat(" ", left) + content + strings.Repeat(" ", gap-left)
	default:
		return content + strings.Repeat(" ", gap)
	}
}

func mdTableFallback(header []string, rows [][]string, width int) []string {
	var out []string
	var headerSpans []mdSpan
	for i, cell := range header {
		if i > 0 {
			headerSpans = append(headerSpans, mdSpan{text: " │ ", dim: true})
		}
		spans := parseInline(cell)
		for j := range spans {
			spans[j].bold = true
			spans[j].dim = false
		}
		headerSpans = append(headerSpans, spans...)
	}
	out = append(out, mdWrap(headerSpans, width, "", "")...)
	out = append(out, mdRuleStyle.Render(strings.Repeat("─", min(width, 40))))
	for _, row := range rows {
		var spans []mdSpan
		for i, cell := range row {
			if i > 0 {
				spans = append(spans, mdSpan{text: " │ ", dim: true})
			}
			spans = append(spans, parseInline(cell)...)
		}
		out = append(out, mdWrap(spans, width, "", "")...)
	}
	return out
}

// -----------------------------------------------------------------------------
// paragraphs
// -----------------------------------------------------------------------------

func mdCollectParagraph(lines []string, start int) (string, int) {
	var b strings.Builder
	i := start
	for i < len(lines) {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			break
		}
		if i > start && mdStartsBlock(lines, i) {
			break
		}
		hard := strings.HasSuffix(line, "  ") || strings.HasSuffix(strings.TrimRight(line, " "), "\\")
		text := strings.TrimSpace(line)
		if hard {
			text = strings.TrimSuffix(text, "\\")
		}
		if b.Len() > 0 {
			prev := lines[i-1]
			prevHard := strings.HasSuffix(prev, "  ") || strings.HasSuffix(strings.TrimRight(prev, " "), "\\")
			if prevHard {
				b.WriteString("\n")
			} else {
				b.WriteString(" ")
			}
		}
		b.WriteString(text)
		i++
	}
	if i == start {
		i++
	}
	return b.String(), i
}

func mdStartsBlock(lines []string, i int) bool {
	if i >= len(lines) {
		return false
	}
	raw := lines[i]
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return true
	}
	if _, _, ok := mdFenceStart(raw); ok {
		return true
	}
	if mdHorizontalRule(trimmed) {
		return true
	}
	if _, _, ok := mdHeading(raw); ok {
		return true
	}
	if strings.HasPrefix(trimmed, ">") {
		return true
	}
	if _, _, _, _, ok := mdListMarker(raw); ok {
		return true
	}
	return mdTableStart(lines, i)
}

// -----------------------------------------------------------------------------
// inline markdown
// -----------------------------------------------------------------------------

type mdSpan struct {
	text   string
	bold   bool
	italic bool
	strike bool
	code   bool
	link   bool
	dim    bool
}

func (s mdSpan) render() string {
	switch {
	case s.code:
		return mdInlineCodeStyle.Render(s.text)
	case s.link:
		return mdLinkStyle.Render(s.text)
	}
	style := lipgloss.NewStyle()
	if s.bold {
		style = style.Bold(true)
	}
	if s.italic {
		style = style.Italic(true)
	}
	if s.strike {
		style = style.Strikethrough(true)
	}
	if s.dim {
		style = style.Foreground(mutedColor)
	}
	return style.Render(s.text)
}

func parseInline(text string) []mdSpan {
	return parseInlineDepth(text, 0)
}

func parseInlineDepth(text string, depth int) []mdSpan {
	var spans []mdSpan
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		spans = append(spans, mdSpan{text: cur.String()})
		cur.Reset()
	}

	runes := []rune(text)
	for i := 0; i < len(runes); {
		r := runes[i]

		if r == '\\' && i+1 < len(runes) && isMarkdownPunct(runes[i+1]) {
			cur.WriteRune(runes[i+1])
			i += 2
			continue
		}

		if r == '`' {
			n := runLength(runes, i, '`')
			if end := findMarkerRun(runes, i+n, '`', n); end >= 0 {
				flush()
				content := string(runes[i+n : end])
				if len(content) >= 2 && strings.HasPrefix(content, " ") &&
					strings.HasSuffix(content, " ") && strings.TrimSpace(content) != "" {
					content = content[1 : len(content)-1]
				}
				spans = append(spans, mdSpan{text: content, code: true})
				i = end + n
				continue
			}
			cur.WriteString(strings.Repeat("`", n))
			i += n
			continue
		}

		if r == '*' || r == '_' {
			n := runLength(runes, i, r)
			intraword := r == '_' && isIntraword(runes, i, n)
			if !intraword && n >= 3 {
				if end := findMarkerRun(runes, i+3, r, 3); end >= 0 {
					flush()
					spans = append(spans, mdSpan{text: string(runes[i+3 : end]), bold: true, italic: true})
					i = end + 3
					continue
				}
			}
			if !intraword && n >= 2 {
				if end := findMarkerRun(runes, i+2, r, 2); end >= 0 {
					flush()
					spans = append(spans, mdSpan{text: string(runes[i+2 : end]), bold: true})
					i = end + 2
					continue
				}
			}
			if !intraword && n == 1 {
				if end := findSingleMarker(runes, i+1, r); end >= 0 {
					flush()
					spans = append(spans, mdSpan{text: string(runes[i+1 : end]), italic: true})
					i = end + 1
					continue
				}
			}
			cur.WriteString(strings.Repeat(string(r), n))
			i += n
			continue
		}

		if r == '~' && i+1 < len(runes) && runes[i+1] == '~' {
			if end := findMarkerRun(runes, i+2, '~', 2); end >= 0 {
				flush()
				spans = append(spans, mdSpan{text: string(runes[i+2 : end]), strike: true})
				i = end + 2
				continue
			}
		}

		if r == '[' && depth < 4 {
			if close := findRuneFrom(runes, i+1, ']'); close > i &&
				close+1 < len(runes) && runes[close+1] == '(' {
				if end := findRuneFrom(runes, close+2, ')'); end > close {
					label := string(runes[i+1 : close])
					target := strings.TrimSpace(string(runes[close+2 : end]))
					if sp := strings.IndexAny(target, " \t"); sp > 0 {
						target = target[:sp]
					}
					flush()
					linkSpans := parseInlineDepth(label, depth+1)
					if len(linkSpans) == 0 {
						linkSpans = []mdSpan{{text: label}}
					}
					for _, span := range linkSpans {
						span.link = true
						span.dim = false
						spans = append(spans, span)
					}
					if target != "" && target != label {
						spans = append(spans, mdSpan{text: " (" + target + ")", dim: true})
					}
					i = end + 1
					continue
				}
			}
		}

		if r == '<' {
			if end := findRuneFrom(runes, i+1, '>'); end > i {
				target := string(runes[i+1 : end])
				if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
					strings.HasPrefix(target, "mailto:") {
					flush()
					spans = append(spans, mdSpan{text: target, link: true})
					i = end + 1
					continue
				}
			}
		}

		cur.WriteRune(r)
		i++
	}
	flush()
	return spans
}

func mdPlainInline(text string) string {
	var b strings.Builder
	for _, span := range parseInline(text) {
		b.WriteString(span.text)
	}
	return b.String()
}

func renderSpans(spans []mdSpan) string {
	var b strings.Builder
	for _, span := range spans {
		b.WriteString(span.render())
	}
	return b.String()
}

func spansWidth(spans []mdSpan) int {
	width := 0
	for _, span := range spans {
		width += lipgloss.Width(span.text)
	}
	return width
}

func truncateSpans(spans []mdSpan, width int) []mdSpan {
	if width <= 0 {
		return nil
	}
	if spansWidth(spans) <= width {
		return spans
	}
	limit := width - 1
	if limit < 0 {
		limit = 0
	}
	var out []mdSpan
	used := 0
	truncated := false
	for _, span := range spans {
		var b strings.Builder
		for _, r := range span.text {
			rw := lipgloss.Width(string(r))
			if used+rw > limit {
				truncated = true
				break
			}
			b.WriteRune(r)
			used += rw
		}
		if b.Len() > 0 {
			next := span
			next.text = b.String()
			out = append(out, next)
		}
		if truncated {
			break
		}
	}
	if truncated {
		if len(out) > 0 {
			out[len(out)-1].text += "…"
		} else {
			out = append(out, mdSpan{text: "…"})
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// inline scanning helpers
// -----------------------------------------------------------------------------

func runLength(runes []rune, start int, marker rune) int {
	n := 0
	for start+n < len(runes) && runes[start+n] == marker {
		n++
	}
	return n
}

func findMarkerRun(runes []rune, start int, marker rune, n int) int {
	for i := start; i+n <= len(runes); i++ {
		if runes[i] != marker {
			continue
		}
		count := runLength(runes, i, marker)
		if count >= n {
			return i
		}
		i += count - 1
	}
	return -1
}

func findSingleMarker(runes []rune, start int, marker rune) int {
	for i := start; i < len(runes); i++ {
		if runes[i] != marker {
			continue
		}
		count := runLength(runes, i, marker)
		if count == 1 {
			return i
		}
		i += count - 1
	}
	return -1
}

func findRuneFrom(runes []rune, start int, target rune) int {
	for i := start; i < len(runes); i++ {
		if runes[i] == target {
			return i
		}
	}
	return -1
}

func isIntraword(runes []rune, start, n int) bool {
	if start == 0 || start+n >= len(runes) {
		return false
	}
	return isAlphaNum(runes[start-1]) && isAlphaNum(runes[start+n])
}

func isAlphaNum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isMarkdownPunct(r rune) bool {
	return strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", r)
}

// -----------------------------------------------------------------------------
// styled wrapping
// -----------------------------------------------------------------------------

func mdWrap(spans []mdSpan, width int, firstPrefix, nextPrefix string) []string {
	if width < 1 {
		width = 1
	}

	type token struct {
		text  string
		style mdSpan
		kind  int // 0 word, 1 space, 2 newline
	}

	var tokens []token
	for _, span := range spans {
		var word strings.Builder
		flushWord := func() {
			if word.Len() > 0 {
				tokens = append(tokens, token{text: word.String(), style: span})
				word.Reset()
			}
		}
		for _, r := range span.text {
			switch {
			case r == '\n':
				flushWord()
				tokens = append(tokens, token{kind: 2})
			case unicode.IsSpace(r):
				flushWord()
				if len(tokens) > 0 && tokens[len(tokens)-1].kind == 0 {
					tokens = append(tokens, token{kind: 1})
				}
			default:
				word.WriteRune(r)
			}
		}
		flushWord()
	}

	firstWidth := lipgloss.Width(firstPrefix)
	nextWidth := lipgloss.Width(nextPrefix)
	availFirst := width - firstWidth
	availNext := width - nextWidth
	if availFirst < 1 {
		availFirst = 1
	}
	if availNext < 1 {
		availNext = 1
	}

	var lines []string
	var cur strings.Builder
	curWidth := 0
	first := true
	avail := availFirst
	pendingSpace := false

	flush := func() {
		line := strings.TrimRight(cur.String(), " ")
		prefix := nextPrefix
		if first {
			prefix = firstPrefix
			first = false
		}
		lines = append(lines, prefix+line)
		cur.Reset()
		curWidth = 0
		avail = availNext
		pendingSpace = false
	}

	for _, tok := range tokens {
		switch tok.kind {
		case 2:
			if curWidth > 0 {
				flush()
			} else if len(lines) > 0 {
				lines = append(lines, "")
			}
			pendingSpace = false
		case 1:
			if curWidth > 0 {
				pendingSpace = true
			}
		case 0:
			word := tok.text
			wordWidth := lipgloss.Width(word)
			space := 0
			if pendingSpace && curWidth > 0 {
				space = 1
			}
			if curWidth > 0 && curWidth+space+wordWidth > avail {
				flush()
				space = 0
			}
			if curWidth == 0 && wordWidth > avail {
				rest := []rune(word)
				for len(rest) > 0 {
					take, tail := mdFitRunes(rest, avail)
					if len(take) == 0 {
						break
					}
					styled := tok.style
					styled.text = string(take)
					cur.WriteString(styled.render())
					curWidth += lipgloss.Width(string(take))
					rest = tail
					if len(rest) > 0 {
						flush()
					}
				}
				pendingSpace = false
				continue
			}
			if space > 0 {
				cur.WriteByte(' ')
				curWidth++
			}
			styled := tok.style
			styled.text = word
			cur.WriteString(styled.render())
			curWidth += wordWidth
			pendingSpace = false
		}
	}
	if curWidth > 0 {
		flush()
	}
	return lines
}

func mdFitRunes(runes []rune, avail int) ([]rune, []rune) {
	if len(runes) == 0 {
		return nil, nil
	}
	if avail < 1 {
		avail = 1
	}
	width := 0
	n := 0
	for n < len(runes) {
		rw := lipgloss.Width(string(runes[n]))
		if width+rw > avail && n > 0 {
			break
		}
		width += rw
		n++
		if width >= avail {
			break
		}
	}
	if n == 0 {
		n = 1
	}
	return runes[:n], runes[n:]
}

// -----------------------------------------------------------------------------
// text hygiene
// -----------------------------------------------------------------------------

// sanitizeText strips terminal control characters while keeping newlines and
// tabs. It prevents model output from injecting ANSI escapes into the UI.
func sanitizeText(text string) string {
	if text == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func expandTabs(line string, size int) string {
	if size < 1 {
		size = 4
	}
	if !strings.ContainsRune(line, '\t') {
		return line
	}
	var b strings.Builder
	col := 0
	for _, r := range line {
		if r == '\t' {
			n := size - col%size
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col++
	}
	return b.String()
}

// -----------------------------------------------------------------------------
// render cache
// -----------------------------------------------------------------------------

func (c *chatMessage) renderedReply(width int) string {
	if c.text == "" {
		return ""
	}
	if c.replyCached && c.renderedWidth == width && c.renderedLen == len(c.text) {
		return c.renderedReplyText
	}
	c.renderedReplyText = renderMarkdown(c.text, width)
	c.renderedWidth = width
	c.renderedLen = len(c.text)
	c.replyCached = true
	return c.renderedReplyText
}

func (c *chatMessage) renderedThinking(width int) string {
	if c.thinking == "" {
		return ""
	}
	if c.thinkingCached && c.thinkingWidth == width && c.thinkingLen == len(c.thinking) {
		return c.renderedThinkingText
	}
	c.renderedThinkingText = wrapText(c.thinking, width)
	c.thinkingWidth = width
	c.thinkingLen = len(c.thinking)
	c.thinkingCached = true
	return c.renderedThinkingText
}
