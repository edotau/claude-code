package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/ui/menu"
)

func run(args []string) int { return cmdConfigure(args) }

// TestSpecArgsOfferSectionsThenProfiles: the first bare argument completes from the menu sections.
func TestSpecArgsOfferSectionsThenProfiles(t *testing.T) {
	want := append([]string{}, menu.SectionNames...)
	if got := configureSpec().Args[0](); !slices.Equal(got, want) {
		t.Errorf("sections = %v, want %v", got, want)
	}
}

func TestConfigureHelpShowsDoubleDashFlags(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	_, _, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"-h"}) })
	if !strings.Contains(stderr, "--restore") {
		t.Errorf("help lacks --restore:\n%s", stderr)
	}
	if strings.Contains(stderr, "\n  -restore") {
		t.Errorf("help prints single-dash -restore:\n%s", stderr)
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
		{"restore with page", []string{"--restore", "provider"}, "take no page"},
		{"revert with page", []string{"--revert", "agents"}, "take no page"},
		{"restore and revert", []string{"--restore", "--revert"}, "mutually exclusive"},
		{"revert with check", []string{"--revert", "--check"}, "--revert takes only"},
		{"revert with out", []string{"--revert", "--out", "x.json"}, "--revert takes only"},
		{"revert with agents", []string{"--revert", "--agents"}, "--revert takes only"},
		{"out without restore", []string{"--out", "x.json"}, "need --restore"},
		{"agents without restore", []string{"--agents"}, "need --restore"},
		{"restore with check", []string{"--restore", "--check"}, "takes no --check or --dry-run"},
		{"restore with dry-run", []string{"--restore", "--dry-run"}, "takes no --check or --dry-run"},
		{"out with agents", []string{"--restore", "--agents", "--out", "x.json"}, "never write one"},
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

// TestConfigureRestoreResetsSettingsAfterBackup: bare --restore copies settings.json aside, then writes the render.
func TestConfigureRestoreResetsSettingsAfterBackup(t *testing.T) {
	live := filepath.Join(resetEnv(t), "settings.json")
	if err := os.WriteFile(live, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return run([]string{"--restore"}) })
	if code != 0 {
		t.Fatalf("bare --restore exit %d: %s", code, stderr)
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

// TestConfigureRestoreAgentsNeverTouchesSettings: --agents/--agent render only the agent configs.
func TestConfigureRestoreAgentsNeverTouchesSettings(t *testing.T) {
	dir := resetEnv(t)
	live := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(live, []byte(`{"model":"mine"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--restore", "--agents", "--agent", "codex"}, {"--restore", "--agent", "codex"}} {
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
		return run([]string{"--restore", "--agents", "--agent", "x"})
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
		{"with --list", []string{"provider", "--all", "--list"}, "cannot be combined with --list or --profile"},
		{"with --profile", []string{"provider", "--all", "--profile", "x"}, "cannot be combined with --list or --profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(tc.args) })
			if code != 1 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q lacks %q", code, stderr, tc.want)
			}
		})
	}
}

// TestConfigureAuthRejectedOutsideGithubPage: --auth is refused before the restore and list branches can drop it.
func TestConfigureAuthRejectedOutsideGithubPage(t *testing.T) {
	resetEnv(t)
	cases := []struct {
		name string
		args []string
	}{
		{"restore", []string{"--restore", "--auth", "follow"}},
		{"revert", []string{"--revert", "--auth", "follow"}},
		{"json list", []string{"provider", "--auth", "follow", "--format", "json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return run(tc.args) })
			if code != 1 || !strings.Contains(stderr, "--auth needs the github page") {
				t.Errorf("exit %d, stderr %q; want 1 and the --auth refusal", code, stderr)
			}
		})
	}
}

// TestConfigureAuthRejectedWithRestore: on the github page --auth with --restore or --revert fails instead of being dropped.
func TestConfigureAuthRejectedWithRestore(t *testing.T) {
	resetEnv(t)
	for _, flagName := range []string{"--restore", "--revert"} {
		t.Run(flagName, func(t *testing.T) {
			code, _, stderr := hosttest.CaptureOutput(t, func() int {
				return run([]string{"github", "--auth", "follow", flagName})
			})
			if code != 1 || !strings.Contains(stderr, "--auth cannot be combined with --restore or --revert") {
				t.Errorf("exit %d, stderr %q; want 1 and the combination refusal", code, stderr)
			}
		})
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
	for l := range strings.SplitSeq(string(b), "\n") {
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
		sb.WriteString(legacyTag + n + "\n")
	}
	for _, n := range names {
		sb.WriteString(rows[n] + "\n")
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
	if got := render(t, legacy); got != string(want) {
		t.Fatalf("registry differs from %s (run -update after reviewing):\n--- got\n%s--- want\n%s", goldenPath, got, want)
	}
}
