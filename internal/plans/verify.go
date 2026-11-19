package plans

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// TaskVerify is the task line's `verify:` command, "" when it has none.
func TaskVerify(t Task) string { return strings.TrimSpace(waveAttributes(t.Title)["verify"]) }

// OwnedPaths is t's owns: entries expanded against each other (abbreviations spliced).
func OwnedPaths(t Task) []string { return ownEntries(t.Owns, nil) }

var errShape = errors.New("verify not run: shape")

// VerifyStep is one && segment of a verify line: its GOOS/GOARCH/CGO_ENABLED assignments and the argv they prefix.
type VerifyStep struct{ Env, Argv []string }

// VerifyShape allows only go build|vet|test on ./ packages, agents diff --check and doctor, rtk forms, && chained.
func VerifyShape(cmd string) error {
	steps, err := VerifyArgv(cmd)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if !allowedVerify(s.Argv) {
			return errShape
		}
	}
	return GateRefused(cmd)
}

// verifyBanned is every byte with shell meaning outside single quotes; && is the one separator taken.
const verifyBanned = "$`<>();|&\\\"#!*?[]{}~\x7f"

// andSep stands for && among verifyTokens' words; a NUL never survives the banned-byte check.
const andSep = "\x00&&"

// VerifyArgv splits cmd into argv steps with no shell: single quotes are literal, any other shell syntax is refused.
func VerifyArgv(cmd string) ([]VerifyStep, error) {
	toks, ok := verifyTokens(cmd)
	if !ok {
		return nil, errShape
	}
	var steps []VerifyStep
	start := 0
	for i := 0; i <= len(toks); i++ {
		if i < len(toks) && toks[i] != andSep {
			continue
		}
		if i == start {
			return nil, errShape
		}
		steps, start = append(steps, splitEnv(toks[start:i])), i+1
	}
	return steps, nil
}

func verifyTokens(cmd string) ([]string, bool) {
	var toks []string
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			toks, inWord = append(toks, cur.String()), false
			cur.Reset()
		}
	}
	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; {
		case c == '\'':
			lit, ok := singleQuoted(cmd[i+1:])
			if !ok {
				return nil, false
			}
			cur.WriteString(lit)
			inWord, i = true, i+1+len(lit)
		case strings.HasPrefix(cmd[i:], "&&"):
			flush()
			toks, i = append(toks, andSep), i+1
		case c == ' ' || c == '\t':
			flush()
		case c < 0x20 || strings.ContainsRune(verifyBanned, rune(c)):
			return nil, false
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	flush()
	return toks, true
}

// singleQuoted is the literal up to the closing quote; an unclosed quote or a control byte inside refuses.
func singleQuoted(s string) (string, bool) {
	end := strings.IndexByte(s, '\'')
	return s[:max(end, 0)], end >= 0 && !strings.ContainsFunc(s[:end], unicode.IsControl)
}

var verifyEnv = []string{"GOOS", "GOARCH", "CGO_ENABLED"}
var goValueFlags = []string{"run", "skip", "count", "timeout", "tags", "p", "cpu", "bench"}
var goBoolFlags = []string{"v", "race", "short", "json", "cover", "failfast"}

// splitEnv peels leading GOOS/GOARCH/CGO_ENABLED=<word> assignments off words; rtk stays argv[0].
func splitEnv(words []string) VerifyStep {
	n := 0
	for n < len(words)-1 && envAssign(words[n]) {
		n++
	}
	return VerifyStep{Env: words[:n], Argv: words[n:]}
}

func envAssign(w string) bool {
	name, val, ok := strings.Cut(w, "=")
	return ok && slices.Contains(verifyEnv, name) && onlyRunes(val, "_0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

func allowedVerify(w []string) bool {
	if len(w) > 0 && w[0] == "rtk" {
		w = w[1:]
	}
	if len(w) >= 3 && w[0] == "go" && slices.Contains([]string{"build", "vet", "test"}, w[1]) {
		return goArgsOK(w[2:])
	}
	return claudeCodeCheck(w)
}

// claudeCodeCheck is a read-only `claude-code doctor` mode or `claude-code agents diff --check` on repo paths.
func claudeCodeCheck(w []string) bool {
	if len(w) < 2 || w[0] != "claude-code" {
		return false
	}
	if w[1] == "doctor" {
		return doctorArgsOK(w[2:])
	}
	return len(w) >= 4 && w[1] == "agents" && w[2] == "diff" && diffArgsOK(w[3:])
}

// doctorArgsOK is at most one of --prompts, --mcp, --permissions plus --format; --fix, --scan and the rest write.
func doctorArgsOK(args []string) bool {
	modes := 0
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--format" && i+1 < len(args) && slices.Contains([]string{"json", "text"}, args[i+1]):
			i++
		case a == "--format=json" || a == "--format=text":
		case slices.Contains([]string{"--prompts", "--mcp", "--permissions"}, a):
			modes++
		default:
			return false
		}
	}
	return modes <= 1
}

