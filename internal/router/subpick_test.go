package router

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// TestIsWorkRepoMarkers exercises the gitRemoteURLs seam directly: multiple remotes, no remotes, and a
// seam error (fail-closed).
func TestIsWorkRepoMarkers(t *testing.T) {
	orig := gitRemoteURLs
	t.Cleanup(func() { gitRemoteURLs = orig })
	origMarkers := workRepoMarkers
	workRepoMarkers = func() ([]string, error) { return []string{"git.corp.example", "corp-widgets"}, nil } // test-only stand-in markers
	t.Cleanup(func() { workRepoMarkers = origMarkers })
	cases := []struct {
		name string
		urls []string
		err  error
		want bool
	}{
		{"bitbucket corp", []string{"ssh://git.corp.example:7999/digitalbtd/x.git"}, nil, true},
		{"github corp-widgets org", []string{"https://github.com/corp-widgets/tool.git"}, nil, true},
		{"personal github", []string{"https://github.com/edotau/claude-code.git"}, nil, false},
		{"no remotes", nil, nil, false},
		{"personal origin, corp upstream (any url in the list is enough)", []string{
			"https://github.com/edotau/claude-code.git", "https://git.corp.example/digitalbtd/x.git",
		}, nil, true},
		{"seam error fails closed", nil, errors.New("git: corrupt config"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gitRemoteURLs = func(string) ([]string, error) { return c.urls, c.err }
			if got := isWorkRepoAt(t.TempDir()); got != c.want {
				t.Errorf("IsWorkRepo() = %v, want %v (urls=%v, err=%v)", got, c.want, c.urls, c.err)
			}
		})
	}
}

// TestIsWorkRepoMarkersLoadErrorFailsClosed checks a workRepoMarkers load error (the registry itself,
// not the git seam) makes IsWorkRepo report true — never fail open on the free gate.
func TestIsWorkRepoMarkersLoadErrorFailsClosed(t *testing.T) {
	origMarkers := workRepoMarkers
	workRepoMarkers = func() ([]string, error) { return nil, errors.New("vault: locked") }
	t.Cleanup(func() { workRepoMarkers = origMarkers })
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	if !IsWorkRepo(dir) {
		t.Error("a markers load error must fail closed (work), not open")
	}
}

// TestIsWorkRepoEmptyMarkersIsPersonal checks no markers configured (and no load error) reports personal
// with no git call at all — zen-free stays allowed when the owner has never set a work-repo marker.
func TestIsWorkRepoEmptyMarkersIsPersonal(t *testing.T) {
	origMarkers := workRepoMarkers
	workRepoMarkers = func() ([]string, error) { return nil, nil }
	t.Cleanup(func() { workRepoMarkers = origMarkers })
	orig := gitRemoteURLs
	gitRemoteURLs = func(string) ([]string, error) {
		t.Fatal("empty markers must short-circuit before any git call")
		return nil, nil
	}
	t.Cleanup(func() { gitRemoteURLs = orig })
	if IsWorkRepo(t.TempDir()) {
		t.Error("empty markers must report personal, so zen-free stays allowed")
	}
}

// isWorkRepoAt is IsWorkRepo over a dir that is itself a real git repo (so rev-parse --git-dir succeeds
// and the seam stub above is actually reached), isolating the stub cases from the real-git cases below.
func isWorkRepoAt(dir string) bool {
	runGit(nil, dir, "init", "-q")
	return IsWorkRepo(dir)
}

// runGit runs git -C dir <args...>, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		if t == nil {
			panic(string(out) + ": " + err.Error())
		}
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestIsWorkRepoRealGitRemotes exercises IsWorkRepo end to end against real scratch git repos (no seam
// stub), so the actual `git remote -v` parsing in gitRemoteURLs is proven, not just the stub above.
func TestIsWorkRepoRealGitRemotes(t *testing.T) {
	origMarkers := workRepoMarkers
	workRepoMarkers = func() ([]string, error) { return []string{"git.corp.example", "corp-widgets"}, nil } // test-only stand-in markers
	t.Cleanup(func() { workRepoMarkers = origMarkers })
	t.Run("personal origin plus corp upstream is work", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "-q")
		runGit(t, dir, "remote", "add", "origin", "https://github.com/edotau/claude-code.git")
		runGit(t, dir, "remote", "add", "upstream", "https://git.corp.example/digitalbtd/x.git")
		if !IsWorkRepo(dir) {
			t.Error("want work: upstream remote names a corp host")
		}
	})

	t.Run("corp pushurl only is work", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "-q")
		runGit(t, dir, "remote", "add", "origin", "https://github.com/edotau/claude-code.git")
		runGit(t, dir, "remote", "set-url", "--push", "origin", "https://git.corp.example/digitalbtd/x.git")
		if !IsWorkRepo(dir) {
			t.Error("want work: origin's pushurl names a corp host, even though its fetch url doesn't")
		}
	})

	t.Run("no remotes is personal", func(t *testing.T) {
		dir := t.TempDir()
		runGit(t, dir, "init", "-q")
		if IsWorkRepo(dir) {
			t.Error("want personal: a repo with no remotes at all")
		}
	})

	t.Run("not a git directory is personal", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "not-a-repo")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if IsWorkRepo(dir) {
			t.Error("want personal: a plain directory with no .git at all")
		}
	})
}

