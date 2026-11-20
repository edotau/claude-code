package pi

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/settings"
)

var _ = schemas.Register(schemas.Format{
	Name: "pi-guard", Owner: "internal/pi", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "pi", "*", "runs", "*", guardFile) },
	Version: 1, Clean: "pi-runs",
})

//go:embed guard/index.ts
var guardTS string

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
	return []byte(r.Replace(guardTS)), nil
}

// WriteGuard writes the extension (0600) into runDir, a private dir the harness made, and mints this run's nonce.
func WriteGuard(runDir string) (Guard, error) {
	bin, err := paths.SelfBinary()
	if err != nil {
		bin = filepath.Join(paths.BinDir(), "claude-code") // a missing binary makes the guard block every call
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
	if string(got) != g.Nonce {
		return errors.New("pi guard did not load: nonce mismatch")
	}
	return nil
}
