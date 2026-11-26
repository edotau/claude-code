package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/cli/menu"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	_ "github.com/edotau/claude-code/internal/proxyteam" // registers its formats until the binary links it
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func run(args []string) int { return cmdConfigure(args) }

// TestSpecArgsOfferSectionsThenProfiles: the first bare argument completes from the menu sections plus desktop.
func TestSpecArgsOfferSectionsThenProfiles(t *testing.T) {
	want := append(append([]string{}, menu.SectionNames...), "desktop")
	if got := configureSpec().Args[0](); !slices.Equal(got, want) {
		t.Errorf("sections = %v, want %v", got, want)
	}
}

func TestConfigureHelpShowsDoubleDashFlags(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	_, _, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"-h"}) })
	if !strings.Contains(stderr, "--agent") {
		t.Errorf("help lacks --agent:\n%s", stderr)
	}
	if strings.Contains(stderr, "\n  -agent") {
		t.Errorf("help prints single-dash -agent:\n%s", stderr)
	}
}

// resetEnv isolates HOME and CLAUDE_CONFIG_DIR so --restore renders only into a scratch dir.
func resetEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", t.TempDir())
	return dir
}

func TestConfigureRestoreRejectsBadCombos(t *testing.T) {
	resetEnv(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"restore with page", []string{"restore", "provider"}, "unexpected argument"},
		{"revert with page", []string{"revert", "agents"}, "unexpected argument"},
		{"revert with check", []string{"revert", "--check"}, "revert takes only"},
		{"revert with out", []string{"revert", "--out", "x.json"}, "revert takes only"},
		{"out without restore", []string{"--out", "x.json"}, "needs restore or fmt"},
		{"restore with check", []string{"restore", "--check"}, "takes no --check or --dry-run"},
		{"restore with dry-run", []string{"restore", "--dry-run"}, "takes no --check or --dry-run"},
		{"out with agents", []string{"restore", "--agent", "all", "--out", "x.json"}, "never writes one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(tc.args) })
			if code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q lacks %q", stderr, tc.want)
			}
		})
	}
}

// TestConfigureRestoreResetsSettingsAfterBackup: bare restore copies settings.json aside, then writes the render.
func TestConfigureRestoreResetsSettingsAfterBackup(t *testing.T) {
	live := filepath.Join(resetEnv(t), "settings.json")
	if err := os.WriteFile(live, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"restore"}) })
	if code != 0 {
		t.Fatalf("bare restore exit %d: %s", code, stderr)
	}
	if got, _ := os.ReadFile(live); strings.Contains(string(got), "stale") || !strings.Contains(string(got), "--safety-scan") {
		t.Fatalf("settings.json is not the render:\n%s", got)
	}
	_, after, ok := strings.Cut(stdout, "(backup ")
	if !ok {
		t.Fatalf("no backup reported:\n%s", stdout)
	}
	bk, _, _ := strings.Cut(after, ")")
	if old, err := os.ReadFile(bk); err != nil || string(old) != `{"stale":true}` || !strings.HasPrefix(bk, live+".bak.") {
		t.Errorf("backup %q missing or wrong (err %v): %s", bk, err, old)
	}
}

