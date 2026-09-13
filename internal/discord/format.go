package discord

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// discordMaxMessageLen is Discord's per-message limit.
	discordMaxMessageLen = 2000

	// maxTableWidth is the practical width of Discord's monospace block. Desktop
	// scrolls horizontally, but mobile hard-wraps at roughly this many columns, so
	// wider tables are wrapped column-wise here instead of mid-row by the client.
	maxTableWidth = 72

	// minColumnWidth is the narrowest a column may be squeezed to when a table has
	// to be fitted into maxTableWidth.
	minColumnWidth = 6

	// columnGap separates rendered columns.
	columnGap = "  "

	fenceMarker = "```"
	fenceClose  = "\n" + fenceMarker

	// maxLanguageTagLen bounds the language tag carried across a split, so a
	// reopened fence can never crowd out the chunk it prefixes.
	maxLanguageTagLen = 16

	// minFenceAwareLen is the smallest limit that can still hold a reopened fence
	// plus a closing one and make progress. Discord's real limit is far above it;
	// anything smaller falls back to plain slicing.
	minFenceAwareLen = 32

	// zeroWidthSpace breaks up a backtick run without occupying a column. Written
	// as an escape on purpose: as a literal it is invisible in every editor and
	// diff, so the constant reads as empty and the defusing below reads as a no-op.
	zeroWidthSpace = "\u200b"
)

// tableSeparatorPattern matches a markdown table separator row such as
// "|---|---|" or "| :--- | ---: |".
var tableSeparatorPattern = regexp.MustCompile(`^\s*\|?(\s*:?-+:?\s*\|)+\s*:?-*:?\s*\|?\s*$`)

// formatTables rewrites every markdown pipe table in text as fixed-width aligned
// columns, since Discord renders monospace code blocks but not markdown tables.
// A table already inside a fence is aligned in place; one outside gets its own
// fence. All other text is passed through untouched.
func formatTables(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	opener := "" // the fence opener currently in effect, "" when outside one

	for i := 0; i < len(lines); i++ {
		// The table is tried first. A fence marker in a header cell would
		// otherwise be counted as a real fence, leaving the row unparsed and the
		// state inverted for every table after it. Rendered rows never carry a
		// marker of their own — splitCells defuses them — and the fence added
		// around a table balances, so neither can disturb opener.
		rows, end, ok := parseTable(lines, i)
		if !ok {
			opener = fenceStateAfter(lines[i], opener)
			out = append(out, lines[i])
			continue
		}

		if opener != "" {
			out = append(out, renderTable(rows)...)
		} else {
			out = append(out, fenceMarker)
			out = append(out, renderTable(rows)...)
			out = append(out, fenceMarker)
		}
		i = end - 1
	}

	return strings.Join(out, "\n")
}

// parseTable reads a markdown pipe table starting at lines[start]. It requires a
// header row followed by a separator row, and consumes body rows until a line
// that is not part of the table. It returns the cells (header first, separator
// dropped) and the index just past the table.
func parseTable(lines []string, start int) (rows [][]string, end int, ok bool) {
	if start+1 >= len(lines) || !isTableSeparator(lines[start+1]) || !isHeaderRow(lines[start]) {
		return nil, start, false
	}

	// The separator is the anchor, so the header needs no leading delimiter of
	// its own: nothing in prose looks like "---|---", which is what makes it
	// safe to relax the header while keeping the guarantee below.
	bordered := strings.HasPrefix(strings.TrimSpace(lines[start]), "|")
	header := splitCells(lines[start])

	rows = [][]string{header}
	end = start + 2
	for end < len(lines) && isBodyRow(lines[end], bordered, len(header)) {
		rows = append(rows, splitCells(lines[end]))
		end++
	}

	// Pad ragged rows so every row has the same number of columns.
	cols := 0
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	for i, row := range rows {
		for len(row) < cols {
			row = append(row, "")
		}
		rows[i] = row
	}

	return rows, end, true
}

// isHeaderRow reports whether the line can be a table header. Only a pipe is
// required, because parseTable has already matched a separator on the next line
// and no prose looks like "---|---"; the borderless form ("a | b", which the
// model emits at least as often as the bordered one) must be accepted here.
func isHeaderRow(line string) bool {
	return strings.Contains(line, "|")
}

