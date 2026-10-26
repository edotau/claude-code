package memory

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// e2eEnv is one isolated world: the built binary, a temp config/home, and a temp git repo as project and cwd.
type e2eEnv struct {
	bin, cfg, home, repo, pathDir string
}

// e2eModuleRoot is the repo root two levels above this file; the build must not run in the temp project.
func e2eModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// e2eRun executes the binary with a from-scratch env (no inherited CLAUDE_*, GIT_* or harvest-child vars).
func (e e2eEnv) run(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = e.repo
	cmd.Env = []string{
		"PATH=" + e.pathDir, // git only: a detached dream/harvest child cannot find a model CLI
		"HOME=" + e.home,
		"XDG_CONFIG_HOME=" + filepath.Join(e.home, ".config"),
		"CLAUDE_CONFIG_DIR=" + e.cfg,
		"CLAUDE_PROJECT_DIR=" + e.repo,
		"TMPDIR=" + e.home,
		"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.com",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s %v: %v", e.bin, args, err)
	}
	return out.String(), errb.String(), code
}

// mustRun fails the test unless the command exits 0.
func (e e2eEnv) mustRun(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	out, errs, code := e.run(t, stdin, args...)
	if code != 0 {
		t.Fatalf("%v exited %d\nstdout: %s\nstderr: %s", args, code, out, errs)
	}
	return out
}

// hookContext extracts hookSpecificOutput.additionalContext from a hook's stdout.
func hookContext(t *testing.T, stdout string) string {
	t.Helper()
	var doc struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("hook stdout is not hook JSON: %v\n%q", err, stdout)
	}
	return doc.HookSpecificOutput.AdditionalContext
}

