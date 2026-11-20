package store

import (
	"strings"
	"testing"
)

// The usage columns, their scan destinations and the values written must
// stay in step.
func TestUsageColumnsInStep(t *testing.T) {
	cols := strings.Count(usageColumns, ",") + 1
	if written := len(JobUsage{}.values()); cols != usageColumnCount || written != cols {
		t.Fatalf("%d usage columns, %d values written, want %d", cols, written, usageColumnCount)
	}
	if n := strings.Count(querySetJobUsage, "?"); n != cols+1 {
		t.Fatalf("querySetJobUsage takes %d arguments, want %d", n, cols+1)
	}
}
