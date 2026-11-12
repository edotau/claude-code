package statusline

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tokenledger"
	"github.com/edotau/claude-code/internal/ui/theme"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return sgr.ReplaceAllString(s, "") }

// hermetic isolates config dir, stamps, pins and terminal env from the live ~/.claude.
func hermetic(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("USER", "tester")
	t.Setenv(providers.EnvSessionProfile, "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("COLUMNS", "")
	origWidth := termWidth
	termWidth = func() int { return 0 }
	t.Cleanup(func() { termWidth = origWidth })
	t.Setenv("COLORTERM", "truecolor")
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		t.Setenv(k, "")
	}
	return dir
}

// registryDefault is the built-in default provider, so the tests track defaults.json instead of a literal.
func registryDefault(t *testing.T) string {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg.Default
}

func sample() values {
	return values{userHost: "tester", path: "~/proj", model: "Opus 5 [1m]", provider: "openrouter",
		proxy: "proxy:18765", branch: "main", total: "1.23M", duration: "12m", contextPct: 42, adds: 10, dels: 3}
}

// assertInOrder fails unless every part appears in got, each after the one before it.
func assertInOrder(t *testing.T, got string, parts ...string) {
	t.Helper()
	at := 0
	for _, p := range parts {
		i := strings.Index(got[at:], p)
		if i < 0 {
			t.Errorf("%q missing or out of order in %q", p, got)
			return
		}
		at += i + len(p)
	}
}

// Segmented styles keep head, cfg.Order, git, velocity order yet differ; the head is painted; only nerd draws arrows.
func TestSegmentedStyles(t *testing.T) {
	hermetic(t)
	parts := []string{"tester:~/proj", "openrouter", "Opus 5 [1m]", "proxy:18765", "42%", "1.23M", "12m", "main", "+10 -3"}
	seen := map[string]string{}
	for _, style := range []string{"powerline", "capsule", "minimal"} {
		cfg := embeddedConfig()
		cfg.Style, cfg.Order = style, []string{"provider", "model", "proxy", "context", "total", "duration"}
		cfg.Segments = map[string]bool{"session": false} // every other segment on
		got := plain(render(sample(), cfg, theme.LoadPalettes()))
		assertInOrder(t, got, parts...)
		if prev, dup := seen[got]; dup {
			t.Errorf("%s renders exactly like %s: %q", style, prev, got)
		}
		seen[got] = style
	}
	raw := render(sample(), embeddedConfig(), theme.LoadPalettes())
	if !regexp.MustCompile(`\x1b\[[0-9;]*mtester`).MatchString(raw) || !regexp.MustCompile(`\x1b\[[0-9;]*m~`).MatchString(raw) {
		t.Errorf("gradient head is not painted: %q", raw)
	}
	cfg := embeddedConfig()
	for charset, want := range map[string]bool{"nerd": true, "text": false} {
		cfg.Style, cfg.Charset = "powerline", charset
		if got := strings.Contains(render(sample(), cfg, theme.LoadPalettes()), glyphArrow); got != want {
			t.Errorf("%s powerline draws the arrow glyph = %v, want %v", charset, got, want)
		}
	}
}

func TestDashboardAlignsHeadersOverValues(t *testing.T) {
	hermetic(t)
	cfg := embeddedConfig()
	cfg.Style = "dashboard"
	cfg.Segments = map[string]bool{"session": false} // pin columns so alignment is checked regardless of defaults
	cfg.Dashboard.Columns = []string{"workspace", "model", "provider", "proxy", "total", "branch", "context"}
	cfg.Dashboard.Width, cfg.Bar.Percent = 90, "after" // the row right-aligns to the configured width
	lines := strings.Split(plain(render(sample(), cfg, theme.LoadPalettes())), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header + value rows, got %q", lines)
	}
	hdr, val := []rune(lines[0]), []rune(lines[1])
	if !strings.HasPrefix(lines[1], "tester:~/proj | Opus 5 [1m]") || !strings.HasSuffix(lines[1], " 42%") {
		t.Errorf("value row %q", lines[1])
	}
	if len(val) != len("tester:~/proj | ")+cfg.Dashboard.Width {
		t.Errorf("value row width %d, want head + %d", len(val), cfg.Dashboard.Width)
	}
	for _, pair := range [][2]string{{"provider", "openrouter"}, {"proxy", "proxy:18765"}, {"context", "42%"}} {
		h := strings.Index(string(hdr), pair[0])
		v := strings.Index(string(val), pair[1])
		if pair[0] == "context" { // right-aligned: both columns end at the same cell
			h, v = utf8.RuneCountInString(lines[0]), utf8.RuneCountInString(lines[1])
		} else {
			h, v = utf8.RuneCountInString(lines[0][:h]), utf8.RuneCountInString(lines[1][:v])
		}
		if h != v {
			t.Errorf("%s header at %d, value at %d", pair[0], h, v)
		}
	}
}

