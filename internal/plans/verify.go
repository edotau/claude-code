package plans

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/shell"
)

// TaskVerify is the task line's `verify:` command, "" when it has none.
func TaskVerify(t Task) string { return strings.TrimSpace(waveAttributes(t.Title)["verify"]) }

// OwnedPaths is t's owns: entries expanded against each other (abbreviations spliced).
func OwnedPaths(t Task) []string { return ownEntries(t.Owns, nil) }

var errShape = errors.New("verify not run: shape")

// VerifyShape allows only go build|vet|test on ./ packages, agents diff --check and doctor, rtk forms, && chained.
func VerifyShape(cmd string) error {
	segs, seps := shell.SegmentsSep(cmd)
	words := shell.CommandWords(cmd)
	if len(segs) == 0 || len(words) != len(segs) || slices.ContainsFunc(seps, func(s string) bool { return strings.TrimSpace(s) != "&&" && s != "" }) {
		return errShape
	}
	for _, w := range words {
		if !allowedVerify(w) {
			return errShape
		}
	}
	return GateRefused(cmd)
}

var verifyEnv = []string{"GOOS=", "GOARCH=", "CGO_ENABLED="}
var goValueFlags = []string{"run", "skip", "count", "timeout", "tags", "p", "cpu", "bench"}
var goBoolFlags = []string{"v", "race", "short", "json", "cover", "failfast"}

func allowedVerify(w []string) bool {
	w = dropVerifyPrefix(w)
	if len(w) >= 3 && w[0] == "go" && slices.Contains([]string{"build", "vet", "test"}, w[1]) {
		return goArgsOK(w[2:])
	}
	return claudeCodeCheck(w)
}

// dropVerifyPrefix strips leading GOOS/GOARCH/CGO_ENABLED assignments and one rtk.
func dropVerifyPrefix(w []string) []string {
	for len(w) > 0 && slices.ContainsFunc(verifyEnv, func(p string) bool { return strings.HasPrefix(w[0], p) }) {
		w = w[1:]
	}
	if len(w) > 0 && w[0] == "rtk" {
		w = w[1:]
	}
	return w
}

// claudeCodeCheck is `claude-code doctor …` or `claude-code agents diff … --check …`.
func claudeCodeCheck(w []string) bool {
	if len(w) < 2 || w[0] != "claude-code" {
		return false
	}
	if w[1] == "doctor" {
		return true
	}
	return len(w) >= 4 && w[1] == "agents" && w[2] == "diff" && slices.Contains(w[3:], "--check")
}

func goArgsOK(args []string) bool {
	pkgs := 0
	for i := 0; i < len(args); i++ {
		name, _, inline := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		switch {
		case strings.HasPrefix(args[i], "./"):
			pkgs++
		case !strings.HasPrefix(args[i], "-"):
			return false
		case slices.Contains(goValueFlags, name):
			if !inline {
				i++
			}
		case !slices.Contains(goBoolFlags, name):
			return false
		}
	}
	return pkgs > 0
}

// DeferTick reports what sha would tick and starts one detached verifier per plan; it ticks the green ones later.
func DeferTick(top, sha string, files []string, session string) ([]PlanTick, []string, error) {
	pending, tied, err := PendingCommit(top, sha, files)
	var errs []error
	for i, r := range pending {
		if len(r.IDs) == 0 {
			continue
		}
		started, serr := spawnVerify(top, r.Plan, sha, r.IDs, session)
		errs = append(errs, serr)
		pending[i].Ticked = nil
		if started {
			pending[i].Notes = append([]string{"plans: verifying " + strings.Join(r.IDs, ", ") + " on " + sha[:min(7, len(sha))] +
				" before ticking; the result is a notice on your next prompt"}, r.Notes...)
		}
	}
	return pending, tied, errors.Join(append(errs, err)...)
}

// spawnVerify starts `plan tick --verify` once per sha and plan; an existing marker means a peer already started it.
func spawnVerify(top, plan, sha string, ids []string, session string) (bool, error) {
	dir := filepath.Join(paths.StateDir(), "verify")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	marker := filepath.Join(dir, sha[:min(12, len(sha))]+"-"+paths.Slug(filepath.Base(plan), "", 40, "plan")+".log")
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	bin, err := paths.SelfBinary()
	if err != nil {
		return false, err
	}
	args := []string{"plan", "tick", "--verify", "--plan", plan, "--sha", sha, "--session", session}
	for _, id := range ids {
		args = append(args, "--task", id)
	}
	if !proc.SpawnDetached(bin, args, top, f) {
		return false, errors.New("plans: the verifier did not start; tick by hand after checking: claude-code plan task <id> --done --sha " + sha)
	}
	return true, nil
}
