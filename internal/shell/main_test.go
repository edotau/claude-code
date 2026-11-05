package shell

import (
	"os"
	"strings"
	"testing"
)

// TestMain treats every path as an existing directory unless it contains "missing"; statDir has its own test.
func TestMain(m *testing.M) {
	dirExists = func(p string) bool { return !strings.Contains(p, "missing") }
	os.Exit(m.Run())
}
