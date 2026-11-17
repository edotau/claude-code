package safety

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

// tempCase is one command and its expected safety-scan decision.
type tempCase struct {
	name string
	cmd  string
	want core.Decision
}

// vaultIn creates <root>/state/vault.db for real and returns its path (the name an Edit deny glob protects).
func vaultIn(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "vault.db")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runTempCases(t *testing.T, cases []tempCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, msg := safetyScanDecision(t, hosttest.BashPayload(c.cmd)); got != c.want {
				t.Errorf("%q: %v, want %v (%s)", c.cmd, got, c.want, msg)
			}
		})
	}
}

func TestSafetyAllowsRemovingProtectedNamesUnderTempRoots(t *testing.T) {
	osTemp := vaultIn(t, filepath.Join(t.TempDir(), "cfg"))
	slashTmp, err := os.MkdirTemp("/tmp", "safety-rm-")
	if err != nil {
		t.Skip("no writable /tmp:", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(slashTmp) })
	inTmp := vaultIn(t, filepath.Join(slashTmp, "cfg"))
	cases := []tempCase{
		{"rm under os.TempDir", "rm -f " + osTemp, core.Allow},
		{"rm under /tmp", "rm " + inTmp, core.Allow},
		{"rm under /private/tmp", "rm /private/tmp/safety-rm-x/cfg/state/vault.db", core.Allow},
		{"unlink under /tmp", "unlink " + inTmp, core.Allow},
		{"rmdir under /tmp", "rmdir " + filepath.Dir(inTmp), core.Allow},
		{"mv to another temp path", "mv " + inTmp + " " + filepath.Join(slashTmp, "gone.db"), core.Allow},
		{"find -delete under /tmp", "find " + slashTmp + " -name vault.db -delete", core.Allow},
		{"rm after cd into the temp dir", "cd " + filepath.Dir(inTmp) + " && rm vault.db", core.Allow},
		{"rm -rf a temp dir holding one", "rm -rf " + filepath.Join(slashTmp, "cfg"), core.Allow},
		{"rm -rf a temp tree holding one", "rm -rf " + filepath.Dir(filepath.Dir(osTemp)), core.Allow},
	}
	if runtime.GOOS == "darwin" {
		cases = append(cases, tempCase{"rm under the per-user var/folders temp",
			"rm /var/folders/ab/cdef0123/T/x/cfg/state/vault.db", core.Allow})
	}
	runTempCases(t, cases)
}

