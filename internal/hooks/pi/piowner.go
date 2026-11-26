package pi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// piDispatchKeys and piCheckKeys are the typed arguments of the two owner actions.
var (
	piDispatchKeys = []string{"unit", "kind", "own", "tier", "prompt"}
	piCheckKeys    = []string{"id"}
)

// piOwner is the running owner's open contract; only an owner may dispatch or check.
func piOwner() (contract.Contract, error) {
	c, ok := heldContract(os.Getenv(proc.DispatchIDEnv))
	if !ok || c.Owner.Role != contract.RoleOwner {
		return contract.Contract{}, errors.New("only a contract owner may dispatch or check")
	}
	return c, nil
}

// piDispatchArgv builds `agents ask --background --format json --overlay <run keys> --brief <task>`; Go writes both.
func piDispatchArgv(input map[string]any, cwd string) ([]string, error) {
	c, err := piOwner()
	if err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}
	unit, prompt := strOf(input["unit"]), strOf(input["prompt"])
	kind, tier := contract.Kind(strOf(input["kind"])), strOf(input["tier"])
	switch {
	case strings.TrimSpace(unit) == "" || strings.TrimSpace(prompt) == "":
		return nil, errors.New("dispatch: unit and prompt are required")
	case !slices.Contains(contract.Kinds(), kind):
		return nil, fmt.Errorf("dispatch: kind %q is not one of %v", kind, contract.Kinds())
	case tier != "fast" && tier != "standard":
		return nil, fmt.Errorf("dispatch: tier %q is not fast or standard", tier)
	}
	own, err := piOwnWithin(input["own"], c.Scope.Own, cwd)
	if err != nil {
		return nil, err
	}
	brief := map[string]any{"own": own, "prompt": prompt,
		"contract": map[string]any{"unit": unit, "kind": string(kind), "role": string(contract.RoleFleet), "parent": c.ID,
			"limits": map[string]int{"tokens": max(c.Limits.Tokens-wire.SpentBy(c).Tokens, 1)}}}
	if c.Scope.Verify != "" { // the owner's typed verify, so a test-repair child's tests term can pass
		brief["verify"] = c.Scope.Verify
	}
	return piBriefArgv(tier, brief, unit)
}

// piBriefArgv adds a Go-derived report path to the task brief and builds the ask argv.
func piBriefArgv(tier string, brief map[string]any, unit string) ([]string, error) {
	report, err := piReportPath(unit)
	if err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}
	brief["report"] = report
	b, err := json.Marshal(brief)
	if err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}
	argv := []string{"agents", "ask", "--background", "--format", "json"}
	if agentconfig.DispatchOverlayContext(context.Background(), "") != "" {
		argv = append(argv, "--agent", "claude", "--tier", tier)
	} else {
		var ov agentconfig.Overlay
		ov.Agent, ov.Tier = "claude", tier
		argv = append(argv, "--overlay", ov.Doc())
	}
	return append(argv, "--brief", string(b)), nil
}

// piReportPath is a fresh dated report file under the memory reports dir, so a leaf's report term passes by any tool.
func piReportPath(unit string) (string, error) {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	slug := paths.Slug(unit, "", 60, "leaf")
	dir := paths.MemorySubDir("reports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%s-%x.md", time.Now().Format("2006-01-02"), slug, r)), nil
}

func strOf(v any) string { s, _ := v.(string); return s }

// piOwnWithin is the files list when every entry sits inside the owner's own scope, else an error naming the strays.
func piOwnWithin(list any, scope []string, cwd string) ([]string, error) {
	items, ok := list.([]any)
	if !ok || len(items) == 0 {
		return nil, errors.New("dispatch: own must be a non-empty array of files")
	}
	var own, stray []string
	for _, it := range items {
		s, _ := it.(string)
		if s == "" || !piInScope(s, scope, cwd) {
			stray = append(stray, fmt.Sprintf("%q", s))
			continue
		}
		own = append(own, piAbs(cwd, s)) // the child gets absolute files: no cwd can move them
	}
	if len(stray) > 0 {
		return nil, fmt.Errorf("dispatch: own files outside the owner's scope: %s", strings.Join(stray, ", "))
	}
	return own, nil
}

// piAbs is p cleaned and rooted at the call's cwd, the base Pi resolves every path against.
func piAbs(cwd, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.ToSlash(filepath.Clean(p))
}

func piInScope(file string, scope []string, cwd string) bool {
	f := piAbs(cwd, file)
	return slices.ContainsFunc(scope, func(o string) bool {
		o = piAbs(cwd, o)
		return f == o || strings.HasPrefix(f, strings.TrimSuffix(o, "/")+"/")
	})
}

// ownsDispatch reports whether id is a dispatch of owner c: its id, a holder, or an earlier retry that resolves to c.
func ownsDispatch(c contract.Contract, id string) bool {
	if id == c.ID || id == os.Getenv(proc.DispatchIDEnv) || slices.Contains(c.Holders, id) {
		return true
	}
	h, ok := heldContract(id)
	return ok && h.ID == c.ID
}

// piCheckArgv builds `agents status <id> --format json` for a dispatch whose record names this owner as parent.
func piCheckArgv(input map[string]any) ([]string, error) {
	c, err := piOwner()
	if err != nil {
		return nil, fmt.Errorf("check: %w", err)
	}
	id := strOf(input["id"])
	rec, err := agent.LoadRecord(id)
	if err != nil || rec.Parent == "" || !ownsDispatch(c, rec.Parent) {
		return nil, fmt.Errorf("check: %q is not a dispatch of this owner", id)
	}
	return []string{"agents", "status", rec.ID, "--format", "json"}, nil
}
