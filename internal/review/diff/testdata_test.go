package diff

import (
	"path/filepath"
	"strings"
)

// testdataDir is this package's testdata, resolved at init (cwd = package dir) so goldens stay machine-independent.
var testdataDir = func() string {
	abs, _ := filepath.Abs("testdata")
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}()

// expandGolden swaps the @TESTDATA@ placeholder in a golden for this checkout's testdata path.
func expandGolden(s string) string { return strings.ReplaceAll(s, "@TESTDATA@", testdataDir) }
