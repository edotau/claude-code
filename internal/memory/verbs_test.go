package memory

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// embedIndex uses the profile it is handed, not a fresh session/pin lookup.
func TestEmbedIndexUsesTheResolvedProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", "session")
	var opened string
	prev := openEmbedClient
	t.Cleanup(func() { openEmbedClient = prev })
	openEmbedClient = func(_ context.Context, profile, _ string) (*EmbedClient, error) {
		opened = profile
		return nil, errors.New("stub")
	}
	embedIndex(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, t.TempDir(), "resolved", false)
	if opened != "resolved" {
		t.Fatalf("embedIndex opened %q, want the profile it was given", opened)
	}
}

// referenceCalls greps memDir's non-test Go files for the verbs passed to Reference("<verb>").
func referenceCalls(t *testing.T, memDir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(memDir)
	if err != nil {
		t.Fatalf("read %s: %v", memDir, err)
	}
	callRe := regexp.MustCompile(`Reference\("([a-z-]+)"\)`)
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(memDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range callRe.FindAllStringSubmatch(string(data), -1) {
			seen[m[1]] = true
		}
	}
	return seen
}

// TestGoReferenceCallsResolveToEmbeddedPrompts: every verb passed to Reference (grepped) has a prompt.
func TestGoReferenceCallsResolveToEmbeddedPrompts(t *testing.T) {
	seen := referenceCalls(t, ".")
	if len(seen) == 0 {
		t.Fatal("no Reference(\"<verb>\") calls found in internal/memory — regex drifted or the API moved")
	}
	for verb := range seen {
		got, err := Reference(verb)
		if err != nil {
			t.Errorf("Reference(%q) is called from Go but errors: %v", verb, err)
			continue
		}
		if strings.TrimSpace(got) == "" {
			t.Errorf("Reference(%q) is called from Go but resolves to an empty prompt", verb)
		}
	}
}

// Given a cached notice naming a problem, When the problem is fixed and `memory index` settles, Then the cache clears.
func TestIndexRefreshesTheCachedDoctorNotice(t *testing.T) {
	tempConfig(t)
	prev := openEmbedClient
	t.Cleanup(func() { openEmbedClient = prev })
	openEmbedClient = func(context.Context, string, string) (*EmbedClient, error) { return nil, errors.New("stub") }
	decisionLog := filepath.Join(StoreDir(), "decisionLog.md")
	decisionLogOver(t, decisionLog, 80)
	if err := writeDoctorNotice(StoreDir(), DefaultStaleDays, time.Now()); err != nil {
		t.Fatal(err)
	}
	write(t, decisionLog, "# D\n")
	if err := Index(&bytes.Buffer{}, &bytes.Buffer{}, IndexOptions{Repo: StoreDir()}); err != nil {
		t.Fatal(err)
	}
	if notice := mustRead(t, doctorNoticePath(StoreDir())); notice != "" {
		t.Errorf("cached notice after the fix and an index = %q, want it cleared", notice)
	}
}

// TestIndexSaysWhenThereIsNoStore: a repo without a bank dir gets a plain "no store" line, not "indexed <dir>".
func TestIndexSaysWhenThereIsNoStore(t *testing.T) {
	tempConfig(t)
	root := gitRepo(t, canonTemp(t, t.TempDir()), "bare")
	var out bytes.Buffer
	if err := Index(&out, &bytes.Buffer{}, IndexOptions{Repo: root}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "indexed ") || !strings.Contains(got, "no store") || !strings.Contains(got, "memory init") {
		t.Fatalf("index output = %q; want a no-store line naming `memory init`", got)
	}
}

func TestAppendReportLandsUnderTheBanksReports(t *testing.T) {
	prev := now
	now = func() time.Time { return time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = prev })
	runBank(t, func(t *testing.T, b Bank) {
		got, err := appendReport(b, "rev", ReportInput{Report: "# R\n", Decisions: []string{"d"}})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{b.Sub("reports", "2026-11-08-rev.md"), b.Sub("reports", "2026-11-08-decisions.md")}
		if !slices.Equal(got, want) {
			t.Fatalf("appendReport = %v, want %v", got, want)
		}
		if !strings.HasPrefix(filepath.Base(got[0]), "repo--") {
			t.Errorf("report %s is not key-prefixed", got[0])
		}
	})
}

func TestResolveBankKeyAndPath(t *testing.T) {
	runRoot(t, func(t *testing.T, root string) {
		if _, err := Init(root); err != nil {
			t.Fatal(err)
		}
		top, b, err := ResolveBank(root)
		if err != nil || top != root || b.Key != "proj" {
			t.Fatalf("ResolveBank(path) = %q %+v %v", top, b, err)
		}
		top, b, err = ResolveBank("proj")
		if err != nil || b.Key != "proj" || top != "" {
			t.Errorf("ResolveBank(key) = %q %+v %v; a key carries no checkout", top, b, err)
		}
		if _, err := ResolveRoot("proj"); err == nil {
			t.Error("ResolveRoot(key) must err: a key names no checkout")
		}
		if _, _, err := ResolveBank("nosuch-bank"); err == nil {
			t.Error("an unknown key that is not a path must err")
		}
	})
}

func TestIndexFlatKeyEmbedsSelectedBank(t *testing.T) {
	isolatedConfig(t)
	b := Bank{Key: "alpha"}
	write(t, b.Path("decisionLog.md"), "## 2026-11-16 — Quasar alpha <!-- id:11111111 -->\nSelected evidence.\n")
	write(t, Bank{Key: "beta"}.Path("decisionLog.md"), "## 2026-11-16 — Quasar beta <!-- id:22222222 -->\nUnrelated evidence.\n")
	previous := openEmbedClient
	t.Cleanup(func() { openEmbedClient = previous })
	var embedded []string
	openEmbedClient = func(context.Context, string, string) (*EmbedClient, error) {
		return &EmbedClient{Model: "fixture", EmbedFunc: func(_ context.Context, texts []string) ([][]float32, error) {
			embedded = append(embedded, texts...)
			vectors := make([][]float32, len(texts))
			for i := range vectors {
				vectors[i] = []float32{1, 0}
			}
			return vectors, nil
		}}, nil
	}
	var out, errw bytes.Buffer
	if err := Index(&out, &errw, IndexOptions{Repo: b.Key}); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(embedded, "\n")
	if !strings.Contains(text, "Selected evidence") || strings.Contains(text, "Unrelated evidence") || strings.Contains(errw.String(), "embeddings skipped") {
		t.Fatalf("embedded=%q stderr=%s", text, errw.String())
	}
}
