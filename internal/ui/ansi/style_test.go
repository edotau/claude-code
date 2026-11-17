package ansi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	corpus := append(statusGeneratedCorpus(), statusTestRows...)
	corpus = append(corpus, statusTestLiterals(t)...)
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

// Read every test literal as well as captured rows so new fixtures enter the oracle sweep automatically.
func statusTestLiterals(t *testing.T) []string {
	t.Helper()
	var corpus []string
	for _, dir := range []string{".", "../tui", "../menu"} {
		paths, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				corpus = append(corpus, value)
				return true
			})
		}
	}
	return corpus
}

// Captured at Status's boundary while running the existing ansi, tui and menu tests.
var statusTestRows = []string{
	"codex      present (ChatGPT login) · cred:ok · ABSENT",
	"gemini  not verified",
	"verified",
	"unpinned",
	"representation",
	"pinned: x",
	"FIX: y",
	"(present)",
	"[not logged in]",
	"[logged in]",
	"in use by claude-code",
	"environment (unset to disconnect)",
	"",
	"a           x",
	"b           y",
	"Back        return to the previous menu",
	"a",
	"b",
	"Enter",
	"claude-sonnet…  Claude Sonnet…  Balanced        vision · reas…  xxxxxxxxxxxxx…",
	"claude-opus-5…  Claude Opus 5…  Agentic coding  vision · reasoning",
	"gpt-6.1-sol     GPT-6.1 Sol     日本語のタイ…   vision · reasoning",
	"Back            return to the previous menu",
	"claude-sonnet…\x1b[37m  Claude Sonnet…  Balanced        vision · reas…  xxxxxxxxxxxxx…\x1b[0m",
	"claude-opus-5…\x1b[37m  Claude Opus 5…  Agentic coding  vision · reasoning\x1b[0m",
	"gpt-6.1-sol\x1b[37m     GPT-6.1 Sol     日本語のタイ…   vision · reasoning\x1b[0m",
	"Back\x1b[37m            return to the previous menu\x1b[0m",
	"tailscale  off",
	"c",
	"Subscription  OAuth · plan limits · Remote Control             [not logged in]",
	"API key       paste a key and keep it in the vault for every…  [verified]",
	"Back          return to the previous menu",
	"Subscription\x1b[38;2;180;180;180m  OAuth · plan limits · Remote Control            \x1b[0m \x1b[38;2;71;150;228m[not logged in\x1b[0m\x1b[38;2;71;150;228m]\x1b[0m",
	"API key\x1b[38;2;180;180;180m       paste a key and keep it in the vault for every… \x1b[0m \x1b[38;2;71;150;228m[verified\x1b[0m\x1b[38;2;71;150;228m]\x1b[0m",
	"Back\x1b[38;2;180;180;180m          return to the previous menu\x1b[0m",
	"  pasted  ghp_************************* (29 chars)\n",
	"wrote COPILOT_GITHUB_TOKEN to the vault (ghp_p*** (29 chars))\n",
	"  pasted  ** (2 chars)\n",
	"COPILOT_GITHUB_TOKEN not a GitHub token (want gho_/ghp_/ghu_/github_pat_, >=20 chars)\n",
	"copilot — effort\n",
	"  1) unset                  copilot's own default (current)\n",
	"  2) Low                    low\n",
	"  3) High                   high\n",
	"  copilot effort: low\n",
	"  1) unset                  copilot's own default\n",
	"  2) Low                    low (current)\n",
	"  copilot effort: unset\n",
	"codex — sign in\n",
	"  1) ChatGPT (browser)      codex login\n",
	"  2) ChatGPT (headless)     codex login --device-auth\n",
	"  3) API key                paste OPENAI_API_KEY, then codex login --with-api-key\n",
	"codex: codex login exited without a usable login; run it again\n",
	"codex: codex login --device-auth exited without a usable login; run it again\n",
	"codex      now bills OPENAI_API_KEY per token\n",
	"claude: verified with one live request\n",
	"codex: verified with one live request\n",
	"antigravity: verified with one live request\n",
	"opencode: verified with one live request\n",
	"copilot: verified with one live request\n",
	"claude: already signed in\n",
	"codex: already signed in\n",
	"antigravity: already signed in\n",
	"opencode: already signed in\n",
	"copilot: already signed in\n",
	"claude: set up stopped at sign-in: boom\n",
	"codex: set up stopped at sign-in: boom\n",
	"antigravity: set up stopped at sign-in: boom\n",
	"opencode: set up stopped at sign-in: boom\n",
	"copilot: set up stopped at sign-in: boom\n",
	"claude: set up stopped at verify: boom\n",
	"codex: set up stopped at verify: boom\n",
	"antigravity: set up stopped at verify: boom\n",
	"opencode: set up stopped at verify: boom\n",
	"copilot: set up stopped at verify: boom\n",
	"claude: set up stopped at route: boom\n",
	"codex: set up stopped at route: boom\n",
	"antigravity: set up stopped at route: boom\n",
	"opencode: set up stopped at route: boom\n",
	"copilot: set up stopped at route: boom\n",
	"claude: signed in; the route was not changed\n",
	"codex: signed in; the route was not changed\n",
	"antigravity: signed in; the route was not changed\n",
	"opencode: signed in; the route was not changed\n",
	"copilot: signed in; the route was not changed\n",
	"copilot: signed in, but copilot has no credits left\n",
	"codex: set up stopped at verify: 402 payment required\n",
	"codex: set up stopped at verify: 401 with Bearer REDACTED\n",
	"codex — profile\n",
	"  1) off                    \n",
	"  2) chatgpt                 (current)\n",
	"  3) openai                 \n",
	"codex: set up stopped at route: open /var/folders/pd/c1tqgl6567vf85kfs9ynxx040000gn/T/TestSetUpRouteFailedPinWriteIsReported3343920905/001/state/vault.db: permission denied\n",
	"agy        signed in\n",
	"agy: not signed in\n",
	"agy: not signed in (timed out)\n",
	"  3) Medium (default)       medium\n",
	"  4) High                   high\n",
	"copilot: cleared effort pin (high) — new-model doesn't support that effort\n",
	"  copilot context: unset\n",
	"copilot: cleared context pin (long_context) — new-model has no long-context tier\n",
	"  1) off                     (current)\n",
	"  2) chatgpt                \n",
	"  codex profile: chatgpt\n",
	"  codex profile: unset\n",
	"opencode — profile\n",
	"  2) zen-free               \n",
	"  3) anthropic              \n",
	"  4) openai                 \n",
	"  5) opencode-go            \n",
	"  6) github-copilot         \n",
	"  7) nvidia                 \n",
	"  opencode profile: zen-free\n",
	"Seed agent profiles\n",
	"  1) claude                 automatic\n",
	"  2) codex                  chatgpt\n",
	"  3) copilot                copilot\n",
	"  4) antigravity            antigravity\n",
	"  5) opencode               zen-free\n",
	"  6) Save                   write these choices\n",
	"copilot — profile\n",
	"  2) copilot                 (current)\n",
	"  2) codex                  off\n",
	"agent profiles saved\n",
	"no agent profiles set: run `claude-code configure` → seed agents\n",
	"copilot: pick an account; gh's active account is GitHub auth (configure github)\n",
	"seed: codex: unknown profile \"no-such-profile\"\n",
	"  claude profile: prod\n",
	"  codex profile: prod\n",
	"  gemini profile: prod\n",
	"  opencode profile: prod\n",
	"  copilot profile: prod\n",
	"  claude profile: ultra\n",
	"  gemini profile: antigravity\n",
	"  copilot profile: copilot\n",
	"  copilot fallback: prod\n",
	"  claude profile: dev\n",
	"  codex profile: dev\n",
	"  gemini profile: dev\n",
	"  opencode profile: dev\n",
	"  copilot profile: dev\n",
	"Codex settings\n",
	"  1) model_reasoning_effort Codex default · default\n",
	"  2) model_verbosity        high · native\n",
	"  3) Save and apply         Codex only\n",
	"model_verbosity\n",
	"  1) low                    \n",
	"  2) medium                 \n",
	"  3) high                   \n",
	"  4) Codex default          remove harness override\n",
	"  2) model_verbosity        low · vault\n",
	"codex: preferences saved and applied\n",
	"== codex ==\n",
	"codex settings: /var/folders/pd/c1tqgl6567vf85kfs9ynxx040000gn/T/TestCodexSettingsReportsNeverWritereport584616084/002/.codex/config.toml\n",
	"model_reasoning_effort  Codex default  default\n",
	"model_verbosity  low  vault · live differs\n",
	"codex: saved preferences differ from native config\n",
	"codex settings: /var/folders/pd/c1tqgl6567vf85kfs9ynxx040000gn/T/TestCodexSettingsReportsNeverWritecheck1205449243/002/.codex/config.toml\n",
	"codex settings: /var/folders/pd/c1tqgl6567vf85kfs9ynxx040000gn/T/TestCodexSettingsReportsNeverWritedry-run2079477163/002/.codex/config.toml\n",
	"codex settings: /var/folders/pd/c1tqgl6567vf85kfs9ynxx040000gn/T/TestCodexSettingsReportsNeverWritepiped119685471/002/.codex/config.toml\n",
	"  2) model_verbosity        Codex default · default\n",
	"codex: preferences saved; native apply failed: open /var/folders/pd/c1tqgl6567vf85kfs9ynxx040000gn/T/TestCodexSettingsApplyFailureIsReported3605297907/002/.codex/config.toml: not a directory\n",
	"codex: open /var/folders/pd/c1tqgl6567vf85kfs9ynxx040000gn/T/TestCodexSettingsApplyFailureIsReported3605297907/002/.codex/config.toml: not a directory\n",
	"codex settings: /var/folders/pd/c1tqgl6567vf85kfs9ynxx040000gn/T/TestCodexSettingsCheckDetectsUnappliedReset3393233540/002/.codex/config.toml\n",
	"model_verbosity  low  native\n",
	"model_verbosity  Codex default  default\n",
	"== keys ==\n",
	"claude     ABSENT: claude-code vault set OPENAI_API_KEY\n",
	"codex      ABSENT: run `codex login` (ChatGPT) or `claude-code configure` → codex → profile\n",
	"copilot    ABSENT: run `claude-code configure github` (Copilot) or set an Anthropic key\n",
	"gemini     ABSENT: run `agy` to sign in with Google\n",
	"opencode   ABSENT: run `opencode auth login` (opencode Go) or `claude-code configure github` (Copilot)\n",
	"  pasted  sk-t********* (13 chars)\n",
	"wrote OPENAI_API_KEY to the vault (sk-t*** (13 chars))\n",
	"codex      present (ChatGPT login (codex login))\n",
	"  pasted  tskey-client-***************************… (49 chars)\n",
	"  pasted  pla******** (11 chars)\n",
	"  pasted  priv********* (13 chars)\n",
	"  pasted  unte************** (18 chars)\n",
	"alpha          first",
	"beta           second",
	"gamma          third",
	"model-x        pick",
	"delta          d",
	"epsilon        e",
	"zeta           z",
	"eta            h",
	"Back           return",
}
