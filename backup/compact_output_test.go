package backup

import (
	"strings"
	"testing"
)

func TestCompactOutputCollapsesRepeats(t *testing.T) {
	var lines []string
	lines = append(lines, "[00] 00:21:43 Starting BACKUP STAGE START")
	for i := 0; i < 60; i++ {
		lines = append(lines, "[00] 00:21:"+string(rune('0'+i%10))+" Waiting for query 177684 (duration "+string(rune('0'+i%10))+" sec): SELECT 1")
	}
	lines = append(lines, "[00] 00:22:44 Unable to obtain lock. Please try again later.")
	got := compactOutput(strings.Join(lines, "\r\n"))
	want := strings.Join([]string{
		lines[0],
		lines[1],
		"... (58 similar lines omitted) ...",
		lines[60],
		lines[61],
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestCompactOutputKeepsShortRunsAndDistinctLines(t *testing.T) {
	in := "Connection attempt 1\nConnection attempt 2\nERROR 2003\nFailed to connect"
	if got := compactOutput(in + "\n"); got != in {
		t.Errorf("got %q, want %q", got, in)
	}
	if got := compactOutput("  \r\n "); got != "" {
		t.Errorf("blank output: got %q", got)
	}
}