func TestWidthClip(t *testing.T) {
	hermetic(t)
	for _, style := range []string{"gradient", "powerline", "dashboard"} {
		cfg := embeddedConfig()
		cfg.Style, cfg.Width = style, 40
		for line := range strings.SplitSeq(render(sample(), cfg, theme.LoadPalettes()), "\n") {
			if w := visibleWidth(line); w > 40 {
				t.Errorf("%s: width %d > 40: %q", style, w, plain(line))
			}
			if !strings.HasSuffix(line, "\x1b[0m") {
				t.Errorf("%s: clipped line must end reset: %q", style, line)
			}
		}
	}
	for width := 16; width <= 40; width++ { // no configured width: $COLUMNS less the CLI's margin is the width
		t.Setenv("COLUMNS", strconv.Itoa(width+ccMargin))
		cfg := embeddedConfig()
		got := render(sample(), cfg, theme.LoadPalettes())
		cfg.Width = width
		if want := render(sample(), cfg, theme.LoadPalettes()); got != want {
			t.Errorf("COLUMNS %d renders %q, want the width-%d line %q", width+ccMargin, plain(got), width, plain(want))
		}
	}
	if s := "short"; clip(s, 40) != s {
		t.Error("a line within width must be untouched")
	}
}

func TestContextPct(t *testing.T) {
	hermetic(t)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeUsage := func(tokens int) {
		line := fmt.Sprintf(`{"type":"assistant","message":{"model":"claude-opus-5","usage":{"input_tokens":%d,"output_tokens":0}}}`+"\n", tokens)
		if err := os.WriteFile(tr, []byte(`{"type":"user"}`+"\n"+line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeUsage(50_000)
	parse := func(s string) input {
		var in input
		if err := json.Unmarshal([]byte(strings.ReplaceAll(s, "TR", tr)), &in); err != nil {
			t.Fatal(err)
		}
		return in
	}
	cases := []struct {
		name, payload string
		want          int
	}{
		{"stdin used_percentage wins", `{"transcript_path":"TR","context_window":{"used_percentage":71.6}}`, 72},
		{"transcript over reported window", `{"transcript_path":"TR","context_window":{"context_window_size":100000}}`, 50},
		{"transcript over 200K default", `{"transcript_path":"TR","model":{"id":"claude-opus-4-6"}}`, 25},
		{"[1m] id widens the window", `{"transcript_path":"TR","model":{"id":"claude-opus-5[1m]"}}`, 5},
		{"exceeds_200k widens the window", `{"transcript_path":"TR","exceeds_200k_tokens":true}`, 5},
		{"no transcript", `{}`, 0},
	}
	for _, c := range cases {
		if got := contextPct(parse(c.payload)); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	writeUsage(150_000) // the (size, mtime) memo must not serve the old usage
	if got := contextPct(parse(`{"transcript_path":"TR","model":{"id":"claude-opus-4-6"}}`)); got != 75 {
		t.Errorf("after growth: %d, want 75", got)
	}
}

func TestSessionProvider(t *testing.T) {
	hermetic(t)
	if name, routed := sessionProvider(); name != registryDefault(t) || routed {
		t.Errorf("registry default: %q %v", name, routed)
	}
	vaulttest.SetRecord(t, "claude", "profile", "antigravity")
	if name, _ := sessionProvider(); name != "antigravity" {
		t.Errorf("vault pins: %q", name)
	}
	t.Setenv(providers.EnvSessionProfile, "openai")
	if name, _ := sessionProvider(); name != "openai" {
		t.Errorf("env beats vault pins: %q", name)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:18765/p/openrouter")
	if name, routed := sessionProvider(); name != "openrouter" || !routed {
		t.Errorf("routed base: %q %v", name, routed)
	}
}

func TestModelSegment(t *testing.T) {
	hermetic(t)
	model := func(id, display string) string {
		var in input
		in.Model.ID, in.Model.DisplayName = id, display
		return modelSegment(in, "anthropic")
	}
	for _, c := range [][3]string{
		{"claude-opus-5[1m]", "Opus 5 (1M context)", "opus-5"},
		{"openai:gpt-5-nano", "openai:gpt-5-nano", "gpt-5-nano ⇄openai"},
		{"qwen3:30b", "qwen3:30b", "qwen3:30b"}, // an ollama tag, not a provider prefix
		{"anthropic:claude-haiku-5", "Haiku 5", "haiku-5"},
		{"claude-sonnet-5-20260101", "", "sonnet-5"},
		{"claude-opus-5-5[1m]", "Opus 5.5 (1M context)", "opus-5-5"},
		{"", "Opus 5 (1M context)", "Opus 5"},
		{"databricks-claude-opus-5-5[1m]", "databricks-claude-opus-5-5", "databricks-opus-5-5"},
		{"databricks-claude-opus-5-5", "databricks-claude-opus-5-5", "databricks-opus-5-5"},
	} {
		if got := model(c[0], c[1]); got != c[2] {
			t.Errorf("model(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	var in input
	in.Model.ID, in.Model.DisplayName = "custom-model", "Custom"
	in.ContextWindow = &struct {
		Size           int      `json:"context_window_size"`
		TotalInput     int      `json:"total_input_tokens"`
		TotalOutput    int      `json:"total_output_tokens"`
		UsedPercentage *float64 `json:"used_percentage"`
	}{Size: 1_000_000}
	if got := modelSegment(in, "anthropic"); got != "custom-model" {
		t.Errorf("reported 1M window: %q", got)
	}
}

func TestRouterSegment(t *testing.T) {
	dir := hermetic(t)
	if got := proxySegment(false); got != "" {
		t.Errorf("no state, unrouted: %q", got)
	}
	if got := proxySegment(true); got != "proxy:down" {
		t.Errorf("no state, routed: %q", got)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	u, _ := url.Parse(srv.URL)
	state := filepath.Join(dir, "state", "harness")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "proxy.json"), []byte(`{"pid":1,"port":`+u.Port()+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "proxy:" + u.Port()
	if got := proxySegment(false); got != want {
		t.Errorf("healthy: %q, want %q", got, want)
	}
	srv.Close()
	if got := proxySegment(false); got != want || hits.Load() != 1 {
		t.Errorf("within TTL the stamp answers without a probe: %q hits=%d", got, hits.Load())
	}
	_ = os.RemoveAll(filepath.Join(state, "statusline")) // fresh stamp dir → a live probe of the closed port
	if got := proxySegment(true); got != "proxy:down" {
		t.Errorf("dead daemon, routed: %q", got)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "feature/statusline")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-q", "-m", "seed")
	return dir
}

func TestGitSegments(t *testing.T) {
	hermetic(t)
	dir := gitRepo(t)
	if got := gitBranch(filepath.Join(dir)); got != "feature/statusline" {
		t.Errorf("branch %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package x\n\nfunc F() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var in input
	in.Cost.LinesAdded, in.Cost.LinesRemoved = 999, 999
	if a, d := codeVelocity(in, dir, embeddedConfig()); a != 4 || d != 2 {
		t.Errorf("git velocity +%d -%d, want +4 -2 (1 edit + 3 untracked, 2 removed)", a, d)
	}
	raw := embeddedConfig()
	raw.VelocityMode = "raw"
	if a, d := codeVelocity(in, dir, raw); a != 999 || d != 999 {
		t.Errorf("raw mode reads the ledger: +%d -%d", a, d)
	}
}

func TestGitSegmentsOutsideRepo(t *testing.T) {
	hermetic(t)
	var in input
	in.Cost.LinesAdded, in.Cost.LinesRemoved = 999, 999
	notRepo := t.TempDir()
	if b := gitBranch(notRepo); b != "" {
		t.Errorf("non-repo branch %q", b)
	}
	if a, d := codeVelocity(in, notRepo, embeddedConfig()); a != 999 || d != 999 {
		t.Errorf("non-repo falls back to the ledger: +%d -%d", a, d)
	}
}

// capProfile caps only the profile after "·" to max runes, ending in "…" when cut; a bare name is untouched.
func TestCapProfile(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"profileMax":1}`)
	if got := loadConfig().ProfileMax; got != 1 {
		t.Fatalf("config.json profileMax = %d, want 1", got)
	}
	for _, tc := range []struct {
		label string
		max   int
	}{{"anthropic·work", 1}, {"databricks·feature/statusline", 8}} {
		got := capProfile(tc.label, tc.max)
		prov, prof, _ := strings.Cut(got, "·")
		wantProv, wantProf, _ := strings.Cut(tc.label, "·")
		if prov != wantProv || utf8.RuneCountInString(prof) != tc.max || !strings.HasSuffix(prof, "…") ||
			!strings.HasPrefix(wantProf, strings.TrimSuffix(prof, "…")) {
			t.Errorf("capProfile(%q, %d) = %q, want %s· and %d profile runes ending …", tc.label, tc.max, got, wantProv, tc.max)
		}
	}
	for _, label := range []string{"anthropic·ab", "plain-profile-name"} {
		if got := capProfile(label, 8); got != label {
			t.Errorf("capProfile(%q, 8) = %q, want it untouched", label, got)
		}
	}
}

func TestRunAndConfigOverride(t *testing.T) {
	dir := hermetic(t)
	payload := `{"workspace":{"current_dir":"/nowhere/proj"},"model":{"id":"claude-opus-5","display_name":"Opus 5"},` +
		`"cost":{"total_cost_usd":0.5,"total_duration_ms":3725000},"context_window":{"total_input_tokens":48000,"total_output_tokens":3400},"session_id":"abcdef0123456789"}`
	var out bytes.Buffer
	if code := Run(flag.NewFlagSet("t", flag.ContinueOnError), []string{"--style", "minimal"}, strings.NewReader(payload), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := plain(out.String())
	assertInOrder(t, got, "anthropic·ultra", "opus-5", "0%")
	if strings.Contains(got, "tok") {
		t.Errorf("minimal run: %q", got)
	}
	if code := Run(flag.NewFlagSet("t", flag.ContinueOnError), []string{"--bogus"}, strings.NewReader(""), &out); code != 2 {
		t.Errorf("bad flag exit %d", code)
	}
	// config.json picks the style and enables the session segment; omitted maps keep the embedded ones.
	if err := os.MkdirAll(filepath.Join(dir, "state", "statusline"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"style":"capsule","segments":{"session":true,"duration":true,"git":false,"velocity":false,"proxy":false}}`
	if err := os.WriteFile(filepath.Join(dir, "state", "statusline", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "tester:/nowhere/proj | ( anthropic·ultra ) ( opus-5 ) ( 0% ) ( 51.4k ) ( 1h02m ) ( abcdef01 )"
	if got := plain(renderStatusline([]byte(payload), "")); got != want {
		t.Errorf("config override:\n got %q\nwant %q", got, want)
	}
	if got := plain(renderStatusline(nil, "gradient")); !strings.HasPrefix(got, "tester:") {
		t.Errorf("blank payload must still render: %q", got)
	}
}

func TestFormatDuration(t *testing.T) {
	for ms, want := range map[int64]string{0: "", 42_000: "42s", 754_000: "12m", 3_725_000: "1h02m"} {
		if got := formatDuration(ms); got != want {
			t.Errorf("formatDuration(%d) = %q, want %q", ms, got, want)
		}
	}
}

func TestSigTokens(t *testing.T) {
	for n, want := range map[int]string{5: "5", 812: "812", 1_234: "1.23k", 45_600: "45.6k", 812_000: "812k",
		999_600: "1.00M", 1_234_567: "1.23M", 12_345_678: "12.3M", 123_456_789: "123M"} {
		if got := sigTokens(n); got != want {
			t.Errorf("sigTokens(%d)=%q want %q", n, got, want)
		}
	}
}

func TestTotalSegment(t *testing.T) {
	hermetic(t)
	var in input
	if err := json.Unmarshal([]byte(`{"session_id":"s1","context_window":{"total_input_tokens":41200,"total_output_tokens":4800}}`), &in); err != nil {
		t.Fatal(err)
	}
	if got := totalSegment(in); got != "46.0k" {
		t.Errorf("payload only = %q, want 46.0k", got)
	}
	tokenledger.Append(tokenledger.Entry{Session: "s1", Input: 1000, Output: 500})
	tokenledger.Append(tokenledger.Entry{Session: "other", Input: 9_000_000})
	if got := totalSegment(in); got != "47.5k" {
		t.Errorf("with ledger = %q, want 47.5k", got)
	}
	if got := totalSegment(input{SessionID: "none"}); got != "" {
		t.Errorf("zero total = %q, want empty", got)
	}
}

// labelRegistry is the built-in rows plus a databricks qa preset row, swapped in for the registry seam.
func labelRegistry(t *testing.T) {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	reg.Providers["qa"] = &providers.Provider{Name: "qa", Provider: "databricks", Preset: providers.PresetDatabricks}
	reg.Providers["anthropic-work"] = &providers.Provider{Name: "anthropic-work", Provider: "anthropic",
		BaseURL: "https://api.anthropic.com", Protocol: providers.ProtocolAnthropic}
	reg.Providers["acme"] = &providers.Provider{Name: "acme", Provider: "anthropic",
		BaseURL: "https://gw.anthropic.com", Protocol: providers.ProtocolAnthropic}
	prev := registry
	t.Cleanup(func() { registry = prev })
	registry = func() *providers.Registry { return reg }
}

func TestProviderLabelFamilyTable(t *testing.T) {
	hermetic(t)
	labelRegistry(t)
	for name, want := range map[string]string{
		"ultra":          "anthropic·ultra",
		"anthropic":      "anthropic", // only the profile named for its provider collapses to bare "anthropic" (L10)
		"anthropic-work": "anthropic·anthropic-work",
		"acme":           "anthropic·acme",
		"qa":             "databricks·qa",
		"chatgpt":        "openai·chatgpt",
		"personal":       "github·personal",
		"antigravity":    "google·antigravity",
		"openai":         "openai",
		"corp-prod":      "corp-prod", // unknown row: unchanged
		"mystery":        "mystery",   // unknown row, no env suffix
	} {
		if got := providerLabel(name); got != want {
			t.Errorf("providerLabel(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestModelFamily(t *testing.T) {
	for id, want := range map[string]string{
		"databricks-claude-opus-5-5":     "databricks",
		"databricks-claude-opus-5-5[1m]": "databricks",
		"claude-opus-5":                  "anthropic",
		"claude-opus-5[1m]":              "anthropic",
		"qwen3:30b":                      "",
		"":                               "",
	} {
		if got := modelFamily(id); got != want {
			t.Errorf("modelFamily(%q) = %q, want %q", id, got, want)
		}
	}
}

func renderPayload(t *testing.T, model string) string {
	t.Helper()
	payload := `{"workspace":{"current_dir":"/nowhere/proj"},"model":{"id":"` + model + `","display_name":"Opus 5"}}`
	return plain(renderStatusline([]byte(payload), "minimal"))
}

// Regression: another session's `switch` rewrites the vault pin; this session's label follows its own env.
func TestLabelTracksSessionEnvOverVaultPin(t *testing.T) {
	hermetic(t)
	labelRegistry(t)
	vaulttest.SetRecord(t, "claude", "profile", "antigravity")
	t.Setenv(providers.EnvSessionProfile, "qa")
	if got := renderPayload(t, "claude-opus-5"); !strings.Contains(got, "databricks·qa") || strings.Contains(got, "antigravity") {
		t.Errorf("env session label: %q", got)
	}
	vaulttest.SetRecord(t, "claude", "profile", "openai") // the other session switches again
	if got := renderPayload(t, "claude-opus-5"); !strings.Contains(got, "databricks·qa") || strings.Contains(got, "openai") {
		t.Errorf("label moved with the pin: %q", got)
	}
}

func TestLabelProviderBeforeModel(t *testing.T) {
	hermetic(t)
	labelRegistry(t)
	t.Setenv(providers.EnvSessionProfile, "qa")
	got := renderPayload(t, "claude-opus-5")
	p, m := strings.Index(got, "databricks·qa"), strings.Index(got, "opus-5")
	if p < 0 || m < 0 || p > m {
		t.Errorf("provider must precede model: %q", got)
	}
}

// Width known: full, then profileMax; no line exceeds the width (minimal: the gradient line shows no profile).
func TestProfileFitsWidth(t *testing.T) {
	hermetic(t)
	const full = "databricks·my-long-workspace-profile"
	pal := theme.LoadPalettes()
	v := values{userHost: "u@h", path: "~/p", providerFull: full, adds: 12, dels: 3}
	cfg := embeddedConfig()
	cfg.Style, cfg.Order, cfg.ProfileMax = "minimal", []string{"provider"}, 16
	v.provider = capProfile(full, cfg.ProfileMax)
	if !strings.Contains(plain(render(v, cfg, pal)), "databricks·my-long-workspa…") {
		t.Fatalf("profileMax cap missing: %q", plain(render(v, cfg, pal)))
	}
	orig := termWidth
	t.Cleanup(func() { termWidth = orig })
	for _, tc := range []struct {
		width int
		want  string
	}{{200, full}, {50, "databricks·my-long-workspa…"}, {20, "…"}} {
		termWidth = func() int { return tc.width + ccMargin } // the terminal; the line gets ccMargin less
		_ = os.RemoveAll(filepath.Dir(stampFile("termwidth", "x")))
		got := plain(renderFit(v, cfg, pal, lineWidth(cfg)))
		if !strings.Contains(got, tc.want) || longestLine(got) > tc.width {
			t.Errorf("width %d: got %q, want %q within %d cells", tc.width, got, tc.want, tc.width)
		}
	}
}

func TestGradientPreservesOllamaTag(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"segments":{"path":false,"git":false,"velocity":false,"proxy":false,"provider":false,"limits":false,"total":false}}`)
	got := plain(renderStatusline([]byte(`{"model":{"id":"qwen3:30b"}}`), "gradient"))
	if got != "qwen3:30b" {
		t.Fatalf("gradient model = %q, want qwen3:30b", got)
	}
}

func TestDashboardOmitsDisabledContext(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"style":"dashboard","segments":{"context":false,"path":false,"git":false,"velocity":false,"proxy":false,"limits":false,"total":false}}`)
	got := plain(renderStatusline([]byte(`{"model":{"id":"qwen3:30b"},"context_window":{"used_percentage":75}}`), ""))
	if strings.Contains(got, "context") || strings.Contains(got, "%") {
		t.Fatalf("disabled context column rendered: %q", got)
	}
	if !strings.Contains(got, "qwen3:30b") {
		t.Fatalf("enabled model column missing: %q", got)
	}
}

type fullDiskWriter struct{}

func (fullDiskWriter) Write([]byte) (int, error) { return 0, syscall.ENOSPC }

func TestRunReportsWriteFailure(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"segments":{"path":false,"git":false,"velocity":false,"proxy":false,"limits":false,"total":false}}`)
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	if code := Run(fs, nil, strings.NewReader(`{"model":{"id":"qwen3:30b"}}`), fullDiskWriter{}); code != 1 {
		t.Fatalf("Run with ENOSPC = %d, want 1", code)
	}
}

func TestConfiguredGitRendersOnce(t *testing.T) {
	hermetic(t)
	cfg := embeddedConfig()
	cfg.Style, cfg.Order = "minimal", []string{"model", "git"}
	cfg.Segments = map[string]bool{"path": false}
	got := plain(render(values{model: "qwen3:30b", branch: "feature-once"}, cfg, theme.LoadPalettes()))
	if count := strings.Count(got, "feature-once"); count != 1 {
		t.Fatalf("branch rendered %d times, want 1: %q", count, got)
	}
}

func TestDashboardPercentBeforeBar(t *testing.T) {
	hermetic(t)
	writeStatuslineConfig(t, `{"style":"dashboard","segments":{"path":false},"dashboard":{"headers":false},"bar":{"width":4,"filledGlyph":"#","emptyGlyph":".","percent":"before"}}`)
	cfg := loadConfig()
	cfg.Dashboard.Columns = []string{"context"}
	got := strings.TrimSpace(plain(render(values{contextPct: 75}, cfg, theme.LoadPalettes())))
	if got != "75% ###." {
		t.Fatalf("dashboard percent before bar = %q, want 75%% ###.", got)
	}
}

func TestGradientClipsOversizedSegment(t *testing.T) {
	hermetic(t)
	cfg := embeddedConfig()
	cfg.Style, cfg.Width, cfg.Order = "gradient", 12, []string{"model"}
	cfg.Segments = map[string]bool{"path": false}
	got := plain(renderFit(values{model: strings.Repeat("m", 30)}, cfg, theme.LoadPalettes(), cfg.Width))
	if got != strings.Repeat("m", 11)+"…" {
		t.Fatalf("gradient width 12 = %q, want 11 model cells and ellipsis", got)
	}
}

func TestRenderFitWrapsWideSegments(t *testing.T) {
	hermetic(t)
	for _, tc := range []struct {
		name  string
		v     values
		width int
		want  string
	}{
		{"path", values{userHost: "u", path: "界界", model: "tail"}, 11, "u:界界\ntail"},
		{"hostname", values{userHost: "界界", path: "p", model: "tail"}, 11, "界界:p\ntail"},
		{"next segment", values{userHost: "u", path: "p", model: "界界"}, 9, "u:p\n界界"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Style, cfg.Width, cfg.Order = "gradient", tc.width, []string{"model"}
			got := plain(renderFit(tc.v, cfg, theme.LoadPalettes(), tc.width))
			if got != tc.want {
				t.Fatalf("wide segment wrapping = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
