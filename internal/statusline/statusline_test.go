package statusline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
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
	t.Setenv("HARNESS_PROVIDER", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("COLUMNS", "")
	t.Setenv("COLORTERM", "truecolor")
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		t.Setenv(k, "")
	}
	return dir
}

func sample() values {
	return values{userHost: "tester", path: "~/proj", model: "Opus 5 [1m]", provider: "openrouter",
		router: "router:18765", branch: "main", tokens: "~1.2M tok", duration: "12m", contextPct: 42, adds: 10, dels: 3}
}

func TestStyleGoldens(t *testing.T) {
	hermetic(t)
	cases := map[string]string{
		"gradient":  "tester:~/proj | Opus 5 [1m] · openrouter · router:18765 ██████████ 42% · ~1.2M tok · 12m │ main · +10 -3",
		"powerline": "tester:~/proj |  Opus 5 [1m]  openrouter  router:18765  42%  ~1.2M tok  12m   main  +10 -3",
		"capsule":   "tester:~/proj | ( Opus 5 [1m] ) ( openrouter ) ( router:18765 ) ( 42% ) ( ~1.2M tok ) ( 12m ) (  main ) +10 -3",
		"minimal":   "tester:~/proj |  Opus 5 [1m]  ·  openrouter  ·  router:18765  ·  42%  ·  ~1.2M tok  ·  12m  ·   main  +10 -3",
	}
	for style, want := range cases {
		cfg := embeddedConfig()
		cfg.Style = style
		if got := plain(render(sample(), cfg, embeddedPalettes())); got != want {
			t.Errorf("%s:\n got %q\nwant %q", style, got, want)
		}
	}
	cfg := embeddedConfig()
	got := render(sample(), cfg, embeddedPalettes())
	// The ramp opens on the path: "~" carries gemini's first stop, bold truecolor.
	if !strings.Contains(got, "\x1b[01;32mtester\x1b[0m") || !strings.Contains(got, "\x1b[1;38;2;71;150;228m~") {
		t.Errorf("gradient head escapes: %q", got)
	}
	cfg.Style = "powerline"
	cfg.Charset = "nerd"
	if !strings.Contains(render(sample(), cfg, embeddedPalettes()), glyphArrow) {
		t.Error("nerd powerline lacks the arrow glyph")
	}
}