// opencodeCand returns the opencode candidate a pick chose, or fails.
func opencodeCand(t *testing.T, r Result) Candidate {
	t.Helper()
	for _, c := range r.Candidates {
		if c.Agent == "opencode" {
			return c
		}
	}
	t.Fatalf("no opencode candidate: %+v skipped %+v", r.Candidates, r.Skipped)
	return Candidate{}
}

func TestOpencodeSubPickFreeFirstOnPersonalRepo(t *testing.T) {
	c := opencodeCand(t, pickRepo(testTable(t), "sonnet", State{}, false))
	if c.Account != "opencode·zen-free" || c.Billing != BillFree {
		t.Errorf("personal sonnet pick = %s (%s), want the free Zen leg", c.Account, c.Billing)
	}
}

func TestOpencodeSubPickNeverFreeAtOpusOrOnWorkRepo(t *testing.T) {
	tb := testTable(t)
	for name, r := range map[string]Result{
		"opus":      pickRepo(tb, "opus", State{}, false),
		"work-repo": pickRepo(tb, "sonnet", State{}, true),
	} {
		c := opencodeCand(t, r)
		if c.Billing == BillFree {
			t.Errorf("%s: picked free leg %s", name, c.Account)
		}
		if c.Account != "opencode·go" {
			t.Errorf("%s: pick = %s, want the owner's first paid leg opencode·go", name, c.Account)
		}
	}
}

// opencodeCands returns every opencode candidate in r, in order.
func opencodeCands(r Result) []Candidate {
	var out []Candidate
	for _, c := range r.Candidates {
		if c.Agent == "opencode" {
			out = append(out, c)
		}
	}
	return out
}

// TestOpencodeSubPickAdjacentBackupOnPersonalRepo: the free winner carries its paid backup right after
// it, both at the front of the pick — score/sort must never separate the pair.
func TestOpencodeSubPickAdjacentBackupOnPersonalRepo(t *testing.T) {
	r := pickRepo(testTable(t), "sonnet", State{}, false)
	oc := opencodeCands(r)
	if len(oc) != 2 || oc[0].Account != "opencode·zen-free" || oc[0].Billing != BillFree ||
		oc[1].Account != "opencode·go" || oc[1].Billing == BillFree {
		t.Fatalf("opencode candidates = %+v, want [zen-free, go] adjacent", oc)
	}
	for i, c := range r.Candidates {
		if c.Agent == "opencode" {
			if i+1 >= len(r.Candidates) || r.Candidates[i+1].Agent != "opencode" {
				t.Fatalf("free+backup must sit adjacent, got %s", runners(r))
			}
			break
		}
	}
}

// TestOpencodeSubPickNoBackupDuplicateOffWorkRepoOrOpus: a paid-only pick (opus, or a work repo) carries
// exactly one opencode candidate — no backup riding along a winner that was never free.
func TestOpencodeSubPickNoBackupDuplicateOffWorkRepoOrOpus(t *testing.T) {
	tb := testTable(t)
	for name, r := range map[string]Result{
		"opus":      pickRepo(tb, "opus", State{}, false),
		"work-repo": pickRepo(tb, "sonnet", State{}, true),
	} {
		oc := opencodeCands(r)
		if len(oc) != 1 || oc[0].Account != "opencode·go" {
			t.Errorf("%s: opencode candidates = %+v, want exactly one (go, no backup)", name, oc)
		}
	}
}