func TestSafetyBlocksRemovalsOutsideTempRoots(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	tmp := t.TempDir()
	liveCfg := paths.ConfigDir()
	live := vaultIn(t, liveCfg)
	link := filepath.Join(tmp, "linkcfg")
	if err := os.Symlink(liveCfg, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	copyDir := filepath.Join(tmp, "copy")
	temp := vaultIn(t, copyDir)
	runTempCases(t, []tempCase{
		{"rm the live config vault", "rm " + live, core.Block},
		{"rm the live config vault via ~", "rm ~/.claude/state/vault.db", core.Block},
		{"mv the live vault into temp", "mv " + live + " " + filepath.Join(tmp, "gone.db"), core.Block},
		{"rm through a temp dir symlink into the config dir", "rm " + filepath.Join(link, "state", "vault.db"), core.Block},
		{"rm climbing out of temp with ..", "rm " + copyDir + "/../../.." + live, core.Block},
		{"rm a protected name outside every temp root", "rm /srv/data/state/vault.db", core.Block},
		{"rm through a glob", "rm " + filepath.Join(tmp, "*", "state", "vault.db"), core.Block},
		{"rm through a variable", "rm " + filepath.Join(tmp, "$X", "state", "vault.db"), core.Block},
		{"ln -s into the config dir, then rm", "ln -sfn " + liveCfg + " " + filepath.Join(tmp, "l2") +
			" && rm " + filepath.Join(tmp, "l2", "state", "vault.db"), core.Block},
		{"temp rm beside a real removal", "rm " + temp + " " + live, core.Block},
		{"mv the config dir into temp, then rm its vault", "mv " + liveCfg + " " + filepath.Join(tmp, "s") +
			" && rm " + filepath.Join(tmp, "s", "state", "vault.db"), core.Block},
	})
}

// A fixture vault or credentials file under a temp root may be seeded; the live ones stay blocked in every spelling.
func TestSafetyTempFixtureWrites(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	tmp := t.TempDir()
	temp := vaultIn(t, filepath.Join(tmp, "copy"))
	creds := filepath.Join(tmp, "h1", ".claude", ".credentials.json")
	liveCfg := paths.ConfigDir()
	live := vaultIn(t, liveCfg)
	link := filepath.Join(tmp, "linkcfg")
	if err := os.Symlink(liveCfg, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	runTempCases(t, []tempCase{
		{"redirect into a temp copy", "echo x > " + temp, core.Allow},
		{"cp onto a temp copy", "cp " + filepath.Join(tmp, "new.db") + " " + temp, core.Allow},
		{"tee a temp credentials file", "printf '{}' | tee " + creds, core.Allow},
		{"sed -i a temp copy", "sed -i s/a/b/ " + temp, core.Allow},
		{"mv onto a temp copy", "mv " + filepath.Join(tmp, "new.db") + " " + temp, core.Allow},
		{"remove a temp copy then write it", "rm " + temp + " && echo x > " + temp, core.Allow},
		{"truncate a temp copy", "truncate -s0 " + temp, core.Block},
		{"cp onto the live vault", "cp x " + live, core.Block},
		{"cp onto the live vault via ~", "cp x ~/.claude/state/vault.db", core.Block},
		{"cp through a temp symlink into the config dir", "cp x " + filepath.Join(link, "state", "vault.db"), core.Block},
		{"ln -s into the config dir, then cp", "ln -s " + liveCfg + " " + filepath.Join(tmp, "l3") + " && cp x " +
			filepath.Join(tmp, "l3", "state", "vault.db"), core.Block},
		{"cd into the config dir, then a relative cp", "cd " + liveCfg + " && cp x state/vault.db", core.Block},
		{"temp write beside a live one", "cp x " + temp + " " + live, core.Block},
		{"climbing out of temp with ..", "cp x " + filepath.Join(tmp, "copy") + "/../../.." + live, core.Block},
	})
}

// A throwaway temp repo's .git may be removed; the live tree's, or one reached by a link, cd or nested shell, asks.
func TestSafetyTempGitRemoval(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	tmp := t.TempDir()
	liveCfg := paths.ConfigDir()
	link := filepath.Join(tmp, "linkcfg")
	if err := os.Symlink(liveCfg, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	fx := filepath.Join(tmp, "fixture")
	runTempCases(t, []tempCase{
		{"rm -rf a temp fixture's .git", "rm -rf " + filepath.Join(fx, ".git"), core.Allow},
		{"rm -rf the live tree's .git", "rm -rf " + filepath.Join(liveCfg, ".git"), core.Block},
		{"rm -rf through a temp symlink, trailing slash", "rm -rf " + filepath.Join(link, ".git") + "/", core.Block},
		{"ln -s into the config dir, then rm", "ln -s " + liveCfg + " " + filepath.Join(tmp, "l4") + " && rm -rf " +
			filepath.Join(tmp, "l4", ".git"), core.Block},
		{"cd, then a relative .git", "cd " + fx + " && rm -rf .git", core.Block},
		{"a glob", "rm -rf " + filepath.Join(tmp, "*", ".git"), core.Block},
		{"inside bash -c", "bash -c 'rm -rf " + filepath.Join(fx, ".git") + "'", core.Block},
		{"temp .git beside the live one", "rm -rf " + filepath.Join(fx, ".git") + " " + filepath.Join(liveCfg, ".git"),
			core.Block},
	})
}

func TestSafetyKeepsDestructiveShapesOverTempRemovals(t *testing.T) {
	temp := vaultIn(t, filepath.Join(t.TempDir(), "copy"))
	runTempCases(t, []tempCase{
		{"rm -rf ~", "rm -rf ~", core.Block},
		{"rm -rf $HOME", "rm -rf $HOME", core.Block},
		{"rm -rf /", "rm -rf /", core.Block},
		{"rm -rf a system dir", "rm -rf /usr", core.Block},
		{"temp copy then the home directory", "rm " + temp + " && rm -rf ~", core.Block},
	})
}

func TestSafetyBlocksRemovingInTheLiveConfigDirUnderATempRoot(t *testing.T) {
	tmp := t.TempDir()
	liveCfg := filepath.Join(tmp, "livecfg")
	t.Setenv("CLAUDE_CONFIG_DIR", liveCfg)
	runTempCases(t, []tempCase{
		{"rm in a config dir that sits under temp", "rm " + filepath.Join(liveCfg, "state", "vault.db"), core.Block},
		{"rm in a sibling temp dir", "rm " + filepath.Join(tmp, "other", "state", "vault.db"), core.Allow},
	})
}