// TestConfigureRestoreAgentsNeverTouchesSettings: --agent <name|all> renders only the agent configs.
func TestConfigureRestoreAgentsNeverTouchesSettings(t *testing.T) {
	dir := resetEnv(t)
	live := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(live, []byte(`{"model":"mine"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"restore", "--agent", "all"}, {"restore", "--agent", "codex"}} {
		if code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(args) }); code != 0 {
			t.Fatalf("%v exit %d: %s", args, code, stderr)
		}
		baks, _ := filepath.Glob(live + ".bak.*")
		if got, _ := os.ReadFile(live); string(got) != `{"model":"mine"}` || len(baks) != 0 {
			t.Errorf("%v touched settings.json: %s (backups %v)", args, got, baks)
		}
	}
}

func TestConfigureRestoreRejectsUnknownAgent(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return run([]string{"restore", "--agent", "x"})
	})
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, `unknown --agent "x"`) {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestConfigureAllFlagGates(t *testing.T) {
	resetEnv(t)
	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
		want     string
	}{
		{"old databricks --all is gone", []string{"databricks", "--all"}, 1, "--all needs the provider page"},
		{"agents --all", []string{"agents", "--all"}, 1, "--all needs the provider page"},
		{"unknown profile", []string{"provider", "--all", "no-such-row"}, 1, "unknown profile no-such-row"},
		{"--family removed", []string{"provider", "--all", "--family", "x"}, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(tc.args) })
			if code != tc.wantCode || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d (want %d), stderr %q lacks %q", code, tc.wantCode, stderr, tc.want)
			}
		})
	}
}

func TestConfigureAllArgumentLimits(t *testing.T) {
	resetEnv(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"two profiles", []string{"provider", "--all", "a", "b"}, "at most one <profile> (got 2)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(tc.args) })
			if code != 1 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q lacks %q", code, stderr, tc.want)
			}
		})
	}
}

// TestConfigureRemovedFormsExitTwoWithHint: a retired flag exits 2 and names its replacement.
func TestConfigureRemovedFormsExitTwoWithHint(t *testing.T) {
	resetEnv(t)
	for flagName, use := range removedForms {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{flagName}) })
		if code != 2 || !strings.Contains(stderr, "`claude-code "+use+"`") {
			t.Errorf("%s: exit %d, stderr %q lacks %q", flagName, code, stderr, use)
		}
	}
}

var update = flag.Bool("update", false, "regenerate the schemas registry golden")

const (
	goldenPath = "../schemas/testdata/registry.golden"
	legacyTag  = "# legacy-layout: "
	// outside marks a Glob that is fixed at package init, so it cannot follow CLAUDE_CONFIG_DIR in a test.
	outside = "<fixed at init>"
)

// decodable reports whether the row has a JSON shape the fixtures describe.
func decodable(f schemas.Format) bool {
	return f.New != nil && f.Kind != schemas.Store && f.Kind != schemas.Derived
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(goldenPath), name))
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	return b
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("bad json %q: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("bad json %q: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

func TestContractRoundTrip(t *testing.T) {
	for _, f := range schemas.All() {
		if !decodable(f) {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			raw := readFixture(t, f.Name+".json")
			dst := f.New()
			from, err := schemas.Decode(f, raw, dst)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if from != f.Version {
				t.Fatalf("fixture is schema %d, registry says %d", from, f.Version)
			}
			out, err := json.Marshal(dst)
			if err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(t, raw, out) {
				t.Fatalf("round trip differs:\nfixture %s\nremarshal %s", bytes.TrimSpace(raw), out)
			}
			var stamp struct {
				Schema *int `json:"schema"`
			}
			if err := json.Unmarshal(out, &stamp); err != nil || stamp.Schema == nil || *stamp.Schema != f.Version {
				t.Fatalf(`re-marshalled value lacks "schema": %d (%s)`, f.Version, out)
			}
		})
	}
}

func TestContractMigrateFromV0(t *testing.T) {
	for _, f := range schemas.All() {
		if !decodable(f) || f.Migrate == nil {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			raw := readFixture(t, f.Name+".v0.json")
			dst := f.New()
			from, err := schemas.Decode(f, raw, dst)
			if err != nil {
				t.Fatalf("decode v0: %v", err)
			}
			if from != 0 {
				t.Fatalf("v0 fixture reports schema %d", from)
			}
			out, err := json.Marshal(dst)
			if err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(t, readFixture(t, f.Name+".json"), out) {
				t.Fatalf("v0 migrated to %s, want the current fixture", out)
			}
		})
	}
}

func relGlob(dir string, f schemas.Format) string {
	if f.Glob == nil {
		return "-"
	}
	g := f.Glob()
	rel, err := filepath.Rel(dir, g)
	if err != nil || strings.HasPrefix(rel, "..") {
		return outside
	}
	return filepath.ToSlash(rel)
}

func ownerBase(f schemas.Format) string { return filepath.Base(f.Owner) }

func TestContractLayout(t *testing.T) {
	dir := cfgDir(t)
	legacy := goldenLegacy(t)
	for _, f := range schemas.All() {
		if f.Glob == nil {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			// Desktop owns its external data root; none of these files may be swept by harness clean.
			switch f.Name {
			case "desktop-profile", "desktop-meta", "desktop-state":
				if f.Owner != "internal/desktop" || f.Clean != "" {
					t.Fatal("Desktop formats must remain externally owned and kept")
				}
				return
			case "rtk-config": // rtk owns its config dir; the harness renders the file but never sweeps it
				if f.Owner != "internal/setup" || f.Kind != schemas.Derived || f.Clean != "" {
					t.Fatal("rtk-config must stay a derived, externally placed file the harness never cleans")
				}
				return
			}
			rel := relGlob(dir, f)
			if rel == outside {
				t.Fatalf("glob %s is outside %s", f.Glob(), dir)
			}
			if !strings.HasPrefix(rel, "state/") {
				t.Fatalf("glob %q is not under state/", rel)
			}
			if legacy[f.Name] {
				return
			}
			want := "state/harness/" + ownerBase(f) + "/"
			if !strings.HasPrefix(rel, want) {
				t.Fatalf("new row %s glob %q must sit under %s (or be added to the golden's legacy-layout list on purpose)",
					f.Name, rel, want)
			}
		})
	}
}

func TestContractCleanOrKeep(t *testing.T) {
	for _, f := range schemas.All() {
		if f.Kind == schemas.Wire {
			continue
		}
		if f.Clean == "" && f.Keep == "" {
			t.Errorf("%s: neither Clean nor Keep", f.Name)
		}
	}
}

func goldenLegacy(t *testing.T) map[string]bool {
	t.Helper()
	m := map[string]bool{}
	b, err := os.ReadFile(goldenPath)
	if err != nil {
		return m
	}
	for l := range strings.SplitSeq(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if n, ok := strings.CutPrefix(l, legacyTag); ok {
			m[strings.TrimSpace(n)] = true
		}
	}
	return m
}

func render(t *testing.T, legacy map[string]bool) string {
	dir := cfgDir(t)
	var sb strings.Builder
	sb.WriteString("# Reviewable registry: name | kind | version | glob (relative to the config dir) | owner | clean:<target>|keep\n")
	sb.WriteString("# A change here is a change to the on-disk contract; regenerate with: go test ./internal/cli -run Golden -update\n")
	var names []string
	rows := map[string]string{}
	for _, f := range schemas.All() {
		disp := "keep"
		if f.Clean != "" {
			disp = "clean:" + f.Clean
		}
		rows[f.Name] = strings.Join([]string{f.Name, string(f.Kind), itoa(f.Version), relGlob(dir, f), f.Owner, disp}, " | ")
		names = append(names, f.Name)
		if f.Glob != nil && !legacy[f.Name] && !strings.HasPrefix(relGlob(dir, f), "state/harness/"+ownerBase(f)+"/") {
			legacy[f.Name] = true
		}
	}
	var keep []string
	for n := range legacy {
		if _, ok := rows[n]; ok {
			keep = append(keep, n)
		}
	}
	sort.Strings(keep)
	for _, n := range keep {
		fmt.Fprintf(&sb, "%s%s\n", legacyTag, n)
	}
	for _, n := range names {
		fmt.Fprintf(&sb, "%s\n", rows[n])
	}
	return sb.String()
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestContractRegistryGolden(t *testing.T) {
	legacy := goldenLegacy(t)
	if *update { // first -update seeds the legacy list with every row that is off the new layout
		got := render(t, legacy)
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("golden missing; run with -update: %v", err)
	}
	if got := render(t, legacy); got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Fatalf("registry differs from %s (run -update after reviewing):\n--- got\n%s--- want\n%s", goldenPath, got, want)
	}
}

// TestConfigureAgentsNamedOnlyActsOnThatAgent: `configure agents codex` reports codex alone, not every default agent.
func TestConfigureAgentsNamedOnlyActsOnThatAgent(t *testing.T) {
	resetEnv(t)
	_, stdout, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"agents", "codex", "--check"}) })
	out := stdout + stderr
	if !strings.Contains(out, "codex") || strings.Contains(out, "opencode") || strings.Contains(out, "gemini") {
		t.Errorf("want codex only:\n%s", out)
	}
	if code, _, _ := hosttest.CaptureOutput(t, func() int { return run([]string{"agents", "codex", "gemini"}) }); code != 1 {
		t.Errorf("two names: exit %d, want 1", code)
	}
}

func fmtExit(t *testing.T, args ...string) (int, string) {
	t.Helper()
	code, stdout, _ := hosttest.CaptureOutput(t, func() int { return run(args) })
	return code, stdout
}

// TestConfigureFmtReordersLiveOnlyAndKeepsStrictCheck: --fmt fixes the live order; bad files and --check still fail.
func TestConfigureFmtReordersLiveOnlyAndKeepsStrictCheck(t *testing.T) {
	live := filepath.Join(resetEnv(t), "settings.json")
	const bad = `{"model":"m","fallbackModel":["sonnet","opus"],"availableModels":["z","a"],"env":{"B":"1","A":"2"},"$schema":"https://json.schemastore.org/claude-code-settings.json"}`
	if err := os.WriteFile(live, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := fmtExit(t, "fmt", "--check"); code != 1 {
		t.Fatalf("fmt --check on out-of-order file exit %d, want 1", code)
	}
	code, stdout := fmtExit(t, "fmt")
	_, bk, _ := strings.Cut(strings.TrimSpace(stdout), "backup at ")
	if old, err := os.ReadFile(bk); code != 0 || err != nil || string(old) != bad {
		t.Fatalf("--fmt exit %d, backup %q (err %v):\n%s", code, bk, err, stdout)
	}
	if code, _ := fmtExit(t, "fmt", "--check"); code != 0 {
		t.Errorf("still out of order after fmt, exit %d", code)
	}
	if got, _ := os.ReadFile(live); strings.Index(string(got), `"sonnet"`) > strings.Index(string(got), `"opus"`) || strings.Index(string(got), `"z"`) > strings.Index(string(got), `"a"`) {
		t.Errorf("fallbackModel priority order changed:\n%s", got)
	}
}

// TestConfigureFmtLeavesUnparsableFileUntouched: a file that does not parse fails --fmt and stays byte-identical.
func TestConfigureFmtLeavesUnparsableFileUntouched(t *testing.T) {
	live := filepath.Join(resetEnv(t), "settings.json")
	if err := os.WriteFile(live, []byte(`{"model":`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _ := fmtExit(t, "fmt")
	got, _ := os.ReadFile(live)
	if code == 0 || string(got) != `{"model":` {
		t.Errorf("unparsable file: exit %d, content %q", code, got)
	}
}

// spendCapText is the routing spend caps as "session day", nil shown as "nil".
func spendCapText(t *testing.T) string {
	t.Helper()
	r, err := providers.RoutingOverlay()
	if err != nil {
		t.Fatal(err)
	}
	show := func(p *float64) string {
		if p == nil {
			return "nil"
		}
		return fmt.Sprint(*p)
	}
	return show(r.SpendCap.SessionUSD) + " " + show(r.SpendCap.DayUSD)
}

// routingOverlayRaw is the stored providers overlay, the bytes a routing write would change.
func routingOverlayRaw(t *testing.T) string {
	t.Helper()
	var raw string
	if err := vault.View(func(tx *vault.Tx) error { raw, _ = tx.Get(vault.Overlay, "providers"); return nil }); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestConfigureRoutingAssign(t *testing.T) {
	vaulttest.Use(t)
	for _, tc := range []struct {
		args  []string
		code  int
		want  string
		write bool // a successful apply, which may change the vault
	}{
		{[]string{"routing", "spendCap.dayUSD=50", "--dry-run"}, 0, "routing spendCap.dayUSD=50.00", false},
		{[]string{"routing", "spendCap.sessionUSD=5", "--check"}, 0, "routing spendCap.sessionUSD=5.00", false},
		{[]string{"routing", "nope=1"}, 2, "unknown routing key", false},
		{[]string{"routing", "spendCap.sessionUSD=5", "bad=1"}, 2, "unknown routing key", false},
		{[]string{"routing", "spendCap.sessionUSD=NaN"}, 2, "finite USD", false},
		{[]string{"routing", "spendCap.sessionUSD"}, 2, "must be key=value", false},
		{[]string{"routing", "spendCap.sessionUSD=5", "spendCap.dayUSD=20", "servedAware=true"}, 0, "", true},
		{[]string{"routing", "spendCap.sessionUSD=", "spendCap.sessionUSD=7"}, 0, "", true},
	} {
		before := routingOverlayRaw(t)
		code, out, stderr := hosttest.CaptureOutput(t, func() int { return run(tc.args) })
		if code != tc.code || !strings.Contains(out+stderr, tc.want) || !tc.write && routingOverlayRaw(t) != before {
			t.Fatalf("%v: code %d out %s err %s", tc.args, code, out, stderr)
		}
	}
}

func TestConfigureRoutingReset(t *testing.T) {
	settings := filepath.Join(vaulttest.Use(t), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"untouched":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"routing", "spendCap.sessionUSD=5", "spendCap.dayUSD=20", "servedAware=true"},
		{"routing", "spendCap.sessionUSD=", "spendCap.sessionUSD=7"},
	} {
		code, _, errout := hosttest.CaptureOutput(t, func() int { return run(args) })
		if code != 0 {
			t.Fatalf("%v: %s", args, errout)
		}
	}
	if got := spendCapText(t); got != "7 20" {
		t.Fatalf("routing spend caps = %q", got)
	}
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"routing", "spendCap.sessionUSD="}) })
	if code != 0 || !strings.Contains(out, "=off") {
		t.Fatalf("reset: %d %s %s", code, out, stderr)
	}
	if got := spendCapText(t); got != "nil 20" {
		t.Fatalf("reset spend caps = %q", got)
	}
	if got, err := os.ReadFile(settings); err != nil || string(got) != `{"untouched":true}` {
		t.Fatalf("settings changed: %s %v", got, err)
	}
}

func TestConfigureRoutingWeights(t *testing.T) {
	vaulttest.Use(t)
	for _, pairs := range [][]string{
		{"weights.price=0.1", "weights.latency=0.8"},
		{"weights.price=0.5", "weights.latency=0.2"},
		{"weights.latency=", "weights.price=0.3"},
	} {
		args := append([]string{"routing"}, pairs...)
		code, preview, stderr := hosttest.CaptureOutput(t, func() int { return run(append(slices.Clone(args), "--dry-run")) })
		if code != 0 {
			t.Fatalf("preview %v: %s", args, stderr)
		}
		code, out, stderr := hosttest.CaptureOutput(t, func() int { return run(args) })
		if code != 0 || out != preview {
			t.Fatalf("write %v: code %d out %s preview %s err %s", args, code, out, preview, stderr)
		}
		r, err := providers.RoutingOverlay()
		if err != nil {
			t.Fatal(err)
		}
		want := strings.TrimPrefix(pairs[len(pairs)-1], "weights.price=")
		if v, ok := strings.CutPrefix(pairs[0], "weights.price="); ok {
			want = v
		}
		if got := fmt.Sprint(r.Weights["price"]); got != want {
			t.Fatalf("price=%s want=%s", got, want)
		}
	}

	for _, args := range [][]string{
		{"routing", "weights.price=0.99", "weights.latency=0.2", "--dry-run"},
		{"routing", "weights.price=0.99", "weights.latency=0.2"},
		{"routing", "spendCap.dayUSD=5", "--list"},
		{"routing", "spendCap.dayUSD=5", "--out", "unused.json"},
	} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(args) })
		if code != 2 {
			t.Fatalf("%v: exit %d %s", args, code, stderr)
		}
	}
}
