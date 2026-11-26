package ansi

import (
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// TestStatusNotVerifiedStaysGray: the longer "not verified" token wins, so its "verified" is never painted green.
func TestStatusNotVerifiedStaysGray(t *testing.T) {
	got := Status(true, "gemini  not verified")
	if want := "gemini  " + Dim + "not verified" + Reset; got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}
	if got := Status(true, "verified"); got != Green+"verified"+Reset {
		t.Fatalf("Status(verified) = %q", got)
	}
}

// A token inside a longer word stays unpainted; edges that are not word characters ("FIX:") still match.
func TestStatusTokensMatchWholeWords(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"unpinned", "unpinned"},
		{"representation", "representation"},
		{"pinned: x", Green + "pinned" + Reset + ": x"},
		{"FIX: y", Red + "FIX:" + Reset + " y"},
		{"(present)", "(" + Green + "present" + Reset + ")"},
	} {
		if got := Status(true, c.in); got != c.want {
			t.Errorf("Status(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStatusNotLoggedInIsOneToken: the negative phrase is one styled token, so its "logged in" tail is never split off.
func TestStatusNotLoggedInIsOneToken(t *testing.T) {
	got := Status(true, "[not logged in]")
	if !strings.Contains(got, "not logged in"+Reset) || strings.Contains(got, Green) {
		t.Fatalf("Status = %q, want one styled token with no green span", got)
	}
	if got := Status(true, "[logged in]"); !strings.Contains(got, Green+"logged in"+Reset) {
		t.Fatalf("Status(logged in) = %q", got)
	}
}

var oracleStatusCode = func() map[string]string {
	m := make(map[string]string, len(statusTokens))
	for _, t := range statusTokens {
		m[t.token] = t.code
	}
	return m
}()

var oracleStatusRE = sync.OnceValue(func() *regexp.Regexp {
	toks := make([]string, len(statusTokens))
	for i, t := range statusTokens {
		toks[i] = t.token
	}
	sort.Slice(toks, func(i, j int) bool { return len(toks[i]) > len(toks[j]) })
	for i, t := range toks {
		toks[i] = oracleWordBounded(t)
	}
	return regexp.MustCompile(strings.Join(toks, "|"))
})

// oracleWordBounded quotes tok with \b on each word-character edge, so "pinned" never paints inside "unpinned".
func oracleWordBounded(tok string) string {
	q := regexp.QuoteMeta(tok)
	if oracleIsWordByte(tok[0]) {
		q = `\b` + q
	}
	if oracleIsWordByte(tok[len(tok)-1]) {
		q += `\b`
	}
	return q
}

func oracleIsWordByte(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func regexpStatus(on bool, line string) string {
	if !on {
		return Strip(line)
	}
	if !strings.ContainsAny(line, "\ue000\ue001\ue003") {
		return regexpPaintTokens(line)
	}
	var out strings.Builder
	last := 0
	for _, loc := range spanRE().FindAllStringIndex(line, -1) {
		out.WriteString(regexpPaintTokens(line[last:loc[0]]))
		out.WriteString(paintSpan(line[loc[0]:loc[1]]))
		last = loc[1]
	}
	out.WriteString(regexpPaintTokens(line[last:]))
	return out.String()
}

func regexpPaintTokens(text string) string {
	return oracleStatusRE().ReplaceAllStringFunc(text, func(tok string) string { return oracleStatusCode[tok] + tok + Reset })
}

const (
	statusFrameRows  = 100
	statusRandomRows = 2000
	statusAllocRuns  = 100
)

var statusBenchmarkResult string

func BenchmarkStatus(b *testing.B) {
	rows := []struct{ name, text string }{
		{"no-token", "  Claude Sonnet 5.5  Balanced  vision · reasoning  API key"},
		{"one-token", "  Claude Sonnet 5.5  verified  vision · reasoning  API key"},
		{"badge", "  Claude Sonnet 5.5  [not logged in]  vision · reasoning"},
	}
	for _, row := range rows {
		b.Run(row.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				statusBenchmarkResult = Status(true, row.text)
			}
		})
	}
	b.Run("frame-100", func(b *testing.B) {
		frame := make([]string, statusFrameRows)
		for i := range len(frame) {
			frame[i] = rows[0].text
		}
		for i := range statusFrameRows / 10 {
			frame[i*10] = rows[1+i%2].text
		}
		b.ReportAllocs()
		for b.Loop() {
			for _, row := range frame {
				statusBenchmarkResult = Status(true, row)
			}
		}
	})
}

func TestStatusRegexpIdentity(t *testing.T) {
	corpus := append(statusGeneratedCorpus(), statusRows...)
	seen := make(map[string]bool, len(corpus))
	for _, text := range corpus {
		if seen[text] {
			continue
		}
		seen[text] = true
		for _, on := range []bool{false, true} {
			if got, want := Status(on, text), regexpStatus(on, text); got != want {
				t.Fatalf("Status(%t, %q) = %q, oracle %q", on, text, got, want)
			}
		}
	}
	if n := testing.AllocsPerRun(statusAllocRuns, func() { statusBenchmarkResult = Status(true, "no matching status here") }); n != 0 {
		t.Fatalf("no-token allocations = %g, want 0", n)
	}
	t.Logf("%d inputs, %d comparisons, zero differences", len(seen), 2*len(seen))
}

func statusGeneratedCorpus() []string {
	pieces := []string{"", " ", "_", "9", ".", "[", "]", "中", "😀", "\xff", "\x1b[32m", Reset,
		SpanOK, SpanOff, SpanWarn, SpanEnd, "active", "VeRiFiEd"}
	corpus := append([]string(nil), pieces...)
	for _, tok := range statusTokens {
		word := tok.token
		pieces = append(pieces, word)
		corpus = append(corpus, word, statusMixedCase(word), "x"+word, word+"y", "9"+word, word+"9", strings.ToUpper(word), strings.ToLower(word), "x"+word+"y", word+word,
			"["+word+"]", "("+word+")", word+" "+word, "a_"+word, word+"_9", "中"+word+"😀",
			"😀"+word+"中", "\xff"+word+"\xfe", Green+word+Reset, word+Green, Green+word,
			word[:1]+Reset+word[1:], SpanOK+word+SpanEnd, SpanWarn+word+SpanEnd, SpanOff+word+SpanEnd)
		for _, other := range statusTokens {
			corpus = append(corpus, word+" "+other.token, word+other.token)
			for i := range len(word) - 1 {
				k := i + 1
				if strings.HasPrefix(other.token, word[k:]) {
					corpus = append(corpus, word[:k]+other.token)
				}
			}
		}
	}
	rng := rand.New(rand.NewPCG(42, 17))
	for range statusRandomRows {
		var row strings.Builder
		for range 1 + rng.IntN(5) {
			row.WriteString(pieces[rng.IntN(len(pieces))])
		}
		corpus = append(corpus, row.String())
	}
	return corpus
}

func statusMixedCase(word string) string {
	mixed := []byte(word)
	for i := range len(mixed) {
		if i%2 == 0 && mixed[i] >= 'a' && mixed[i] <= 'z' {
			mixed[i] -= 'a' - 'A'
		}
		if i%2 == 1 && mixed[i] >= 'A' && mixed[i] <= 'Z' {
			mixed[i] += 'a' - 'A'
		}
	}
	return string(mixed)
}

// statusRows are real menu rows: spans, nested escapes, a badge split by colour, wide runes.
var statusRows = []string{
	"codex      present (ChatGPT login) · cred:ok · ABSENT",
	"gemini  not verified",
	"claude-opus-5…\x1b[37m  Claude Opus 5…  Agentic coding  vision · reasoning\x1b[0m",
	"gpt-6.1-sol     GPT-6.1 Sol     日本語のタイ…   vision · reasoning",
	"API key\x1b[38;2;180;180;180m       paste a key…\x1b[0m \x1b[38;2;71;150;228m[verified\x1b[0m\x1b[38;2;71;150;228m]\x1b[0m",
	"claude     ABSENT: claude-code vault set OPENAI_API_KEY",
	"codex: set up stopped at verify: 402 payment required\n",
	"a" + SpanOK + "verified" + SpanEnd + " pinned " + SpanWarn + "not verified" + SpanEnd,
}

func TestEnabledEnv(t *testing.T) {
	for _, c := range []struct {
		noColor, term, force string
		want                 bool
	}{
		{"", "xterm", "1", true},
		{"", "dumb", "1", false},
		{"1", "xterm", "1", false},
		{"", "xterm", "0", false},
	} {
		t.Setenv("NO_COLOR", c.noColor)
		t.Setenv("TERM", c.term)
		t.Setenv("FORCE_COLOR", c.force)
		if got := Enabled(&strings.Builder{}); got != c.want {
			t.Errorf("%+v: Enabled = %v", c, got)
		}
	}
}
