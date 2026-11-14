package memory

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
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

// TestGoReferenceCallsResolveToEmbeddedPrompts: every verb Go's Reference(verb) is actually
// called with (grepped from internal/memory source, not hardcoded) resolves to a non-empty
// embedded prompt — commands/reflection and skills/reflection/references are gone, so this is the
// drift check that used to compare against on-disk files.
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
	decisionLog := filepath.Join(HomeBankDir(), "decisionLog.md")
	decisionLogOver(t, decisionLog, 80)
	if err := writeDoctorNotice(HomeBankDir(), DefaultStaleDays, time.Now()); err != nil {
		t.Fatal(err)
	}
	write(t, decisionLog, "# D\n")
	if err := Index(&bytes.Buffer{}, &bytes.Buffer{}, IndexOptions{Repo: HomeBankDir()}); err != nil {
		t.Fatal(err)
	}
	if notice := mustRead(t, doctorNoticePath(HomeBankDir())); notice != "" {
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