// dirHashes snapshots every regular file under dir (path -> bytes) so a dry run can prove it wrote nothing.
func dirHashes(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			snap[p] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// TestE2EMemoryLifecycle drives init -> session-start -> remember -> recall -> promote -> index through the real binary.
func TestE2EMemoryLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the claude-code binary")
	}
	bin := filepath.Join(t.TempDir(), "claude-code")
	build := exec.Command("go", "build", "-o", bin, "./cmd/claude-code") // before isolatedConfig: keeps the real GOCACHE
	build.Dir = e2eModuleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	cfg, home := isolatedConfig(t) // parent env only locates the on-disk seed; subprocesses get their own env
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	pathDir := canonTemp(t, t.TempDir())
	if err := os.Symlink(gitPath, filepath.Join(pathDir, "git")); err != nil {
		t.Fatal(err)
	}
	repo := gitRepo(t, canonTemp(t, t.TempDir()), "proj")
	e := e2eEnv{bin: bin, cfg: cfg, home: home, repo: repo, pathDir: pathDir}

	initStore(t)                                                      // completed journal + store git repo, written under cfg by the same helper the unit tests use
	write(t, DreamStampPath(), time.Now().UTC().Format(time.RFC3339)) // dream not due: session-start spawns nothing

	const fact = "rotate the zebrafish kestrel gearbox calibration before every deploy"
	var bankDir, factID string

	t.Run("init", func(t *testing.T) {
		out := e.mustRun(t, "", "memory", "init")
		if !strings.Contains(out, "bank:") {
			t.Errorf("init output names no bank:\n%s", out)
		}
		bankDir = strings.TrimSpace(e.mustRun(t, "", "memory", "path"))
		if rel, err := filepath.Rel(StoreDir(), bankDir); err != nil || strings.HasPrefix(rel, "..") {
			t.Fatalf("bank dir %s is not under the store %s", bankDir, StoreDir())
		}
		for _, name := range BankFiles() {
			if _, err := os.Stat(filepath.Join(bankDir, name)); err != nil {
				t.Errorf("bank file %s missing: %v", name, err)
			}
		}
		if got := len(BankFiles()); got != 6 {
			t.Errorf("BankFiles = %d, want 6", got)
		}
	})

	t.Run("session-start", func(t *testing.T) {
		e.mustRun(t, "", "memory", "index") // a fresh scaffold has no focus text; the generated catalog is what names the bank
		in := `{"session_id":"e2e-session-start","cwd":"` + repo + `","hook_event_name":"SessionStart"}`
		out := e.mustRun(t, in, "hook", "session-start")
		ctx := hookContext(t, out)
		if !strings.Contains(ctx, "Session memory bank") || !strings.Contains(ctx, filepath.Base(bankDir)) {
			t.Errorf("additionalContext does not name the bank %s:\n%s", filepath.Base(bankDir), ctx)
		}
		link := filepath.Join(cfg, "projects", ProjectSlug(repo), "memory")
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("native link %s: %v (mode %v)", link, err, fi)
		}
		got, err := filepath.EvalSymlinks(link)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := filepath.EvalSymlinks(bankDir)
		if got != want {
			t.Errorf("native link resolves to %s, want the store bank %s", got, want)
		}
	})

	t.Run("remember", func(t *testing.T) {
		out := e.mustRun(t, "", "memory", "remember", "--type", "feedback", fact)
		m := regexp.MustCompile(`remembered \[([^\]]+)\]`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("remember printed no id:\n%s", out)
		}
		factID = m[1]
		// the task text said --type convention; the verb accepts only user|feedback|project|reference
		if _, errs, code := e.run(t, "", "memory", "remember", "--type", "convention", "x y z"); code == 0 {
			t.Errorf("--type convention accepted; the flag help lists only user/feedback/project/reference (stderr %q)", errs)
		}
		data, err := os.ReadFile(filepath.Join(bankDir, "conventions.md"))
		if err != nil || !strings.Contains(string(data), "zebrafish kestrel") {
			t.Errorf("fact not in conventions.md (err %v)", err)
		}
	})

	t.Run("recall-and-search", func(t *testing.T) {
		if out := e.mustRun(t, "", "memory", "recall", "zebrafish", "kestrel"); !strings.Contains(out, factID) {
			t.Errorf("recall misses id %s:\n%s", factID, out)
		}
		if out := e.mustRun(t, "", "memory", "search", "zebrafish", "kestrel"); !strings.Contains(out, factID) {
			t.Errorf("search misses id %s:\n%s", factID, out)
		}
	})

	t.Run("recall-hook", func(t *testing.T) {
		prompt := "how do I rotate the zebrafish kestrel gearbox calibration before deploy"
		b, _ := json.Marshal(map[string]string{"session_id": "e2e-recall", "cwd": repo, "prompt": prompt,
			"hook_event_name": "UserPromptSubmit"})
		out := e.mustRun(t, string(b), "hook", "memory-recall")
		ctx := hookContext(t, out)
		if !strings.Contains(ctx, "<!-- memory-recall:") || !strings.Contains(ctx, "zebrafish kestrel") {
			t.Errorf("recall hook injected no matching block:\n%q", out)
		}
	})

	t.Run("review-candidates", func(t *testing.T) {
		e.mustRun(t, "", "memory", "review", "--candidates")
	})

	t.Run("promote", func(t *testing.T) {
		homeBank := HomeBankDir()
		beforeLocal, beforeHome := dirHashes(t, bankDir), dirHashes(t, homeBank)
		e.mustRun(t, "", "memory", "promote", factID, "--to", "home", "--dry-run")
		if got := dirHashes(t, bankDir); !equalSnap(got, beforeLocal) {
			t.Error("--dry-run changed the local bank")
		}
		if got := dirHashes(t, homeBank); !equalSnap(got, beforeHome) {
			t.Error("--dry-run changed the home bank")
		}
		e.mustRun(t, "", "memory", "promote", factID, "--to", "home")
		homeConv, err := os.ReadFile(filepath.Join(homeBank, "conventions.md"))
		if err != nil || !strings.Contains(string(homeConv), "zebrafish kestrel") {
			t.Errorf("promoted fact missing from home conventions.md (err %v)", err)
		}
		local, _ := os.ReadFile(filepath.Join(bankDir, "conventions.md"))
		if strings.Contains(string(local), "zebrafish kestrel") {
			t.Error("promoted fact still in the local conventions.md")
		}
	})

	t.Run("update-procedure", func(t *testing.T) {
		if out := e.mustRun(t, "", "memory", "update"); strings.TrimSpace(out) == "" {
			t.Error("memory update printed no procedure")
		}
	})

	t.Run("update-transcript", func(t *testing.T) {
		// copilot and the other vendor legs are absent from <cfg>/bin, so the chain lands on the fake claude there
		const marker = "e2e-harvest-marker-7731"
		const session = "e2e-harvest-session"
		argvLog, stdinLog := filepath.Join(t.TempDir(), "argv.log"), filepath.Join(t.TempDir(), "stdin.log")
		script := "#!/bin/sh\n" +
			"for a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + argvLog + "'; done\n" +
			"while IFS= read -r l || [ -n \"$l\" ]; do printf '%s\\n' \"$l\"; done > '" + stdinLog + "'\n" + // PATH has no cat
			"printf '\\n- " + marker + "\\n' >> '" + filepath.Join(bankDir, "sessionHistory.md") + "'\n" +
			"echo '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"harvested\"}]}}'\n" +
			"echo '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"harvested\"}'\n"
		write(t, filepath.Join(cfg, "bin", "claude"), script)
		if err := os.Chmod(filepath.Join(cfg, "bin", "claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		transcript := filepath.Join(t.TempDir(), "session.jsonl")
		write(t, transcript, `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hello"}}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"DECISION: ship the zebrafish gearbox"}]}}`+"\n")
		before := dirHashes(t, cfg)

		out := e.mustRun(t, "", "memory", "update", "--transcript", transcript, "--cwd", repo, "--session", session)
		if !strings.Contains(out, "memory update: ran on") {
			t.Errorf("update reported no runner:\n%s", out)
		}
		prompt, _ := os.ReadFile(stdinLog)
		for _, want := range []string{transcript, "Background harvest", "DECISION: ship the zebrafish gearbox"} {
			if !strings.Contains(string(prompt), want) {
				t.Errorf("fake claude prompt lacks %q:\n%s", want, prompt)
			}
		}
		if argv, _ := os.ReadFile(argvLog); !strings.Contains(string(argv), "--print") {
			t.Errorf("fake claude was not run headless: %q", argv)
		}
		if hist, _ := os.ReadFile(filepath.Join(bankDir, "sessionHistory.md")); !strings.Contains(string(hist), marker) {
			t.Error("the fake reply's sessionHistory write is missing from the bank")
		}
		res, ok := ReadHarvestResult(session)
		if !ok || res.Status != HarvestOK || !res.BankUpdated {
			t.Errorf("harvest result = %+v ok=%v, want status ok with bank_updated", res, ok)
		}
		store, _ := filepath.EvalSymlinks(filepath.Join(cfg, "memory"))
		for p := range dirHashes(t, cfg) {
			if _, existed := before[p]; existed {
				continue
			}
			if r, _ := filepath.EvalSymlinks(p); !strings.HasPrefix(r, store+string(filepath.Separator)) &&
				!strings.HasPrefix(p, filepath.Join(cfg, "state")+string(filepath.Separator)) {
				t.Errorf("update created %s outside the store and state/", p)
			}
		}
	})

	t.Run("index-and-fmt", func(t *testing.T) {
		e.mustRun(t, "", "memory", "index")
		e.mustRun(t, "", "memory", "fmt", "--check")
	})

	t.Run("layout", func(t *testing.T) {
		if _, err := os.Lstat(filepath.Join(cfg, "docs", "memory")); err == nil {
			t.Errorf("legacy tree %s was created beside the live store", filepath.Join(cfg, "docs", "memory"))
		}
		store, _ := filepath.EvalSymlinks(filepath.Join(cfg, "memory"))
		bankNames := map[string]bool{}
		for _, n := range BankFiles() {
			bankNames[n] = true
		}
		err := filepath.WalkDir(cfg, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 || d.IsDir() || !bankNames[d.Name()] {
				return nil
			}
			if r, _ := filepath.EvalSymlinks(p); !strings.HasPrefix(r, store+string(filepath.Separator)) {
				t.Errorf("bank file %s lives outside %s", p, store)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func equalSnap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
