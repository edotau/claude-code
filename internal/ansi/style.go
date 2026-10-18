package ansi

import (
	"io"
	"os"
	"strings"
)

// Semantic 16-color SGR codes for plain CLI output: every terminal renders them, and they compose with Bold.
const (
	Red    = "\x1b[31m"
	Green  = "\x1b[32m"
	Yellow = "\x1b[33m"
	Cyan   = "\x1b[36m"
	Dim    = "\x1b[2m"
)

// Enabled follows https://no-color.org: color only when w is a terminal and NO_COLOR is unset.
func Enabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && IsTerminal(f)
}

// IsTerminal reports whether f is a real terminal. /dev/null is itself a char device, so
// ModeCharDevice alone misreports `cmd </dev/null` as interactive; os.SameFile rules it out.
func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}

// Style wraps s in code (one or more SGR escapes) and a reset when on; otherwise s unchanged.
func Style(on bool, code, s string) string {
	if !on || s == "" {
		return s
	}
	return code + s + Reset
}

// statusTokens are the words the harness's status lines pivot on; Status paints them so a reader's eye lands
// on the verdict first. Longer tokens are listed before their prefixes so "cred:ok" wins over "ok".
var statusTokens = []struct{ token, code string }{
	{"cred:missing", Red}, {"cred:ok", Green},
	{"REJECTED", Red}, {"ABSENT", Red}, {"DRIFT", Red}, {"FIX:", Red}, {"ERROR", Red}, {"failed", Red},
	{"unreachable", Yellow}, {"unverified", Yellow}, {"skipped", Dim}, {"KEEP", Dim},
	{"present", Green}, {"verified", Green}, {"routes to", Green}, {"runs on", Green}, {"logged in", Green},
	{"pinned", Green}, {"unchanged", Dim},
}

// Status paints the known status tokens in line when on; the text is otherwise byte-identical.
func Status(on bool, line string) string {
	if !on {
		return line
	}
	for _, t := range statusTokens {
		if strings.Contains(line, t.token) {
			line = strings.ReplaceAll(line, t.token, t.code+t.token+Reset)
		}
	}
	return line
}