// diffArgsOK is --check with --path <repo-relative> and --gates <names>; the harness supplies --base itself.
func diffArgsOK(args []string) bool {
	check := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--check" {
			check = true
			continue
		}
		name, val, inline := strings.Cut(args[i], "=")
		if !inline && i+1 < len(args) {
			i, val = i+1, args[i+1]
		}
		if ok := name == "--path" && repoRelative(val) || name == "--gates" && gateNames(val); !ok {
			return false
		}
	}
	return check
}

// repoRelative is a non-empty relative path with no .. element, so it cannot leave the tree it runs in.
func repoRelative(p string) bool {
	return p != "" && !filepath.IsAbs(p) && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "-") &&
		!slices.Contains(strings.Split(filepath.ToSlash(p), "/"), "..")
}

func gateNames(v string) bool {
	return v != "" && onlyRunes(v, ",-abcdefghijklmnopqrstuvwxyz")
}

func onlyRunes(s, set string) bool { return strings.Trim(s, set) == "" }

func goArgsOK(args []string) bool {
	pkgs := 0
	for i := 0; i < len(args); i++ {
		name, _, inline := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		switch {
		case strings.HasPrefix(args[i], "./"):
			if !repoRelative(args[i]) {
				return false
			}
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

var shaPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// ValidSHA is a 7-40 digit lower-case hex commit id, safe to use in a state path.
func ValidSHA(sha string) bool { return shaPattern.MatchString(sha) }

// DeferTick reports what sha would tick and starts one detached verifier per plan; it ticks the green ones later.
func DeferTick(top, sha string, files []string, session string) ([]PlanTick, []string, error) {
	if len(sha) != 40 { // a full sha needs no resolving; a short one does
		if full, err := proc.RunGit(context.Background(), top, "rev-parse", "--verify", sha+"^{commit}"); err == nil && ValidSHA(strings.TrimSpace(full)) {
			sha = strings.TrimSpace(full)
		}
	}
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

// verifyKeep is how long verify markers, logs and outputs stay in state/verify.
const verifyKeep = 7 * 24 * time.Hour

// spawnVerify starts `plan tick --verify` once per sha and plan; a live or finished marker means no new run.
func spawnVerify(top, plan, sha string, ids []string, session string) (bool, error) {
	if !ValidSHA(sha) {
		return false, errors.New("plans: not a commit sha: " + sha)
	}
	dir := filepath.Join(paths.StateDir(), "verify")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	PruneVerify(dir, "*.log*")
	marker := filepath.Join(dir, sha[:min(12, len(sha))]+"-"+paths.Slug(filepath.Base(plan), "", 40, "plan")+".log")
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) && staleMarker(marker) {
		_ = os.Remove(marker + ".pid")
		_ = os.Remove(marker)
		f, err = os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // a peer that re-took it wins
	}
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	bin, err := paths.SelfBinary()
	if err != nil {
		_ = os.Remove(marker)
		return false, err
	}
	args := []string{"plan", "tick", "--verify", "--plan", plan, "--sha", sha, "--session", session}
	for _, id := range ids {
		args = append(args, "--task", id)
	}
	pid := proc.SpawnDetachedPID(bin, args, top, f)
	if pid == 0 {
		_ = os.Remove(marker)
		return false, errors.New("plans: the verifier did not start; tick by hand after checking: claude-code plan task <id> --done --sha " + sha)
	}
	start, _ := proc.StartTime(pid)
	return true, os.WriteFile(marker+".pid", []byte(strconv.Itoa(pid)+"\n"+start+"\n"), 0o600)
}

// staleMarker is a marker whose verifier died before finishing (no "plans: " result line) and no live pid holds.
func staleMarker(marker string) bool {
	log, err := os.ReadFile(marker)
	if err != nil || strings.HasPrefix(string(log), "plans: ") || strings.Contains(string(log), "\nplans: ") {
		return false
	}
	raw, err := os.ReadFile(marker + ".pid")
	if err != nil {
		st, serr := os.Stat(marker)
		return serr == nil && time.Since(st.ModTime()) > time.Minute // older than any spawn still writing its pid
	}
	pidText, start, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		return true
	}
	got, err := proc.StartTime(pid)
	return errors.Is(err, proc.ErrNoProcess) || err == nil && got != start
}

// PruneVerify removes entries of dir matching glob that are older than verifyKeep.
func PruneVerify(dir, glob string) {
	old, _ := filepath.Glob(filepath.Join(dir, glob))
	for _, p := range old {
		if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > verifyKeep {
			_ = os.RemoveAll(p)
		}
	}
}
