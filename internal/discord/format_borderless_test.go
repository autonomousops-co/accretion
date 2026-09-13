package discord

import (
	"strings"
	"testing"
)

// The bot's actual Discord output, 2026-08-27 — the borderless form that #45's
// fix did not detect.
func TestFormatTables_BorderlessIsRendered(t *testing.T) {
	in := strings.Join([]string{
		"Here's a snapshot:",
		"",
		"Resource | Namespace | Count",
		"---|---|---",
		"node_status | (cluster-wide) | 3",
		"pod_status | accretion-db | 2",
		"",
		"Everything looks normal.",
	}, "\n")
	got := formatTables(in)
	if strings.Contains(got, "---|---|---") {
		t.Fatalf("separator survived; table not detected:\n%s", got)
	}
	if !strings.Contains(got, "```") {
		t.Fatalf("no code fence emitted:\n%s", got)
	}
	for _, want := range []string{"node_status", "(cluster-wide)", "pod_status", "accretion-db"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost cell %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Everything looks normal.") {
		t.Errorf("trailing prose lost:\n%s", got)
	}
}

// The bordered form must render exactly as before.
func TestFormatTables_BorderedUnchanged(t *testing.T) {
	in := strings.Join([]string{
		"| Resource | Namespace | Count |",
		"|---|---|---|",
		"| node_status | (cluster-wide) | 3 |",
	}, "\n")
	got := formatTables(in)
	want := strings.Join([]string{
		"```",
		"Resource     Namespace       Count",
		"-----------  --------------  -----",
		"node_status  (cluster-wide)  3",
		"```",
	}, "\n")
	if got != want {
		t.Errorf("bordered form changed.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// The property isTableRow's leading-pipe rule existed to protect: a shell
// pipeline after a table must not be absorbed into it.
func TestFormatTables_PipelineAfterBorderedTableNotSwallowed(t *testing.T) {
	in := strings.Join([]string{
		"| A | B |",
		"|---|---|",
		"| 1 | 2 |",
		"cat foo | grep bar",
	}, "\n")
	got := formatTables(in)
	if !strings.Contains(got, "cat foo | grep bar") {
		t.Errorf("pipeline was swallowed or rewritten:\n%s", got)
	}
}

func TestFormatTables_PipelineAfterBorderlessTableNotSwallowed(t *testing.T) {
	// Three-column table, then a two-cell pipeline: the count guard ends the table.
	in := strings.Join([]string{
		"A | B | C",
		"---|---|---",
		"1 | 2 | 3",
		"cat foo | grep bar",
	}, "\n")
	got := formatTables(in)
	if !strings.Contains(got, "cat foo | grep bar") {
		t.Errorf("pipeline was swallowed:\n%s", got)
	}
	if strings.Contains(got, "---|---|---") {
		t.Errorf("table itself was not rendered:\n%s", got)
	}
}

// Ragged rows must not panic or mis-column.
func TestFormatTables_RaggedBorderlessRows(t *testing.T) {
	in := strings.Join([]string{
		"A | B | C",
		"---|---|---",
		"1 | 2 | 3",
		"4 | 5",         // fewer cells — ends the table, stays as prose
		"7 | 8 | 9 | 0", // more cells — also ends it
	}, "\n")
	got := formatTables(in) // must not panic
	if strings.Contains(got, "---|---|---") {
		t.Errorf("table not rendered:\n%s", got)
	}
}

// A fenced code block containing pipes is left alone.
func TestFormatTables_FencedPipesUntouched(t *testing.T) {
	in := strings.Join([]string{
		"```",
		"ps aux | grep go",
		"```",
	}, "\n")
	if got := formatTables(in); got != in {
		t.Errorf("fenced content altered.\ngot:\n%s\nwant:\n%s", got, in)
	}
}

// Prose containing a pipe, with no separator anywhere, is not a table.
func TestFormatTables_ProseWithPipeIsNotATable(t *testing.T) {
	in := "run `kubectl get pods | head` to check"
	if got := formatTables(in); got != in {
		t.Errorf("prose altered:\ngot:  %s\nwant: %s", got, in)
	}
}