// isBodyRow reports whether the line continues the table whose header had
// headerCells columns.
//
// A bordered table keeps the original rule exactly: the leading delimiter is
// required, so prose that merely contains a pipe — a shell command with a
// pipeline, say — is never swallowed into the table above it.
//
// A borderless table has no delimiter to require, so the column count stands in
// for it: a line continues the table only if it splits into the same number of
// cells as the header. `cat foo | grep bar` below a three-column table is two
// cells and ends the table, which is the same guarantee by a different means.
// It is weaker only for a pipeline that happens to match the column count
// exactly, and a blank line — which has no pipe and so ends the table either
// way — is the normal separator in that case.
func isBodyRow(line string, bordered bool, headerCells int) bool {
	if isTableSeparator(line) {
		return false
	}
	trimmed := strings.TrimSpace(line)
	if bordered {
		return strings.HasPrefix(trimmed, "|")
	}
	if !strings.Contains(trimmed, "|") {
		return false
	}
	return len(splitCells(line)) == headerCells
}

// isTableSeparator reports whether the line is a table's "|---|---|" row.
func isTableSeparator(line string) bool {
	return strings.Contains(line, "|") && tableSeparatorPattern.MatchString(line)
}

// escapedPipeMark stands in for an escaped pipe while a row is split. Without
// it a "\|" inside a cell would split the row there and shift every later value
// one column left — an incident table showing values under the wrong header,
// which reads as fact rather than as a glitch.
const escapedPipeMark = "\x00"

// splitCells splits one table row into trimmed, fence-safe cells, discarding the
// leading and trailing delimiters.
func splitCells(line string) []string {
	// A literal NUL could not be told from the stand-in afterwards, and displays
	// as nothing anyway, so it goes before the swap.
	trimmed := strings.ReplaceAll(strings.TrimSpace(line), escapedPipeMark, "")
	trimmed = strings.ReplaceAll(trimmed, `\|`, escapedPipeMark)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")

	cells := strings.Split(trimmed, "|")
	for i, cell := range cells {
		cell = strings.ReplaceAll(cell, escapedPipeMark, "|")
		cells[i] = neutralizeFences(strings.TrimSpace(cell))
	}

	return cells
}

// neutralizeFences breaks up runs of three or more backticks by separating them
// with a zero-width space. Cells carry log-derived text the operator does not
// control, and a bare ``` inside the block would close it early — everything
// after would render as live markdown, so a pasted mention or link in a log line
// would become a real one.
//
// The separator is deliberately invisible and occupies no column, so alignment
// is unaffected and the cell stays a faithful transcript of the log line; it
// does travel with a copy-paste. The substitution is just as deliberately narrow
// — it fires only on runs of three or more backticks. Do not broaden it to
// single backticks: those are ordinary in log text and in the commands an
// operator copies out of the table, and rewriting them would corrupt both.
func neutralizeFences(cell string) string {
	if !strings.Contains(cell, fenceMarker) {
		return cell
	}

	var b strings.Builder
	b.Grow(len(cell) + len(zeroWidthSpace))
	run := 0
	for _, r := range cell {
		if r != '`' {
			run = 0
		} else {
			if run == 2 {
				b.WriteString(zeroWidthSpace)
				run = 0
			}
			run++
		}
		b.WriteRune(r)
	}

	return b.String()
}

// renderTable lays rows out as left-aligned fixed-width columns: the header, a
// dashed rule, then the body. Cells too wide for their column are wrapped, so a
// row may occupy several lines but never runs past maxTableWidth.
func renderTable(rows [][]string) []string {
	if len(rows) == 0 {
		return nil
	}
	widths := fitColumns(rows)

	out := make([]string, 0, len(rows)+1)
	out = append(out, renderRow(rows[0], widths)...)

	rule := make([]string, len(widths))
	for i, w := range widths {
		rule[i] = strings.Repeat("-", w)
	}
	out = append(out, strings.Join(rule, columnGap))

	for _, row := range rows[1:] {
		out = append(out, renderRow(row, widths)...)
	}

	return out
}

// fitColumns sizes each column to its widest cell, then shrinks the widest
// columns one at a time until the table fits maxTableWidth.
func fitColumns(rows [][]string) []int {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if w := displayWidth(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}
	for i, w := range widths {
		if w < 1 {
			widths[i] = 1
		}
	}

	gaps := len(columnGap) * (len(widths) - 1)
	for sumWidths(widths)+gaps > maxTableWidth {
		widest := 0
		for i, w := range widths {
			if w > widths[widest] {
				widest = i
			}
		}
		if widths[widest] <= minColumnWidth {
			break // every column is already at its floor
		}
		widths[widest]--
	}

	return widths
}

