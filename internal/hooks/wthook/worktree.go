package wthook

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// Create replaces `.claude/worktrees/<name>` with <root>/<repo>/<name>, printing the path; it fails CLOSED.
func Create(in *core.Input) core.Result {
	var stderr bytes.Buffer
	// The live payload carries only `name`; the documented worktree_path/branch are honored if they ever appear.
	name := in.Nested("name")
	if name == "" {
		name = filepath.Base(filepath.Clean(in.Nested("worktree_path")))
	}
	repo, err := worktree.ResolveRepo(in.CWD)
	if top, _ := paths.CheckoutOf(in.CWD); err != nil && top == "" { // no repo at cwd: a harness lane
		repo, err = worktree.ResolveRepo(paths.ConfigDir())
	}
	if err != nil {
		fmt.Fprintf(&stderr, "worktree-create: %v\n", err)
		return core.ExitResult(1, stderr.String())
	}
	ctx, cancel := core.HookContext("worktree-create")
	defer cancel()
	deadline, _ := ctx.Deadline()
	dest, err := worktree.Create(repo, name, worktree.CreateOptions{
		Branch: in.Nested("branch"), Session: in.SessionID, Log: &stderr, Deadline: deadline,
	})
	if err != nil {
		fmt.Fprintf(&stderr, "worktree-create: %v\n", err)
		return core.ExitResult(1, stderr.String())
	}
	res := core.ExitResult(core.ExitProceed, stderr.String())
	res.Stdout = strings.TrimSpace(dest)
	return res
}

// Remove removes a hook-created worktree; no --force, so a dirty tree stays and the removal reports failure.
func Remove(in *core.Input, stderr io.Writer) int {
	path := in.Nested("worktree_path")
	if path == "" {
		fmt.Fprintln(stderr, "worktree-remove: payload has no worktree_path")
		return 1
	}
	if reason, err := worktree.InUseBy(path); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: in-use check failed: %v\n", err)
		return 1
	} else if reason != "" {
		fmt.Fprintf(stderr, "worktree-remove: %s\n", reason)
		return 1
	}
	ctx, cancel := core.HookContext("worktree-remove")
	defer cancel()
	main := paths.MainCheckout(path)
	branch, err := proc.RunGit(ctx, path, "branch", "--show-current")
	if err != nil {
		fmt.Fprintf(stderr, "worktree-remove: warning: branch probe failed, its branch is kept: %v\n", err)
	}
	head, err := proc.RunGit(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		fmt.Fprintf(stderr, "worktree-remove: warning: HEAD probe failed, the ledger records no head: %v\n", err)
	}
	if _, err := proc.RunGit(ctx, main, "worktree", "remove", path); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: %v\n", err)
		return 1
	}
	branchDeleted := false
	if b := strings.TrimSpace(branch); b != "" {
		_, derr := proc.RunGit(ctx, main, "branch", "-d", b)
		branchDeleted = derr == nil
	}
	closeRemoved(main, path, strings.TrimSpace(head), branchDeleted, stderr)
	return core.ExitProceed
}

// closeRemoved records the removal in the worktree ledger; a failed write only warns.
func closeRemoved(main, path, head string, branchDeleted bool, stderr io.Writer) {
	if err := worktree.CloseLedger(main, filepath.Base(filepath.Clean(path)), worktree.LedgerClose{
		Via: "remove", Head: head, BranchDeleted: branchDeleted,
	}); err != nil {
		fmt.Fprintf(stderr, "worktree-remove: warning: ledger: %v\n", err)
	}
}

// worktreeModeEnv is the vault's worktree mode; only `lanes` changes this hook package's behaviour.
const worktreeModeEnv = "WORKTREE_MODE"

// worktreeLanes is lanes mode: nothing tracked is edited in the live checkout, work goes to a lane.
func worktreeLanes() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(worktreeModeEnv)), "lanes")
}

