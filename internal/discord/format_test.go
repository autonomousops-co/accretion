package discord

import (
	"strconv"
	"strings"
	"testing"
)

func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"pleg", 4},
		{"2026-08-20T10:58:02.0Z – 2026-08-23T01:08:56.0Z", 47}, // en dash counts as one column
		{"节点", 4}, // East Asian wide runes count as two
		{"é", 1}, // combining acute adds nothing
	}
	for _, tc := range cases {
		if got := displayWidth(tc.in); got != tc.want {
			t.Errorf("displayWidth(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if want := len("2026-08-20T10:58:02.0Z – 2026-08-23T01:08:56.0Z"); want == 47 {
		t.Fatal("test case no longer exercises the byte-vs-width difference")
	}
}

func TestFormatTablesAlignsColumnsInsideAFence(t *testing.T) {
	in := strings.Join([]string{
		"Here is what I found:",
		"| Issue | Count |",
		"|---|---|",
		"| PLEG not healthy | 2 |",
		"| Node NotReady | 11 |",
		"Ask me for detail.",
	}, "\n")

	got := formatTables(in)
	want := strings.Join([]string{
		"Here is what I found:",
		"```",
		"Issue             Count",
		"----------------  -----",
		"PLEG not healthy  2",
		"Node NotReady     11",
		"```",
		"Ask me for detail.",
	}, "\n")

	if got != want {
		t.Errorf("formatTables mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "|---|") {
		t.Error("separator row leaked into the rendered output")
	}
}

func TestFormatTablesAlignsMultiByteCellsByDisplayWidth(t *testing.T) {
	in := strings.Join([]string{
		"| Window | Node |",
		"|---|---|",
		"| 10:58 – 11:02 | worker |", // en dash: 3 bytes, 1 column
		"| 12:12 | worker |",
		"| 节点 | worker2 |", // wide runes: 2 columns each
	}, "\n")

	got := formatTables(in)
	want := strings.Join([]string{
		"```",
		"Window         Node",
		"-------------  -------",
		"10:58 – 11:02  worker",
		"12:12          worker",
		"节点           worker2",
		"```",
	}, "\n")

	if got != want {
		t.Errorf("multi-byte cells misaligned:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTablesLeavesTablelessTextAlone(t *testing.T) {
	in := "### Summary\n- one\n- two\n\nA pipe | in prose is not a table.\n"
	if got := formatTables(in); got != in {
		t.Errorf("formatTables rewrote non-table text:\ngot:\n%s\nwant:\n%s", got, in)
	}
}

func TestFormatTablesAlignsInPlaceInsideAnExistingFence(t *testing.T) {
	in := strings.Join([]string{
		"```",
		"| A | B |",
		"|---|---|",
		"| 1 | 22 |",
		"```",
	}, "\n")

	got := formatTables(in)
	if strings.Count(got, fenceMarker) != 2 {
		t.Errorf("expected no nested fence, got:\n%s", got)
	}
	if !strings.Contains(got, "A  B\n-  --\n1  22") {
		t.Errorf("table inside a fence was not aligned:\n%s", got)
	}
}

func TestFormatTablesWrapsTablesWiderThanTheViewport(t *testing.T) {
	wide := "2026-08-20T10:58:02.0Z – 2026-08-23T01:08:56.0Z recurring on every worker node"
	in := strings.Join([]string{
		"| Issue | Namespace/Resource | Frequency | Time Range (UTC) |",
		"|---|---|---|---|",
		"| Container runtime down / node NotReady | accretion-core-worker, accretion-core-worker2 | Recurring (5+ instances) | " + wide + " |",
	}, "\n")

	got := formatTables(in)
	for _, line := range strings.Split(got, "\n") {
		if w := displayWidth(line); w > maxTableWidth {
			t.Errorf("line is %d columns wide, want <= %d: %q", w, maxTableWidth, line)
		}
	}
	if !strings.Contains(got, "Frequency") {
		t.Errorf("header did not survive fitting:\n%s", got)
	}
}

func TestWrapCellKeepsEveryCharacter(t *testing.T) {
	cells := []string{
		"accretion-core-worker, accretion-core-worker2",
		"2026-08-20T10:58:02.0Z – 2026-08-23T01:08:56.0Z",
		"Recurring (5+ instances)",
		"节点未就绪",
	}
	for _, cell := range cells {
		for _, width := range []int{6, 9, 16, 80} {
			lines := wrapCell(cell, width)
			for _, line := range lines {
				if w := displayWidth(line); w > width {
					t.Errorf("wrapCell(%q, %d) produced a %d-column line %q", cell, width, w, line)
				}
			}
			// Wrapping only ever replaces whitespace with a line break, so the
			// lines concatenated must match the cell with its spaces removed.
			got := strings.ReplaceAll(strings.Join(lines, ""), " ", "")
			want := strings.ReplaceAll(cell, " ", "")
			if got != want {
				t.Errorf("wrapCell(%q, %d) = %q, lost content", cell, width, lines)
			}
		}
	}
}

func TestSplitMessageShortContentIsOneChunk(t *testing.T) {
	got := splitMessage("hello", discordMaxMessageLen)
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("splitMessage = %q, want one unchanged chunk", got)
	}
}

func TestSplitMessagePreservesRunesAndLimit(t *testing.T) {
	content := strings.Repeat("héllo wörld – ok\n", 400)
	chunks := splitMessage(content, 200)

	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 200 {
			t.Errorf("chunk %d is %d bytes, want <= 200", i, len(c))
		}
		if strings.ContainsRune(c, '�') {
			t.Errorf("chunk %d split a multi-byte rune: %q", i, c)
		}
	}
	if joined := strings.Join(chunks, ""); joined != content {
		t.Error("rejoined chunks do not reproduce the input")
	}
}

func TestSplitMessageClosesAndReopensAFenceSplitMidTable(t *testing.T) {
	var b strings.Builder
	b.WriteString("Findings:\n```\n")
	for i := 0; i < 40; i++ {
		b.WriteString("worker-node-0" + string(rune('0'+i%10)) + "  PLEG not healthy – recurring\n")
	}
	b.WriteString("```\nDone.")

	chunks := splitMessage(b.String(), 300)
	if len(chunks) < 3 {
		t.Fatalf("expected the table to split across several chunks, got %d", len(chunks))
	}

	for i, c := range chunks {
		if len(c) > 300 {
			t.Errorf("chunk %d is %d bytes, want <= 300", i, len(c))
		}
		if n := strings.Count(c, fenceMarker); n%2 != 0 {
			t.Errorf("chunk %d has %d fence markers, want a balanced count:\n%s", i, n, c)
		}
	}

	// Every chunk carrying table rows must render as monospace, i.e. open with a
	// fence before its first row.
	for i, c := range chunks[1:] {
		if !strings.HasPrefix(c, fenceMarker) {
			t.Errorf("continuation chunk %d does not reopen the fence:\n%s", i+1, c)
		}
	}
	if strings.HasSuffix(chunks[len(chunks)-1], fenceMarker) {
		t.Error("final chunk should end with the trailing text, not a reopened fence")
	}
}

func TestSplitMessageReopensWithTheLanguageTag(t *testing.T) {
	content := "```json\n" + strings.Repeat("{\"pod\":\"accretion-core-worker\"}\n", 20) + "```"
	chunks := splitMessage(content, 200)

	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks[1:] {
		if !strings.HasPrefix(c, "```json\n") {
			t.Errorf("chunk %d lost the language tag:\n%s", i+1, c)
		}
	}
}

func TestSplitMessageClosesAFenceTheModelLeftOpen(t *testing.T) {
	chunks := splitMessage("```\nunterminated", discordMaxMessageLen)
	if len(chunks) != 1 {
		t.Fatalf("expected one chunk, got %d", len(chunks))
	}
	if !strings.HasSuffix(chunks[0], "\n```") {
		t.Errorf("unclosed fence was not closed: %q", chunks[0])
	}
}

func TestFormatTablesNeutralizesFencesInCellContent(t *testing.T) {
	// A log line surfaced into a cell would otherwise close the block we opened,
	// leaving the rest of the answer to render as live markdown.
	in := strings.Join([]string{
		"| Node | Log |",
		"|---|---|",
		"| worker-01 | ``` @everyone urgent |",
	}, "\n")

	got := formatTables(in)
	if n := strings.Count(got, fenceMarker); n != 2 {
		t.Errorf("output carries %d fence markers, want exactly the 2 we opened and closed:\n%s", n, got)
	}
	if !strings.HasPrefix(got, fenceMarker+"\n") || !strings.HasSuffix(got, "\n"+fenceMarker) {
		t.Errorf("cell content escaped the fence:\n%s", got)
	}
	if !strings.Contains(got, "@everyone") {
		t.Error("neutralizing must defuse the fence, not drop the text")
	}
	// Neutralizing inserts zero-width runes, so alignment must be unaffected.
	for _, line := range strings.Split(got, "\n") {
		if w := displayWidth(line); w > maxTableWidth {
			t.Errorf("line is %d columns wide, want <= %d: %q", w, maxTableWidth, line)
		}
	}
}

func TestNeutralizeFencesBreaksEveryRun(t *testing.T) {
	cases := []string{"```", "````````", "a ``` b ``` c", "`` `` ``"}
	for _, in := range cases {
		got := neutralizeFences(in)
		if strings.Contains(got, fenceMarker) {
			t.Errorf("neutralizeFences(%q) = %q, still contains a fence marker", in, got)
		}
		if displayWidth(got) != displayWidth(in) {
			t.Errorf("neutralizeFences(%q) changed the display width", in)
		}
		if strings.ReplaceAll(got, zeroWidthSpace, "") != in {
			t.Errorf("neutralizeFences(%q) = %q, altered more than the run", in, got)
		}
	}
	if got := neutralizeFences("`inline` code"); got != "`inline` code" {
		t.Errorf("inline code was rewritten: %q", got)
	}
}

func TestFormatTablesLeavesProseWithAPipeAlone(t *testing.T) {
	// Absorbing this line into the table above would hide "--namespace=staging"
	// in a wrapped column, and an operator copying the visible part of the
	// command would run it against the wrong namespace.
	prose := "Run `kubectl delete pod worker-01 | tee log` --namespace=staging to clear it."
	in := strings.Join([]string{
		"| A | B |",
		"|---|---|",
		"| 1 | 2 |",
		prose,
	}, "\n")

	got := formatTables(in)
	if !strings.HasSuffix(got, "\n"+fenceMarker+"\n"+prose) {
		t.Errorf("prose was absorbed into the table:\n%s", got)
	}
}

func TestFenceStateAfterCountsMidLineMarkers(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"no fences here", ""},
		{"```\nrows", fenceMarker},
		{"```json\n{}\n```", ""},
		{"```\nerr: stray ``` inside\nmore", ""},         // the stray marker closes the block
		{"```\nerr: stray ``` inside\n```", fenceMarker}, // ...so the last one reopens it
		{"```verylonglanguagetagthatisnotone\n", fenceMarker},
	}
	for _, tc := range cases {
		if got := fenceStateAfter(tc.in, ""); got != tc.want {
			t.Errorf("fenceStateAfter(%q, \"\") = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFenceStateAfterHonoursEscapesOnlyOutsideABlock(t *testing.T) {
	// Outside a block Discord renders \``` as literal backticks and opens
	// nothing. Believing a fence is open here is the unsafe direction: the next
	// table would be left unfenced and its cells would render as live markdown.
	if got := fenceStateAfter("log said \\``` then more", ""); got != "" {
		t.Errorf("escaped marker outside a block opened one: %q", got)
	}
	// An escaped backslash is a literal backslash, so the marker after it is real.
	if got := fenceStateAfter("path C:\\\\```", ""); got != fenceMarker {
		t.Errorf("marker after an escaped backslash was skipped: %q", got)
	}
	// Inside a block the backslash is just a character, so the marker still closes.
	if got := fenceStateAfter("```\ncode \\``` after", ""); got != "" {
		t.Errorf("escaped marker inside a block failed to close it: %q", got)
	}
}

func TestFormatTablesFencesATableAfterAnEscapedMarker(t *testing.T) {
	in := strings.Join([]string{
		"The agent logged \\``` while quoting the pod output.",
		"| Issue | Count |",
		"|---|---|",
		"| PLEG | 2 |",
	}, "\n")

	got := formatTables(in)
	if !strings.Contains(got, fenceMarker+"\nIssue  Count") {
		t.Errorf("table after an escaped marker was left unfenced:\n%s", got)
	}
}

func TestSplitMessageBalancesFencesAroundAMidLineMarker(t *testing.T) {
	var b strings.Builder
	b.WriteString("```\n")
	for i := 0; i < 30; i++ {
		b.WriteString("2026-08-22T12:12:58.0Z worker-01 pleg not healthy\n")
		if i == 5 {
			b.WriteString("err: stray ``` inside a log line\n")
		}
	}
	b.WriteString("```\n")

	for _, c := range splitMessage(b.String(), 200) {
		if n := strings.Count(c, fenceMarker); n%2 != 0 {
			t.Errorf("chunk has %d fence markers, want an even count:\n%s", n, c)
		}
	}
}

func TestSplitMessageNeverExceedsTheLimit(t *testing.T) {
	const maxLen = discordMaxMessageLen

	inputs := map[string]string{
		// A reply truncated mid-code-block leaves an open fence; the closing one
		// this adds must not push the chunk past the limit.
		"fenced block, no newlines":   fenceMarker + "\n" + strings.Repeat("y", 9000),
		"absurdly long language tag":  fenceMarker + strings.Repeat("z", 2100),
		"wide runes across the limit": fenceMarker + "\n" + strings.Repeat("节点未就绪 ", 900),
		"no fences at all":            strings.Repeat("plain text line\n", 400),
	}
	// Sweep the boundary: the off-by-four only shows at exact lengths.
	for pad := 1988; pad <= 2004; pad++ {
		inputs["unclosed fence + "+strconv.Itoa(pad)] = fenceMarker + "\n" + strings.Repeat("x", pad)
	}

	for name, in := range inputs {
		var rebuilt strings.Builder
		for i, c := range splitMessage(in, maxLen) {
			if len(c) > maxLen {
				t.Errorf("%s: chunk %d is %d bytes, want <= %d", name, i, len(c), maxLen)
			}
			if n := strings.Count(c, fenceMarker); n%2 != 0 {
				t.Errorf("%s: chunk %d has %d fence markers, want an even count", name, i, n)
			}
			rebuilt.WriteString(c)
		}
		// Fences and the whitespace around them are the only things splitting may
		// add or drop; every other byte has to come back.
		if got, want := stripFences(rebuilt.String()), stripFences(in); got != want {
			t.Errorf("%s: splitting lost content (%d bytes back, want %d)", name, len(got), len(want))
		}
	}
}

// stripFences removes fence markers and all whitespace, leaving only the payload
// splitting has to preserve.
func stripFences(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, fenceMarker, "")), "")
}

