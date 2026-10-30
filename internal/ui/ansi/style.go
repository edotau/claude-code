package ansi

import (
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Columns is $COLUMNS (trimmed) when it parses to > 0, else def.
func Columns(def int) int {
	if v := strings.TrimSpace(os.Getenv("COLUMNS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

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
	{"cred:missing", Red}, {"cred:ok", Green}, {"rejected", Red}, {"not verified", Dim},
	{"REJECTED", Red}, {"ABSENT", Red}, {"DRIFT", Red}, {"FIX:", Red}, {"ERROR", Red}, {"failed", Red},
	{"unreachable", Yellow}, {"unverified", Yellow}, {"skipped", Dim}, {"KEEP", Dim},
	{"present", Green}, {"verified", Green}, {"routes to", Green}, {"runs on", Green}, {"logged in", Green},
	{"pinned", Green}, {"unchanged", Dim},
}

// Status paints the known status tokens in line when on, in one pass, longest token first — so "not verified" stays
// gray instead of having its "verified" painted green; the text is otherwise byte-identical.
func Status(on bool, line string) string {
	if !on {
		return line
	}
	return statusRE.ReplaceAllStringFunc(line, func(tok string) string { return statusCode[tok] + tok + Reset })
}

var statusCode = func() map[string]string {
	m := make(map[string]string, len(statusTokens))
	for _, t := range statusTokens {
		m[t.token] = t.code
	}
	return m
}()

var statusRE = func() *regexp.Regexp {
	toks := make([]string, len(statusTokens))
	for i, t := range statusTokens {
		toks[i] = regexp.QuoteMeta(t.token)
	}
	sort.Slice(toks, func(i, j int) bool { return len(toks[i]) > len(toks[j]) })
	return regexp.MustCompile(strings.Join(toks, "|"))
}()