// Busy runs on UserPromptSubmit: the busy line when the peer set changed (mode on), then drained notices.
func Busy(in *core.Input) core.Result {
	ctx, cancel := core.HookContext("worktree-busy")
	defer cancel()
	run := newRun(ctx, in)
	busyWarn := ""
	if err := notePrompt(ctx, in); err != nil { // before the notices, so this prompt counts the owner present
		busyWarn = fmt.Sprintf("worktree-busy: WARNING — %v", err)
	}
	notes, warn := noticeContext(ctx, in, run)
	audits := strings.Join(worktree.AuditReports(), "\n")
	// notes lead with fix-now directives, already recorded as delivered, so unbounded peer and audit text goes after
	return core.Result{Context: core.JoinContext(notes, audits), Stderr: core.JoinContext(busyWarn, warn)}
}

// checkoutInfo is CheckoutPeersInfo's result.
type checkoutInfo struct {
	top, branch string
	nested      []string
}

// hookRun memoizes checkout and peer discovery for the payload's directory (cwd, else the process's).
type hookRun struct {
	dir      string
	checkout func() checkoutInfo
	peers    func() ([]worktree.Proc, error)
}

func newRun(ctx context.Context, in *core.Input) *hookRun {
	r := &hookRun{dir: in.CWD}
	if r.dir == "" {
		r.dir, _ = os.Getwd()
	}
	r.checkout = sync.OnceValue(func() checkoutInfo {
		top, branch, nested := worktree.CheckoutPeersInfo(ctx, r.dir)
		return checkoutInfo{top, branch, nested}
	})
	r.peers = sync.OnceValues(func() ([]worktree.Proc, error) {
		c := r.checkout()
		return worktree.PeersOf(ctx, c.top, c.nested)
	})
	return r
}

// busyScanTTL is how long the state file's mtime vouches for the last peer scan (about 300 ms of git and lsof).
var busyScanTTL = time.Minute

