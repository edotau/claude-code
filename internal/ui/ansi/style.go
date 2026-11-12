package ansi

import (
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

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
		return Strip(line)
	}
	var out strings.Builder
	last := 0
	for _, loc := range spanRE.FindAllStringIndex(line, -1) {
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
	return statusRE.ReplaceAllStringFunc(text, func(tok string) string { return statusCode[tok] + tok + Reset })
}

// Span markers are zero-meaning runes a row wraps around one value: SpanOK..SpanEnd paints green, SpanWarn yellow and
// SpanOff dim, in Status only; Strip removes them wherever a row prints unpainted.
const (
	SpanOK   = "\ue000"
	SpanOff  = "\ue001"
	SpanEnd  = "\ue002"
	SpanWarn = "\ue003"
)

var spanRE = regexp.MustCompile("[\ue000\ue001\ue003][^\ue002]*\ue002")

var spanStrip = strings.NewReplacer(SpanOK, "", SpanOff, "", SpanWarn, "", SpanEnd, "")

// Strip removes the span markers from s.
func Strip(s string) string { return spanStrip.Replace(s) }

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