func TestDashboardAlignsHeadersOverValues(t *testing.T) {
	hermetic(t)
	cfg := embeddedConfig()
	cfg.Style = "dashboard"
	lines := strings.Split(plain(render(sample(), cfg, embeddedPalettes())), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header + value rows, got %q", lines)
	}
	hdr, val := []rune(lines[0]), []rune(lines[1])
	if !strings.HasPrefix(lines[1], "tester:~/proj | Opus 5 [1m]") || !strings.HasSuffix(lines[1], "██████████ 42%") {
		t.Errorf("value row %q", lines[1])
	}
	if len(val) != len("tester:~/proj | ")+100 {
		t.Errorf("value row width %d, want head + 100", len(val))
	}
	for _, pair := range [][2]string{{"provider", "openrouter"}, {"router", "router:18765"}, {"context", "42%"}} {
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
		for _, line := range strings.Split(render(sample(), cfg, embeddedPalettes()), "\n") {
			if w := visibleWidth(line); w > 40 {
				t.Errorf("%s: width %d > 40: %q", style, w, plain(line))
			}
			if !strings.HasSuffix(line, "\x1b[0m") {
				t.Errorf("%s: clipped line must end reset: %q", style, line)
			}
		}
	}
	cfg := embeddedConfig()
	t.Setenv("COLUMNS", "30")
	if got := plain(render(sample(), cfg, embeddedPalettes())); got != "tester:~/proj | Opus 5 [1m] ·…" {
		t.Errorf("COLUMNS clip %q", got)
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
	parse := func(s string) Input {
		var in Input
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
		{"transcript over 200K default", `{"transcript_path":"TR","model":{"id":"claude-opus-5"}}`, 25},
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
	if got := contextPct(parse(`{"transcript_path":"TR","model":{"id":"claude-opus-5"}}`)); got != 75 {
		t.Errorf("after growth: %d, want 75", got)
	}
}

func TestProviderAndModelSegments(t *testing.T) {
	dir := hermetic(t)
	if name, routed := sessionProvider(); name != "anthropic" || routed {
		t.Errorf("registry default: %q %v", name, routed)
	}
	if err := os.MkdirAll(filepath.Join(dir, "env.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env.d", "provider.env"), []byte("HARNESS_PROVIDER=ollama\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if name, _ := sessionProvider(); name != "ollama" {
		t.Errorf("pin file: %q", name)
	}
	t.Setenv("HARNESS_PROVIDER", "openai")
	if name, _ := sessionProvider(); name != "openai" {
		t.Errorf("env beats pin file: %q", name)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:18765/p/openrouter")
	if name, routed := sessionProvider(); name != "openrouter" || !routed {
		t.Errorf("routed base: %q %v", name, routed)
	}

	model := func(id, display string) string {
		var in Input
		in.Model.ID, in.Model.DisplayName = id, display
		return modelSegment(in, "anthropic")
	}
	for _, c := range [][3]string{
		{"claude-opus-5[1m]", "Opus 5 (1M context)", "Opus 5 [1m]"},
		{"openai:gpt-5-nano", "openai:gpt-5-nano", "gpt-5-nano ⇄openai"},
		{"openai:gpt-5-nano", "", "gpt-5-nano ⇄openai"},
		{"qwen3:30b", "qwen3:30b", "qwen3:30b"}, // an ollama tag, not a provider prefix
		{"anthropic:claude-haiku-5", "Haiku 5", "Haiku 5"},
		{"claude-sonnet-5", "", "claude-sonnet-5"},
	} {
		if got := model(c[0], c[1]); got != c[2] {
			t.Errorf("model(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestRouterSegment(t *testing.T) {
	dir := hermetic(t)
	if got := routerSegment(false); got != "" {
		t.Errorf("no state, unrouted: %q", got)
	}
	if got := routerSegment(true); got != "router:down" {
		t.Errorf("no state, routed: %q", got)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	u, _ := url.Parse(srv.URL)
	state := filepath.Join(dir, "state", "harness")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "router.json"), []byte(`{"pid":1,"port":`+u.Port()+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "router:" + u.Port()
	if got := routerSegment(false); got != want {
		t.Errorf("healthy: %q, want %q", got, want)
	}
	srv.Close()
	if got := routerSegment(false); got != want || hits.Load() != 1 {
		t.Errorf("within TTL the stamp answers without a probe: %q hits=%d", got, hits.Load())
	}
	t.Setenv("TMPDIR", t.TempDir()) // fresh stamp dir → a live probe of the closed port
	if got := routerSegment(true); got != "router:down" {
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
	var in Input
	in.Cost.LinesAdded, in.Cost.LinesRemoved = 999, 999
	if a, d := codeVelocity(in, dir, embeddedConfig()); a != 4 || d != 2 {
		t.Errorf("git velocity +%d -%d, want +4 -2 (1 edit + 3 untracked, 2 removed)", a, d)
	}
	raw := embeddedConfig()
	raw.VelocityMode = "raw"
	if a, d := codeVelocity(in, dir, raw); a != 999 || d != 999 {
		t.Errorf("raw mode reads the ledger: +%d -%d", a, d)
	}
	notRepo := t.TempDir()
	if b := gitBranch(notRepo); b != "" {
		t.Errorf("non-repo branch %q", b)
	}
	if a, d := codeVelocity(in, notRepo, embeddedConfig()); a != 999 || d != 999 {
		t.Errorf("non-repo falls back to the ledger: +%d -%d", a, d)
	}
	if got := truncBranch("feature/statusline", 8); got != "feature…" {
		t.Errorf("truncBranch %q", got)
	}
}

func TestRunAndConfigOverride(t *testing.T) {
	dir := hermetic(t)
	payload := `{"workspace":{"current_dir":"/nowhere/proj"},"model":{"id":"claude-opus-5","display_name":"Opus 5"},` +
		`"cost":{"total_cost_usd":0.5,"total_duration_ms":3725000},"context_window":{"total_input_tokens":48000,"total_output_tokens":3400},"session_id":"abcdef0123456789"}`
	var out bytes.Buffer
	if code := Run([]string{"--style", "minimal"}, strings.NewReader(payload), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := plain(out.String()); !strings.Contains(got, " Opus 5  ·  anthropic ") || !strings.Contains(got, "~51k tok  ·  1h02m") {
		t.Errorf("minimal run: %q", got)
	}
	if code := Run([]string{"--bogus"}, strings.NewReader(""), &out); code != 2 {
		t.Errorf("bad flag exit %d", code)
	}
	// config.json picks the style and enables the session segment; omitted maps keep the embedded ones.
	if err := os.MkdirAll(filepath.Join(dir, "statusline"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"style":"capsule","segments":{"session":true,"git":false,"velocity":false,"router":false}}`
	if err := os.WriteFile(filepath.Join(dir, "statusline", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "tester:/nowhere/proj | ( Opus 5 ) ( anthropic ) ( 0% ) ( ~51k tok ) ( 1h02m ) ( abcdef01 )"
	if got := plain(Render([]byte(payload), "")); got != want {
		t.Errorf("config override:\n got %q\nwant %q", got, want)
	}
	if got := plain(Render(nil, "gradient")); !strings.HasPrefix(got, "tester:") {
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

func TestApproxTokens(t *testing.T) {
	for n, want := range map[int]string{0: "", 950: "~950 tok", 12_345: "~12k tok", 999_400: "~999k tok", 1_234_567: "~1.2M tok"} {
		if got := approxTokens(n); got != want {
			t.Errorf("approxTokens(%d)=%q want %q", n, got, want)
		}
	}
}
