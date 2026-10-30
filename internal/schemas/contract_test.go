package schemas_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/clean"
	_ "github.com/edotau/claude-code/internal/cli" // links every package that registers a row
	"github.com/edotau/claude-code/internal/schemas"
)

var update = flag.Bool("update", false, "regenerate testdata/registry.golden")

const (
	goldenPath = "testdata/registry.golden"
	legacyTag  = "# legacy-layout: "
	// outside marks a Glob that is fixed at package init, so it cannot follow CLAUDE_CONFIG_DIR in a test.
	outside = "<fixed at init>"
)

// decodable reports whether the row has a JSON shape the fixtures describe.
func decodable(f schemas.Format) bool {
	return f.New != nil && f.Kind != schemas.Store && f.Kind != schemas.Derived
}

func contractDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
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
	dir := contractDir(t)
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

func TestContractCleanSweeps(t *testing.T) {
	for _, f := range schemas.All() {
		if f.Clean == "" {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			dir := contractDir(t)
			path := strings.ReplaceAll(f.Glob(), "*", "contract-fixture")
			if !strings.HasPrefix(path, dir) {
				t.Fatalf("glob %s outside %s", path, dir)
			}
			sweep := func() string {
				var out, errw bytes.Buffer
				if code := clean.Run([]string{"--target", f.Clean, "--dry-run", "--older-than", "30d"}, &out, &errw); code != 0 {
					t.Fatalf("clean exit %d: %s", code, errw.String())
				}
				return out.String() + errw.String()
			}
			if got := sweep(); strings.Contains(got, f.Clean+" ") && strings.Contains(got, "would remove") {
				t.Fatalf("empty tree already reports a removal: %s", got)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().AddDate(0, 0, -400)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			got := sweep()
			if !strings.Contains(got, f.Clean) || !strings.Contains(got, "would remove 1 entr") {
				t.Fatalf("clean --target %s does not sweep %s:\n%s", f.Clean, path, got)
			}
		})
	}
}

func goldenLegacy(t *testing.T) map[string]bool {
	t.Helper()
	m := map[string]bool{}
	b, err := os.ReadFile(goldenPath)
	if err != nil {
		return m
	}
	for _, l := range strings.Split(string(b), "\n") {
		if n, ok := strings.CutPrefix(l, legacyTag); ok {
			m[strings.TrimSpace(n)] = true
		}
	}
	return m
}

func render(t *testing.T, legacy map[string]bool) string {
	dir := contractDir(t)
	var sb strings.Builder
	sb.WriteString("# Reviewable registry: name | kind | version | glob (relative to the config dir) | owner | clean:<target>|keep\n")
	sb.WriteString("# A change here is a change to the on-disk contract; regenerate with: go test ./internal/schemas -run Golden -update\n")
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