func sumWidths(widths []int) int {
	sum := 0
	for _, w := range widths {
		sum += w
	}
	return sum
}

// renderRow renders one logical row, wrapping cells that exceed their column
// width onto continuation lines.
func renderRow(row []string, widths []int) []string {
	wrapped := make([][]string, len(row))
	height := 1
	for i, cell := range row {
		wrapped[i] = wrapCell(cell, widths[i])
		if len(wrapped[i]) > height {
			height = len(wrapped[i])
		}
	}

	lines := make([]string, 0, height)
	for line := 0; line < height; line++ {
		parts := make([]string, len(row))
		for col := range row {
			text := ""
			if line < len(wrapped[col]) {
				text = wrapped[col][line]
			}
			parts[col] = text + strings.Repeat(" ", widths[col]-displayWidth(text))
		}
		lines = append(lines, strings.TrimRight(strings.Join(parts, columnGap), " "))
	}

	return lines
}

// wrapCell breaks a cell into lines, preferring word boundaries and splitting
// words that are too long to fit alone. No returned line is ever wider than
// width display columns, which is what lets renderRow pad without underflowing.
func wrapCell(cell string, width int) []string {
	if cell == "" {
		return []string{""}
	}

	var lines []string
	cur, curWidth := "", 0
	for _, word := range strings.Fields(cell) {
		wordWidth := displayWidth(word)
		switch {
		case cur == "":
			cur, curWidth = word, wordWidth
		case curWidth+1+wordWidth <= width:
			cur, curWidth = cur+" "+word, curWidth+1+wordWidth
		default:
			lines = append(lines, cur)
			cur, curWidth = word, wordWidth
		}
		for curWidth > width {
			head, tail, headWidth := cutToDisplayWidth(cur, width)
			lines = append(lines, head)
			cur, curWidth = tail, curWidth-headWidth
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) == 0 {
		return []string{""}
	}

	return lines
}

// breakAfter are the characters a long word may be broken after, so identifiers
// like "accretion-core-worker" wrap at a hyphen rather than mid-syllable.
const breakAfter = "-,/_:."

// cutToDisplayWidth splits s so the head fits within width display columns,
// preferring the last punctuation break inside it. It always consumes at least
// one rune so callers terminate, and reports the head's display width.
func cutToDisplayWidth(s string, width int) (head, tail string, headWidth int) {
	end, used := len(s), 0
	for i, r := range s {
		if used+runeWidth(r) > width && i > 0 {
			end = i
			break
		}
		used += runeWidth(r)
	}
	if cut := strings.LastIndexAny(s[:end], breakAfter); cut > 0 {
		end = cut + 1
		used = displayWidth(s[:end])
	}

	return s[:end], s[end:], used
}

// displayWidth is the number of monospace columns s occupies. Byte length would
// misalign any row containing a multi-byte rune such as an en dash.
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// runeWidth returns 0 for combining and formatting marks, 2 for East Asian wide
// and fullwidth runes, and 1 otherwise.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Cf, r):
		return 0
	case isWide(r):
		return 2
	default:
		return 1
	}
}

// wideRanges covers the East Asian Wide and Fullwidth blocks, which occupy two
// monospace columns.
var wideRanges = []struct{ lo, hi rune }{
	{0x1100, 0x115F}, {0x2E80, 0x303E}, {0x3041, 0x33FF}, {0x3400, 0x4DBF},
	{0x4E00, 0x9FFF}, {0xA000, 0xA4CF}, {0xA960, 0xA97F}, {0xAC00, 0xD7A3},
	{0xF900, 0xFAFF}, {0xFE10, 0xFE19}, {0xFE30, 0xFE6F}, {0xFF00, 0xFF60},
	{0xFFE0, 0xFFE6}, {0x1F300, 0x1F64F}, {0x1F900, 0x1F9FF},
	{0x20000, 0x2FFFD}, {0x30000, 0x3FFFD},
}

func isWide(r rune) bool {
	for _, rng := range wideRanges {
		if r >= rng.lo && r <= rng.hi {
			return true
		}
	}
	return false
}

