package ansi

import (
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/proc"
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
	return ok && proc.IsTerminal(f)
}

// Style wraps s in code (one or more SGR escapes) and a reset when on; otherwise s unchanged.
func Style(on bool, code, s string) string {
	if !on || s == "" {
		return s
	}
	return code + s + Reset
}

// statusTokens are the words Status paints; longer tokens precede their prefixes so "cred:ok" wins over "ok".
var statusTokens = []struct{ token, code string }{
	{"cred:missing", Red}, {"cred:ok", Green}, {"rejected", Red}, {"not verified", Dim}, {"not logged in", Dim},
	{"REJECTED", Red}, {"ABSENT", Red}, {"DRIFT", Red}, {"FIX:", Red}, {"ERROR", Red}, {"failed", Red},
	{"unreachable", Yellow}, {"unverified", Yellow}, {"skipped", Dim}, {"KEEP", Dim},
	{"present", Green}, {"verified", Green}, {"routes to", Green}, {"runs on", Green}, {"logged in", Green},
	{"pinned", Green}, {"unchanged", Dim},
}

// Status paints known status tokens longest first, so "not verified" stays gray rather than painting "verified" green.
func Status(on bool, line string) string {
	if !on {
		return Strip(line)
	}
	if !strings.ContainsAny(line, "\ue000\ue001\ue003") {
		return paintTokens(line)
	}
	var out strings.Builder
	last := 0
	for _, loc := range spanRE().FindAllStringIndex(line, -1) {
		out.WriteString(paintTokens(line[last:loc[0]]))
		out.WriteString(paintSpan(line[loc[0]:loc[1]]))
		last = loc[1]
	}
	out.WriteString(paintTokens(line[last:]))
	return out.String()
}

// paintSpan paints one span-marked value in its color, dropping the markers.
func paintSpan(sp string) string {
	code := Dim
	switch {
	case strings.HasPrefix(sp, SpanOK):
		code = Green
	case strings.HasPrefix(sp, SpanWarn):
		code = Yellow
	}
	return code + strings.TrimSuffix(sp[len(SpanOK):], SpanEnd) + Reset
}

// paintTokens paints the status tokens in text that sits outside any span.
func paintTokens(text string) string {
	return statusRE().ReplaceAllStringFunc(text, func(tok string) string { return statusCode[tok] + tok + Reset })
}

// Span markers wrap one value in a row: SpanOK..SpanEnd green, SpanWarn yellow, SpanOff dim; Strip removes them.
const (
	SpanOK   = "\ue000"
	SpanOff  = "\ue001"
	SpanEnd  = "\ue002"
	SpanWarn = "\ue003"
)

var spanRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("[\ue000\ue001\ue003][^\ue002]*\ue002") })

var spanStrip = strings.NewReplacer(SpanOK, "", SpanOff, "", SpanWarn, "", SpanEnd, "")

// Strip removes the span markers from s.
func Strip(s string) string {
	if !strings.ContainsAny(s, "\ue000\ue001\ue002\ue003") {
		return s
	}
	return spanStrip.Replace(s)
}

var statusCode = func() map[string]string {
	m := make(map[string]string, len(statusTokens))
	for _, t := range statusTokens {
		m[t.token] = t.code
	}
	return m
}()

var statusRE = sync.OnceValue(func() *regexp.Regexp {
	toks := make([]string, len(statusTokens))
	for i, t := range statusTokens {
		toks[i] = t.token
	}
	sort.Slice(toks, func(i, j int) bool { return len(toks[i]) > len(toks[j]) })
	for i, t := range toks {
		toks[i] = wordBounded(t)
	}
	return regexp.MustCompile(strings.Join(toks, "|"))
})

// wordBounded quotes tok with \b on each word-character edge, so "pinned" never paints inside "unpinned".
func wordBounded(tok string) string {
	q := regexp.QuoteMeta(tok)
	if isWordByte(tok[0]) {
		q = `\b` + q
	}
	if isWordByte(tok[len(tok)-1]) {
		q += `\b`
	}
	return q
}

func isWordByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}