// gitAnyCmd pre-screens a Bash command for any git invocation: the moment a drained notice matters most.
var gitAnyCmd = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\bgit\b`) })

// failureLines caps the failure lines per prompt (about 1.2 KB), apart from the 4000-byte inbox drains.
const (
	failureLines = 3
	failureFetch = 500 // the log keeps 500 records; filter by repo before the cap
	failureCols  = 80
	failureWait  = 200 * time.Millisecond
)

// repoFailures keeps the records of the session's main checkout and those carrying no repo.
func repoFailures(fails []worktree.Failure, top string) []worktree.Failure {
	if len(fails) == 0 {
		return nil
	}
	want := realDir(top)
	real := map[string]string{top: want, want: want}
	return slices.DeleteFunc(slices.Clone(fails), func(f worktree.Failure) bool {
		if f.Repo == "" {
			return false
		}
		r, ok := real[f.Repo]
		if !ok {
			r = realDir(f.Repo)
			real[f.Repo] = r
		}
		return r != want
	})
}

func realDir(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// failureNotice renders f as one line within failureCols; open is true while nobody holds or finished it.
func failureNotice(f worktree.Failure) (line string, open bool) {
	holder, done := worktree.FailureHolder(worktree.FailuresDir(), f.ID)
	kind, holder := string(f.Kind), textutil.CapRunes(holder, 16, "…")
	room := failureCols - len("failure  ") - len(kind) - 1 // the ellipsis
	if holder != "" {
		room -= len(assets.HookFailureLine("", "", holder)) - len("failure  ")
	}
	body := textutil.CapRunes(strings.Join(strings.Fields(f.Subject+": "+f.Summary), " "), max(room, 8), "…")
	return assets.HookFailureLine(kind, body, holder), holder == "" && !done
}

// inboxLines is this session's drained notices plus the divergence line, and the warning an infra error earns.
func inboxLines(ctx context.Context, in *core.Input, run *hookRun) (lines []string, warn string) {
	notices, drainErr := worktree.DrainFor(in.SessionID, func() string { return run.checkout().top })
	for _, n := range notices {
		who := n.Kind + ", from " + n.From
		if n.Kind == n.From {
			who = n.Kind
		}
		lines = append(lines, assets.HookNoticeLine(who, n.Text))
	}
	if drainErr != nil {
		warn += assets.HookNoticeDrainWarn(drainErr.Error())
	}
	line, err := divergedLine(ctx, in, run)
	if err != nil {
		warn = strings.TrimSpace(warn + assets.HookNoticeDivergeWarn(err.Error()))
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines, warn
}

// noticeContext drains this session's inbox and adds the divergence line; warn names an infra error (fail open).
func noticeContext(ctx context.Context, in *core.Input, run *hookRun) (text, warn string) {
	fails, more, failErr := worktree.FailuresSinceWithin(worktree.FailuresDir(), in.SessionID, failureFetch, failureWait)
	if errors.Is(failErr, paths.ErrLockTimeout) { // busy log: stay silent, the cursor did not move
		failErr = nil
	}
	tail, warn := inboxLines(ctx, in, run) // the slow reads run first: a directive is recorded only just before the return
	main := mainSession(ctx, in)
	var lines []string
	var sent map[string]bool
	if len(fails) > 0 || main {
		_, mainRepo := paths.CheckoutOf(run.dir)
		var fixErr error
		lines, sent, fixErr = fixNowLines(ctx, in, mainRepo, true)
		failErr = cmp.Or(failErr, fixErr)
		fails = slices.DeleteFunc(repoFailures(fails, mainRepo), func(f worktree.Failure) bool { return fixNowHides(ctx, main, sent, f) })
	}
	lines = appendFailureLines(lines, fails, more)
	if failErr != nil {
		warn += assets.HookFailuresWarn(failErr.Error())
	}
	lines = append(lines, tail...)
	return core.CapContext(strings.Join(lines, "\n")), warn
}

func appendFailureLines(lines []string, fails []worktree.Failure, more int) []string {
	if len(fails) > failureLines {
		more, fails = more+len(fails)-failureLines, fails[:failureLines]
	}
	claim := ""
	for _, f := range fails {
		line, open := failureNotice(f)
		lines = append(lines, line)
		if open && claim == "" {
			claim = assets.HookFailureClaim(f.ID)
		}
	}
	if more > 0 {
		lines = append(lines, assets.HookFailuresMore(more))
	}
	if claim != "" {
		lines = append(lines, claim)
	}
	return lines
}

// divergence is a checkout whose HEAD and upstream both have commits the other lacks.
type divergence struct {
	head, up, branch, upName string
	ahead, behind            int
	rewritten                bool // some commit's patch sits on both sides
}

// divergedLine names a shared checkout's local/upstream divergence once per (HEAD, upstream) pair; "" otherwise.
func divergedLine(ctx context.Context, in *core.Input, run *hookRun) (string, error) {
	if !divergedCheckDue(in.SessionID) {
		return "", nil
	}
	d, err := divergedAt(ctx, run.dir)
	if d == nil || err != nil {
		return "", err
	}
	shared, err := checkoutShared(run)
	if !shared || err != nil {
		return "", err
	}
	if !firstSighting(in.SessionID, "diverged\t"+d.head+"\t"+d.up) {
		return "", nil
	}
	return assets.HookDiverged(d.branch, d.upName, d.ahead, d.behind, d.rewritten), nil
}

// divergedCheckDue rate-limits the divergence git call to once per busyScanTTL per session, via an empty stamp file.
func divergedCheckDue(session string) bool {
	stamp := filepath.Join(paths.StateDir(), "worktree-diverged", worktree.SessionSlug(session))
	st, err := os.Stat(stamp)
	if err == nil && time.Since(st.ModTime()) < busyScanTTL {
		return false
	}
	if os.MkdirAll(filepath.Dir(stamp), 0o700) == nil {
		if err != nil {
			core.PruneSeen(filepath.Dir(stamp))
		}
		_ = os.WriteFile(stamp, nil, 0o600)
	}
	return true
}

type divergenceMemo struct {
	key   string
	value *divergence
}

var divergenceCache = struct {
	sync.Mutex
	byDir map[string]divergenceMemo
}{byDir: make(map[string]divergenceMemo)}

// divergenceKey includes ref names because equal commits can belong to differently named tracking branches.
func divergenceKey(dir string) (string, bool) {
	key, branch, up, ok := namedRefPair(dir)
	return key + "\t" + branch + "\t" + up, ok
}

// divergedAt reuses the full result only while refs, tracking names and ancestry safety checks hold.
func divergedAt(ctx context.Context, dir string) (*divergence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, keyed := divergenceKey(dir)
	if keyed {
		divergenceCache.Lock()
		memo, found := divergenceCache.byDir[dir]
		divergenceCache.Unlock()
		if found && memo.key == key {
			return cloneDivergence(memo.value), nil
		}
	}
	d, err := readDivergence(ctx, dir)
	if again, ok := divergenceKey(dir); keyed && ok && again == key && err == nil {
		divergenceCache.Lock()
		if len(divergenceCache.byDir) >= 128 {
			clear(divergenceCache.byDir)
		}
		divergenceCache.byDir[dir] = divergenceMemo{key, cloneDivergence(d)}
		divergenceCache.Unlock()
	}
	return d, err
}

func cloneDivergence(d *divergence) *divergence {
	if d == nil {
		return nil
	}
	copy := *d
	return &copy
}

// readDivergence reads dir's ahead/behind counts against its upstream; nil when there is no upstream or no divergence.
func readDivergence(ctx context.Context, dir string) (*divergence, error) {
	a, b, err := aheadBehind(ctx, dir)
	if err != nil || a == 0 || b == 0 {
		return nil, err
	}
	ids, _, err := proc.Git(ctx, dir, "rev-parse", "--show-toplevel", "HEAD", "@{upstream}", "--abbrev-ref", "HEAD",
		"@{upstream}")
	if len(ids) != 5 {
		return nil, err
	}
	// A rewrite (amend, rebase) leaves the same patch on both sides; --cherry-mark tags those commits "=".
	marked, _, err := proc.Git(ctx, dir, "rev-list", "--left-right", "--cherry-mark", "HEAD...@{upstream}")
	if err != nil {
		return nil, err
	}
	rewritten := slices.ContainsFunc(marked, func(l string) bool { return strings.HasPrefix(l, "=") && hasPatch(ctx, ids[0], l[1:]) })
	return &divergence{head: ids[1], up: ids[2], branch: ids[3], upName: ids[4], ahead: a, behind: b,
		rewritten: rewritten}, nil
}

// hasPatch reports whether commit changes a file; empty commits share one patch-id yet replace nothing.
func hasPatch(ctx context.Context, dir, commit string) bool {
	files, _, err := proc.Git(ctx, dir, "diff-tree", "--root", "-r", "--name-only", "--no-commit-id", commit)
	return err != nil || len(files) > 0 && files[0] != ""
}

// bothSides parses rev-list --left-right --count output; ok=false for anything else.
func bothSides(line string) (ahead, behind int, ok bool) {
	f := strings.Fields(line)
	if len(f) != 2 {
		return 0, 0, false
	}
	ahead, aerr := strconv.Atoi(f[0])
	behind, berr := strconv.Atoi(f[1])
	return ahead, behind, aerr == nil && berr == nil
}

// aheadBehind is rev-list's HEAD...@{upstream} counts, reused while the HEAD and upstream ref shas stay unchanged.
func aheadBehind(ctx context.Context, dir string) (ahead, behind int, err error) {
	key, keyed := refPair(dir)
	cache := filepath.Join(paths.StateDir(), "worktree-diverged", "revs-"+textutil.SHA256Hex([]byte(key))[:16])
	if keyed {
		if ahead, behind, ok := cachedCounts(cache, key); ok {
			return ahead, behind, nil
		}
	}
	counts, ok, err := proc.Git(ctx, dir, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
	if err != nil || !ok {
		return 0, 0, err // !ok: not a repo, detached, or no upstream
	}
	ahead, behind, _ = bothSides(counts[0])
	// Refs that moved while rev-list ran may not match its counts, so only an unchanged key is stored.
	if again, _ := refPair(dir); keyed && again == key && os.MkdirAll(filepath.Dir(cache), 0o700) == nil {
		_ = paths.AtomicWriteNoSync(cache, []byte(key+"\n"+counts[0]), 0o600) // a lost cache only costs the next spawn
	}
	return ahead, behind, nil
}

// cachedCounts reads the counts stored under key; ok=false for a missing, stale or malformed file.
func cachedCounts(cache, key string) (ahead, behind int, ok bool) {
	raw, err := os.ReadFile(cache)
	k, counts, cut := strings.Cut(string(raw), "\n")
	if err != nil || !cut || k != key {
		return 0, 0, false
	}
	return bothSides(counts)
}

// refPair is "<checkout>\t<HEAD sha>\t<upstream sha>" read from the ref files; ok=false leaves it to git.
func refPair(dir string) (string, bool) {
	key, _, _, ok := namedRefPair(dir)
	return key, ok
}

func namedRefPair(dir string) (key, branchName, upstream string, ok bool) {
	top, gitDir, common, ok := keyableCheckout(dir)
	if !ok {
		return "", "", "", false
	}
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	ref, symbolic := strings.CutPrefix(strings.TrimSpace(string(raw)), "ref: ")
	branch, isBranch := strings.CutPrefix(ref, "refs/heads/")
	if err != nil || !symbolic || !isBranch {
		return "", "", "", false
	}
	upRef, ok := upstreamRef(common, branch)
	head, headOK := refSha(common, ref)
	up, upOK := refSha(common, upRef)
	if !ok || !headOK || !upOK {
		return "", "", "", false
	}
	return top + "\t" + head + "\t" + up, branch, upRef, true
}

// keyableCheckout finds dir's checkout as git would; ok=false when env or repo state makes file-read counts unsafe.
func keyableCheckout(dir string) (top, gitDir, common string, ok bool) {
	if os.Getenv("GIT_DIR") != "" || os.Getenv("GIT_COMMON_DIR") != "" {
		return "", "", "", false
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real // git resolves cwd before its walk, so a lexical walk could find another repo
	}
	top, _ = paths.CheckoutOf(dir)
	if gitDir, ok = gitDirOf(top); !ok {
		return "", "", "", false
	}
	common = commonDirOf(gitDir)
	return top, gitDir, common, stableAncestry(gitDir, common)
}

// gitDirOf is top's .git directory, following a linked worktree's "gitdir:" file.
func gitDirOf(top string) (string, bool) {
	if top == "" {
		return "", false
	}
	dotGit := filepath.Join(top, ".git")
	st, err := os.Stat(dotGit)
	if err != nil || st.IsDir() {
		return dotGit, err == nil
	}
	raw, err := os.ReadFile(dotGit)
	ptr, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: ")
	if err != nil || !ok || ptr == "" {
		return "", false
	}
	if !filepath.IsAbs(ptr) {
		ptr = filepath.Join(top, ptr)
	}
	return ptr, true
}

// stableAncestry is false when shallow, grafts, replace refs or per-worktree/global branch config can change counts.
func stableAncestry(gitDir, common string) bool {
	for _, p := range []string{filepath.Join(common, "shallow"), filepath.Join(common, "info", "grafts"),
		filepath.Join(gitDir, "config.worktree")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			return false
		}
	}
	if names, err := os.ReadDir(filepath.Join(common, "refs", "replace")); len(names) > 0 || err != nil && !os.IsNotExist(err) {
		return false
	}
	packed, err := os.ReadFile(filepath.Join(common, "packed-refs"))
	if err != nil && !os.IsNotExist(err) || strings.Contains(string(packed), " refs/replace/") {
		return false
	}
	return !globalBranchConfig()
}

// globalBranchConfig rejects external tracking overrides; proc.RunGit scrubs GIT_CONFIG_* environment overrides.
func globalBranchConfig() bool {
	home, _ := os.UserHomeDir()
	xdg := cmp.Or(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config"))
	files := []string{filepath.Join(home, ".gitconfig"), filepath.Join(xdg, "git", "config"),
		"/etc/gitconfig", "/usr/local/etc/gitconfig", "/opt/homebrew/etc/gitconfig"}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			return true
		}
		for line := range strings.Lines(string(raw)) {
			line = strings.ToLower(strings.TrimSpace(line))
			for _, prefix := range []string{"[branch", "[remote", "[include", "[extensions"} {
				if strings.HasPrefix(line, prefix) {
					return true
				}
			}
		}
	}
	return false
}

// commonDirOf is where a linked worktree's gitdir keeps shared refs and config (its commondir file), else gitDir.
func commonDirOf(gitDir string) string {
	raw, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	common := strings.TrimSpace(string(raw))
	if filepath.IsAbs(common) {
		return common
	}
	return filepath.Join(gitDir, common)
}

// upstreamRef is branch's upstream ref from the repo config; ok=false unless the remote has the one default refspec.
func upstreamRef(common, branch string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(common, "config"))
	if err != nil {
		return "", false
	}
	cfg, ok := sectionConfig(string(raw), `[branch "`+branch+`"]`)
	ext, _ := sectionConfig(string(raw), "[extensions]")
	merge, remote := only(cfg["merge"]), only(cfg["remote"])
	name, isBranch := strings.CutPrefix(merge, "refs/heads/")
	if !ok || !isBranch || remote == "" || len(ext["worktreeconfig"]) > 0 { // config.worktree may override merge
		return "", false
	}
	if remote == "." {
		return merge, true
	}
	rc, _ := sectionConfig(string(raw), `[remote "`+remote+`"]`)
	if strings.TrimPrefix(only(rc["fetch"]), "+") != "refs/heads/*:refs/remotes/"+remote+"/*" {
		return "", false // a custom refspec may track another ref, so a cached count could hide divergence
	}
	return "refs/remotes/" + remote + "/" + name, true
}

// only is vals' single value, else "" (missing or set more than once).
func only(vals []string) string {
	if len(vals) != 1 {
		return ""
	}
	return vals[0]
}

// sectionConfig is the lower-cased keys of config's header sections; ok=false when an include may add more.
func sectionConfig(config, header string) (map[string][]string, bool) {
	keys := map[string][]string{}
	in := false
	for line := range strings.Lines(config) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "[include") {
			return nil, false
		}
		if strings.HasPrefix(line, "[") {
			in = line == header
		} else if key, val, _ := strings.Cut(line, "="); in {
			k := strings.ToLower(strings.TrimSpace(key))
			keys[k] = append(keys[k], strings.TrimSpace(val))
		}
	}
	return keys, true
}

// refSha is ref's sha from its loose file, else packed-refs; ok=false when neither holds a plain sha.
func refSha(common, ref string) (string, bool) {
	if ref == "" || !filepath.IsLocal(filepath.FromSlash(ref)) {
		return "", false
	}
	raw, err := os.ReadFile(filepath.Join(common, filepath.FromSlash(ref)))
	if err == nil {
		sha := strings.TrimSpace(string(raw))
		return sha, isOID(sha)
	}
	if !os.IsNotExist(err) {
		return "", false
	}
	packed, err := os.ReadFile(filepath.Join(common, "packed-refs"))
	if err != nil {
		return "", false
	}
	for line := range strings.Lines(string(packed)) {
		if sha, name, ok := strings.Cut(strings.TrimRight(line, "\r\n"), " "); ok && name == ref {
			return sha, isOID(sha)
		}
	}
	return "", false
}

func isOID(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && (len(s) == 40 || len(s) == 64)
}

// checkoutShared is true for the live config tree or a checkout a peer session works in.
func checkoutShared(run *hookRun) (bool, error) {
	top := run.checkout().top
	if top == "" {
		return false, nil
	}
	if top == paths.Real(paths.ConfigDir()) {
		return true, nil
	}
	peers, err := run.peers()
	return len(peers) > 0, err
}

// CleanEnd (SessionEnd) removes the repo's finished lanes (CleanFinished), one line each; never fails or blocks.
func CleanEnd(in *core.Input) core.Result {
	repo, err := worktree.ResolveRepo(in.CWD)
	if err != nil {
		return core.Result{}
	}
	results, err := worktree.CleanFinished(repo)
	if err != nil {
		return core.Result{}
	}
	var lines []string
	for _, r := range results {
		if r.Removed {
			lines = append(lines, "worktree-clean-end: removed "+r.Path)
		}
	}
	return core.Result{Stderr: strings.Join(lines, "\n")}
}