// TestOpencodeSubPickZenFreeExhaustedFallsToGoAlone: an exhausted zen-free account drops out of the sub-pick
// entirely — opencode·go wins alone, with no backup pair (there is nothing left to pair it with).
func TestOpencodeSubPickZenFreeExhaustedFallsToGoAlone(t *testing.T) {
	st := State{"opencode·zen-free": {UsedPct: 100, Seen: now0, ExhaustedUntil: now0.Add(time.Hour)}}
	r := pickRepo(testTable(t), "sonnet", st, false)
	oc := opencodeCands(r)
	if len(oc) != 1 || oc[0].Account != "opencode·go" || oc[0].Billing == BillFree {
		t.Fatalf("exhausted zen-free: opencode candidates = %+v, want opencode·go alone", oc)
	}
	found := false
	for _, s := range r.Skipped {
		if s.Agent == "opencode·zen-free" && s.Gate == GateExhausted {
			found = true
		}
	}
	if !found {
		t.Errorf("skip reasons = %+v, want opencode·zen-free reported exhausted", r.Skipped)
	}
}

// denyOpencode fails every opencode cell (simulating no opencode login at all) and passes every other runner.
func denyOpencode(runner string, _ Cell) error {
	if runner == "opencode" {
		return errors.New("not configured: no opencode login")
	}
	return nil
}

// TestOpencodeSubPickNoLoginSkipsAndChainContinues: with no opencode login at all, every sub-candidate is
// skipped not-configured and the outer chain still produces other runners' candidates.
func TestOpencodeSubPickNoLoginSkipsAndChainContinues(t *testing.T) {
	tb := testTable(t)
	tb.OpencodeLadder = []string{"opencode-go", "github-copilot", "openai"}
	r := tb.Pick(context.Background(), "sonnet", "", State{}, Opts{Fallback: FallbackVendors, Configured: denyOpencode,
		Now: func() time.Time { return now0 }, WorkRepo: func() bool { return false }})
	if len(opencodeCands(r)) != 0 {
		t.Fatalf("opencode must be skipped entirely, got %s", runners(r))
	}
	if len(r.Candidates) == 0 {
		t.Fatal("the chain must continue past opencode to the other vendors")
	}
	var reasons int
	for _, s := range r.Skipped {
		if strings.HasPrefix(s.Agent, "opencode") && strings.Contains(s.Reason, "not configured") {
			reasons++
		}
	}
	if reasons == 0 {
		t.Errorf("skipped = %+v, want opencode sub-candidates reported not-configured", r.Skipped)
	}
}

// TestFreeTierOKGateBlocksSonnetAllowsHaiku checks the FreeTierOK gate directly: a registry that only
// enables zen-free at haiku must block the free leg at sonnet (falling to the paid opencode·go leg) while
// still serving the free leg at haiku.
func TestFreeTierOKGateBlocksSonnetAllowsHaiku(t *testing.T) {
	reg := &providers.Registry{Routing: providers.Routing{Opencode: providers.OpencodeRouting{FreeTiers: &[]string{"haiku"}}}}
	tb, err := LoadFrom(reg, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	sonnet := opencodeCand(t, pickRepo(tb, "sonnet", State{}, false))
	if sonnet.Billing == BillFree {
		t.Errorf("sonnet pick = %s (%s), want the free leg blocked (freeTiers = [haiku] only)", sonnet.Account, sonnet.Billing)
	}
	haiku := opencodeCand(t, pickRepo(tb, "haiku", State{}, false))
	if haiku.Account != "opencode·zen-free" || haiku.Billing != BillFree {
		t.Errorf("haiku pick = %s (%s), want the free Zen leg (freeTiers includes haiku)", haiku.Account, haiku.Billing)
	}
}

// TestOpencodeSubPickOverlayReorderRespected: an overlay that reorders opencodeLadder changes which paid
// backend rides as the free winner's backup — the free leg itself still always leads.
func TestOpencodeSubPickOverlayReorderRespected(t *testing.T) {
	tb := testTable(t)
	tb.OpencodeLadder = []string{"github-copilot", "opencode-go", "openai"} // overlay: copilot ahead of Go
	r := tb.Pick(context.Background(), "sonnet", "", State{}, Opts{Fallback: FallbackVendors, Configured: allConfigured,
		Now: func() time.Time { return now0 }, WorkRepo: func() bool { return false }})
	oc := opencodeCands(r)
	if len(oc) != 2 || oc[0].Account != "opencode·zen-free" {
		t.Fatalf("opencode candidates = %+v, want the free leg still leading", oc)
	}
	if oc[1].Account != "opencode·github-copilot" {
		t.Errorf("backup = %s, want the reordered ladder's new first paid entry (github-copilot)", oc[1].Account)
	}
}
