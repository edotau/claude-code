package pi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/rtkwrap"
	"github.com/edotau/claude-code/internal/platform/schemas"
	"github.com/edotau/claude-code/internal/platform/settings"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
)

var _ = schemas.Register(schemas.Format{
	Name: "pi-guard", Owner: "internal/driver/pi", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "pi", "*", "runs", "*", guardFile) },
	Version: 1, Clean: "pi-runs",
})

const (
	guardFile     = "guard.ts"
	readyFile     = "guard-ready"
	nonceFileEnv  = "HARNESS_PI_GUARD_NONCE_FILE"
	nonceValueEnv = "HARNESS_PI_GUARD_NONCE"
)

// Guard is a written guard extension: Argv loads it, Env arms it, Ready proves it loaded.
type Guard struct {
	Path, NonceFile, Nonce string
}

// guardBudgetMs is the time the extension gives one judged call: every PreToolUse module's budget plus a margin; the
// modules themselves run in the harness (`hook PreToolUse --pi-guard`), so none declared means no guard.
func guardBudgetMs() (int64, error) {
	var total int64
	for _, s := range settings.SpecsFor("pi", "plugin") {
		if s.Event == settings.EventPreToolUse {
			total += s.Budget.Milliseconds()
		}
	}
	if total == 0 {
		return 0, errors.New("pi guard: no PreToolUse modules are declared for pi")
	}
	return total + 2000, nil
}

// renderGuard fills the template's literals: the harness binary, the Pi version the guard was verified on, the budget.
func renderGuard(bin string, budgetMs int64) ([]byte, error) {
	binJSON, err := json.Marshal(bin)
	if err != nil {
		return nil, err
	}
	r := strings.NewReplacer("__BIN__", string(binJSON), "__PI_VERSION__", strconv.Quote(tools.PiPin), "__BUDGET_MS__", strconv.FormatInt(budgetMs, 10))
	return []byte(r.Replace(assets.PiGuardSource())), nil
}

// WriteGuard writes the extension (0600) into runDir, a private dir the harness made, and mints this run's nonce.
func WriteGuard(runDir string) (Guard, error) {
	bin, err := paths.SelfBinary()
	if err != nil {
		bin = filepath.Join(paths.BinDir(), paths.HookBinaryName) // a missing binary makes the guard block every call
	} else {
		bin = paths.HookBinary(bin)
	}
	budget, err := guardBudgetMs()
	if err != nil {
		return Guard{}, err
	}
	body, err := renderGuard(bin, budget)
	if err != nil {
		return Guard{}, err
	}
	raw := make([]byte, 16)
	if _, err = rand.Read(raw); err != nil {
		return Guard{}, fmt.Errorf("pi guard: nonce: %w", err)
	}
	g := Guard{Path: filepath.Join(runDir, guardFile), NonceFile: filepath.Join(runDir, readyFile), Nonce: hex.EncodeToString(raw)}
	if err = os.WriteFile(g.Path, body, 0o600); err != nil {
		return Guard{}, fmt.Errorf("pi guard: %w", err)
	}
	return g, nil
}

// Argv is Pi's explicit extension flag, which loads even under --no-extensions.
func (g Guard) Argv() []string { return []string{"-e", g.Path} }

// Env names the nonce file and value the extension reads on load.
func (g Guard) Env() []string {
	return []string{nonceFileEnv + "=" + g.NonceFile, nonceValueEnv + "=" + g.Nonce}
}

// Ready is nil only when the extension wrote this run's nonce; a missing or different file means the guard did not load.
func (g Guard) Ready() error {
	got, err := os.ReadFile(g.NonceFile)
	if err != nil {
		return fmt.Errorf("pi guard did not load: %w", err)
	}
	if subtle.ConstantTimeCompare(got, []byte(g.Nonce)) != 1 {
		return errors.New("pi guard did not load: nonce mismatch")
	}
	return nil
}

// writeGuard is WriteGuard; tests swap it to prove a failed write refuses the run.
var writeGuard = WriteGuard

const (
	// codemodeTool is Pi's script tool; codemodeSource is the builtin extension the guard accepts it from.
	codemodeTool   = "codemode"
	codemodeSource = "builtin:codemode"
	// guardCodemodeEnv tells the guard to allow codemodeTool, and only when codemodeSource registered it.
	guardCodemodeEnv = "HARNESS_PI_GUARD_CODEMODE"
)

// ResolveCodemode is the typed --codemode (a typed off stays off), else the vault CODEMODE under --tools, else "".
func ResolveCodemode(typed string, tools bool) (string, error) {
	if typed != "" || !tools {
		return typed, nil
	}
	mode := strings.ToLower(strings.TrimSpace(providers.CustomEnv()[rtkwrap.CodemodeEnv]))
	if mode == route.CodemodeOff || !slices.Contains(route.Codemodes, mode) {
		return "", nil
	}
	return mode, nil
}

// Experiment is the trial a run of req carries: code mode, else rtk wrapping wherever its bash tool runs shimmed.
func (piEngine) Experiment(req agent.RunOptions) string {
	return experiment(req.Codemode, req.Tools)
}

// experiment is the one trial a run carries; the shims never wrap under code mode, nor on Windows (no shims).
func experiment(codemode string, bash bool) string {
	switch {
	case rtkwrap.CodemodeOn(codemode):
		return "codemode"
	case bash && runtime.GOOS != "windows":
		return "rtk-wrap"
	}
	return "none"
}

// codemodeLaunchEnv is env exporting the effective code mode to Pi and putting the shim dir on PATH for it.
func codemodeLaunchEnv(env []string, mode string) []string {
	return rtkwrap.Launch(env, mode, agent.NoticeOut)
}

// checkCodemode refuses a code mode Pi would not run as asked: an unknown mode or no tools.
func checkCodemode(req agent.RunOptions, _ []string) error {
	if req.Codemode == "" {
		return nil
	}
	if !slices.Contains(route.Codemodes, req.Codemode) {
		return fmt.Errorf("pi: --codemode %q is not one of %s", req.Codemode, strings.Join(route.Codemodes, "|"))
	}
	if !rtkwrap.CodemodeOn(req.Codemode) {
		return nil
	}
	if !req.Tools {
		return errors.New("pi: --codemode needs --tools: its scripts call the guarded tools")
	}
	return nil
}

// codemodeArgv loads the builtin codemode extension explicitly (--no-extensions drops it) and turns its tool on.
func codemodeArgv(mode string) []string {
	if !rtkwrap.CodemodeOn(mode) {
		return nil
	}
	return []string{"-e", codemodeSource, "--tools", "+" + codemodeTool}
}

// writeCodemodeSettings sets codemode.mode in the run's private settings.json, the file WriteConfig wrote.
func writeCodemodeSettings(env []string, mode string) error {
	if !rtkwrap.CodemodeOn(mode) {
		return nil
	}
	file := filepath.Join(proc.LastEnv(env, agentDirEnv), "settings.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("pi codemode: %w", err)
	}
	s := map[string]any{}
	if err = json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("pi codemode: %s: %w", file, err)
	}
	s["codemode"] = map[string]any{"mode": mode}
	if raw, err = json.MarshalIndent(s, "", " "); err != nil {
		return err
	}
	if _, err = paths.WriteIfChanged(file, raw, 0o600); err != nil {
		return fmt.Errorf("pi codemode: %w", err)
	}
	return nil
}