func TestSplitMessageTinyLimitStillRespectsIt(t *testing.T) {
	in := fenceMarker + "\nnode – worker-01 unhealthy\n" + fenceMarker
	for _, maxLen := range []int{4, 16, 31} {
		var rebuilt strings.Builder
		for i, c := range splitMessage(in, maxLen) {
			if len(c) > maxLen {
				t.Errorf("maxLen %d: chunk %d is %d bytes", maxLen, i, len(c))
			}
			rebuilt.WriteString(c)
		}
		if rebuilt.String() != in {
			t.Errorf("maxLen %d: content lost, got %q", maxLen, rebuilt.String())
		}
	}
}
func TestFormatTablesHandlesAFenceMarkerInAHeaderCell(t *testing.T) {
	// One backtick run in a header used to be counted as a real fence: the row
	// stayed unparsed and every later table rendered without a fence, which is
	// the #45 failure mode reintroduced.
	in := strings.Join([]string{
		"| Pod | ``` note |",
		"|---|---|",
		"| worker-01 | ok |",
		"",
		"| Issue | Count |",
		"|---|---|",
		"| PLEG | 2 |",
	}, "\n")

	got := formatTables(in)
	if n := strings.Count(got, fenceMarker); n != 4 {
		t.Errorf("want 4 fence markers (two balanced tables), got %d:\n%s", n, got)
	}
	if strings.Contains(got, "| Pod |") {
		t.Errorf("header row with a marker was emitted raw:\n%s", got)
	}
	if !strings.Contains(got, fenceMarker+"\nIssue  Count") {
		t.Errorf("the following table lost its fence:\n%s", got)
	}
}

func TestSplitCellsKeepsEscapedPipes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"| a \\| b | c |", []string{"a | b", "c"}},
		{"| plain | cell |", []string{"plain", "cell"}},
		{"| has\x00nul | c |", []string{"hasnul", "c"}}, // the stand-in cannot survive
	}
	for _, tc := range cases {
		got := splitCells(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("splitCells(%q) = %q, want %q", tc.in, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("splitCells(%q) cell %d = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestFormatTablesKeepsAnEscapedPipeInItsColumn(t *testing.T) {
	// A shifted row would put a value under the wrong header, which an operator
	// would read as fact.
	in := strings.Join([]string{
		"| Pod | Command | State |",
		"|---|---|---|",
		"| worker-01 | ps \\| grep kubelet | Running |",
	}, "\n")

	got := formatTables(in)
	want := strings.Join([]string{
		"```",
		"Pod        Command            State",
		"---------  -----------------  -------",
		"worker-01  ps | grep kubelet  Running",
		"```",
	}, "\n")
	if got != want {
		t.Errorf("escaped pipe shifted the row:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
