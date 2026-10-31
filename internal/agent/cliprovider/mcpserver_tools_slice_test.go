package cliprovider

import (
	"strconv"
	"strings"
	"testing"
)

// Regression for legacy Read(start_line=10,end_line=20): end_line is absolute,
// inclusive, rather than a count applied after slicing from start_line.
// REVERT CHECK: change sliceLines to slice from start and then take end lines;
// this test returns lines 10-29 (or fails its expected value) instead of 10-20.
func TestSliceLinesUsesAbsoluteEndLine(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = strconv.Itoa(i + 1)
	}
	got := sliceLines(strings.Join(lines, "\n"), 10, 20)
	want := strings.Join(lines[9:20], "\n")
	if got != want {
		t.Fatalf("sliceLines(start=10,end=20) = %q, want %q", got, want)
	}
}