// splitMessage breaks content into chunks of at most maxLen bytes — the limit
// holds on every chunk, including the ones that gain a fence — splitting on rune
// boundaries and preferring a newline. A split that lands inside a code fence
// closes the fence on the chunk it ends and reopens it, with the same language
// tag, on the next, so no chunk arrives as plain text.
func splitMessage(content string, maxLen int) []string {
	if maxLen < minFenceAwareLen {
		return sliceToLimit(content, maxLen)
	}

	var chunks []string
	opener := "" // non-empty while a fence is open across a chunk boundary

	for content != "" {
		prefix := ""
		if opener != "" {
			prefix = opener + "\n"
			content = strings.TrimLeft(content, "\n")
		}

		// Everything left fits, as long as a fence the model left open can be
		// closed within the limit too. If it cannot, fall through and split.
		if whole := prefix + content; len(whole) <= maxLen {
			if fenceStateAfter(whole, "") == "" {
				return append(chunks, whole)
			}
			if len(whole)+len(fenceClose) <= maxLen {
				return append(chunks, whole+fenceClose)
			}
		}

		// Room for the closing fence is reserved whether or not it is needed, so
		// that appending it below can never push the chunk over the limit.
		body, rest := cutToByteLimit(content, maxLen-len(prefix)-len(fenceClose))
		chunk := prefix + body
		// A chunk always carries its own opener, so its state starts closed.
		if opener = fenceStateAfter(chunk, ""); opener != "" {
			chunk += fenceClose
		}

		chunks = append(chunks, chunk)
		content = rest
	}

	return chunks
}

// sliceToLimit cuts content into rune-safe pieces of at most maxLen bytes with
// no fence bookkeeping, for limits too small to hold a fence pair.
func sliceToLimit(content string, maxLen int) []string {
	var chunks []string
	for content != "" {
		head, tail := cutToByteLimit(content, maxLen)
		if head == "" { // a single rune wider than the limit
			_, size := utf8.DecodeRuneInString(content)
			head, tail = content[:size], content[size:]
		}
		chunks = append(chunks, head)
		content = tail
	}

	return chunks
}

// cutToByteLimit takes the longest prefix of content that fits maxBytes without
// breaking a rune, preferring the last newline in its second half so chunks end
// on line boundaries. The returned strings share content's backing array.
func cutToByteLimit(content string, maxBytes int) (head, tail string) {
	if maxBytes <= 0 {
		return "", content
	}
	if len(content) <= maxBytes {
		return content, ""
	}

	end := maxBytes
	for end > 0 && !utf8.RuneStart(content[end]) {
		end--
	}
	// Split at the last newline before the limit for readability. Never at zero:
	// the head has to advance or the caller loops forever.
	if idx := strings.LastIndex(content[:end], "\n"); idx > 0 && idx >= end/2 {
		end = idx
	}

	return content[:end], content[end:]
}

// fenceStateAfter returns the fence opener still unclosed at the end of text,
// given the opener already in effect when text began. Markers are counted
// wherever they appear, not just at the start of a line, because Discord ends a
// block at the next ``` anywhere in the text.
//
// A backslash-escaped marker is honored only outside a block — that is
// Discord's own asymmetry, not a shortcut here. Outside, "\```" renders as
// literal backticks and opens nothing; inside, the backslash is just a
// character and the marker still closes the block. Treating an escaped marker
// as a fence outside a block is the unsafe direction: this code would believe a
// block is open where the client has none, and would then leave a table
// unfenced with its cells rendering as live markdown.
func fenceStateAfter(text, opener string) string {
	for i := 0; ; {
		j := strings.Index(text[i:], fenceMarker)
		if j < 0 {
			return opener
		}
		pos := i + j
		i = pos + len(fenceMarker)

		switch {
		case opener != "":
			opener = ""
		case isEscaped(text, pos):
			continue
		default:
			opener = fenceMarker + languageTag(text[i:])
		}
	}
}

// isEscaped reports whether the byte at pos is preceded by an odd number of
// backslashes, which is what makes it escaped rather than a backslash literal.
func isEscaped(text string, pos int) bool {
	backslashes := 0
	for pos-backslashes > 0 && text[pos-backslashes-1] == '\\' {
		backslashes++
	}

	return backslashes%2 == 1
}

// languageTag reads the language tag directly following a fence opener, or ""
// if there is none or it is too long to be one.
func languageTag(afterMarker string) string {
	end := len(afterMarker)
	for i, r := range afterMarker {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '+' && r != '#' {
			end = i
			break
		}
	}
	if end > maxLanguageTagLen {
		return ""
	}

	return afterMarker[:end]
}
