package plans

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// NeedsBrief is a ready row's Brief when its line names no existing brief: file.
const NeedsBrief = "needs a brief"

// ReadyInput is what Ready reads besides the plans; plans cannot import worktree or agent, so callers pass them.
type ReadyInput struct {
	Root   string                     // main checkout whose plans home is read and whose paths owns: entries name
	Plans  []string                   // plan files; nil reads Root's active plans
	Slots  int                        // free dispatch slots
	Claims func() map[string][]string // absolute path -> live claimants; called once, only when a task needs it
}

// ReadyTask is one open task: where it lives, what a leaf needs, and why it cannot launch when it cannot.
type ReadyTask struct {
	Plan    string   `json:"plan"`
	ID      string   `json:"id"`
	Leaf    string   `json:"leaf,omitempty"`
	Owns    []string `json:"owns"`
	Verify  string   `json:"verify,omitempty"`
	Brief   string   `json:"brief"`             // a brief file path, or NeedsBrief
	Command string   `json:"command,omitempty"` // the next command to run for a ready task
	Reason  string   `json:"reason,omitempty"`  // why a not-ready task waits
}

// ReadyReport is `plan ready`: the free slots, the launchable tasks and the open ones that wait.
type ReadyReport struct {
	Slots    int         `json:"slots"`
	Ready    []ReadyTask `json:"ready"`
	NotReady []ReadyTask `json:"not_ready"`
}

var (
	codeSpan  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("`[^`]*`") })
	briefAttr = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\bbrief:\s*(\S+)`) })
	ownerGate = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\bowner(?:'s)?\s+(?:decision|yes|approval|answer)\b`)
	})
	carriedCLI = []string{"codex", "copilot", "gemini", "opencode"}
)

// Ready sorts every open task of the plans into ready and not ready; it reads plan files and in.Claims only.
func Ready(in ReadyInput) (ReadyReport, error) {
	rep := ReadyReport{Slots: max(in.Slots, 0), Ready: []ReadyTask{}, NotReady: []ReadyTask{}}
	files := in.Plans
	if files == nil {
		files = activePlans(Home(in.Root))
	}
	var rows []ReadyTask
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return rep, err
		}
		rows = append(rows, readyTasks(in.Root, parseBytes(f, b), strings.Split(string(b), "\n"))...)
	}
	claimRows(in, rows)
	for _, r := range rows {
		if r.Reason == "" {
			r.Command = dispatchCommand(r)
			rep.Ready = append(rep.Ready, r)
		} else {
			rep.NotReady = append(rep.NotReady, r)
		}
	}
	return rep, nil
}

// claimRows sets a claim reason on each row its line left launchable; git reads only the claims those rows touch.
func claimRows(in ReadyInput, rows []ReadyTask) {
	var owns []string
	for _, r := range rows {
		if r.Reason == "" {
			owns = append(owns, slices.DeleteFunc(slices.Clone(r.Owns), func(o string) bool { return strings.EqualFold(o, "none") })...)
		}
	}
	if len(owns) == 0 {
		return
	}
	for i, o := range owns {
		owns[i] = relPath(in.Root, o)
	}
	claims, err := repoClaims(context.Background(), in.Root, touching(in, owns))
	held := relClaims(in.Root, claims)
	keys := slices.Sorted(maps.Keys(held))
	for i := range rows {
		if rows[i].Reason == "" && err != nil {
			rows[i].Reason = "lane claims unreadable: " + err.Error() // an unread lane may hold the files
		} else if rows[i].Reason == "" {
			rows[i].Reason = claimReason(in.Root, rows[i].Owns, held, keys)
		}
	}
}

// touching is in's live claims on a file one of owns (repo-relative) names; the rest need no git read.
func touching(in ReadyInput, owns []string) map[string][]string {
	live := map[string][]string{}
	if in.Claims == nil {
		return live
	}
	for p, who := range in.Claims() {
		if rel := relPath(in.Root, p); slices.ContainsFunc(owns, func(o string) bool { return OwnsOverlap(o, rel) }) {
			live[p] = who
		}
	}
	return live
}

// ReadyNotice is one context line naming each ready task's command; "" when none is ready, no slot free, or on error.
func ReadyNotice(in ReadyInput) string {
	if in.Slots <= 0 {
		return ""
	}
	rep, err := Ready(in)
	if err != nil {
		return ""
	}
	return noticeOf(rep, in.Slots, nil)
}

// noticeOf is rep's ready notice with the tasks keyed in skip ("<plan-slug> <id>", already sent) left out.
func noticeOf(rep ReadyReport, slots int, skip []string) string {
	var parts []string
	for _, t := range rep.Ready {
		key := strings.TrimSuffix(filepath.Base(t.Plan), ".md") + " " + t.ID
		if !slices.Contains(skip, key) {
			parts = append(parts, key+": "+t.Command)
		}
	}
	if len(parts) == 0 || slots <= 0 {
		return ""
	}
	return "plans: " + plural(len(parts), "task") + " ready, " + plural(slots, "slot") + " free: " + strings.Join(parts, "; ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// activePlans is home's dated plans no older than recentDays, oldest first, as OpenPlans picks them.
func activePlans(home string) []string {
	entries, _ := os.ReadDir(home) // no plans home is no plans
	cutoff := now().AddDate(0, 0, -recentDays).Format(time.DateOnly)
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && datedName().MatchString(e.Name()) && e.Name()[:10] >= cutoff {
			out = append(out, filepath.Join(home, e.Name()))
		}
	}
	return out
}

// readyTasks types plan's open tasks; an id-less task cannot be named in a dispatch, so it is skipped.
func readyTasks(root string, plan Plan, lines []string) []ReadyTask {
	byID := make(map[string]Task, len(plan.Tasks))
	for _, t := range plan.Tasks {
		byID[taskKey(t)] = t
	}
	var out []ReadyTask
	for _, t := range plan.Tasks {
		if t.closed() || t.ID == "" {
			continue
		}
		// a code span quoting "Blocked by" is prose about the key, not the key
		title := codeSpan().ReplaceAllStringFunc(ownLine(lines, t), func(s string) string { return strings.ReplaceAll(s, "Blocked by", "") })
		brief := ""
		if m := briefAttr().FindStringSubmatchIndex(title); m != nil {
			brief, title = title[m[2]:m[3]], title[:m[0]]+title[m[1]:]
		}
		attrs := waveAttributes(title)
		_, _, owns, _, _, _ := parseTaskText("owns: " + attrs["owns"])
		r := ReadyTask{Plan: plan.Path, ID: taskKey(t), Leaf: firstWord(cmp.Or(attrs["agent"], attrs["leaf"])),
			Owns: ownEntries(owns, nil), Verify: attrs["verify"], Brief: NeedsBrief}
		if brief = paths.ExpandHome(brief); brief != "" && !filepath.IsAbs(brief) {
			brief = filepath.Join(root, brief) // a relative brief: path names a file of the plan's repo
		}
		if _, err := os.Stat(brief); brief != "" && err == nil {
			r.Brief = brief
		}
		r.Reason = cmp.Or(blockReason(t, attrs["blocked by"], byID), launchReason(r, t.Claim))
		out = append(out, r)
	}
	return out
}

// ownLine is t's title without its nested bullets: a "- NOTE" under the task would run on into its last attribute.
func ownLine(lines []string, t Task) string {
	_, _, text, ok := checkItem(lines[t.Start])
	if !ok {
		return t.Title
	}
	end := t.Start + 1
	for end <= t.End && !listLine().MatchString(lines[end]) {
		end++
	}
	_, title, _, _, _, _ := parseTaskText(strings.Join(append([]string{text}, lines[t.Start+1:end]...), " "))
	return title
}

// launchReason is why no new leaf can take r: a plan claim, no leaf: on its line, or a main-session step.
func launchReason(r ReadyTask, claim string) string {
	switch {
	case claim != "":
		return "claimed: " + claim
	case r.Leaf == "":
		return "no leaf: on the line"
	case r.Leaf == "in-context" || strings.HasSuffix(r.ID, ".V"):
		return "in-context: the main session's own step"
	}
	return ""
}

// taskKey is a wave task's canonical "Phase N.M", the form blockers names; any other id stays as parsed.
func taskKey(t Task) string {
	if m := waveID().FindStringSubmatch(t.Title); m != nil {
		return "Phase " + m[1] + "." + strings.ToUpper(m[2])
	}
	return t.ID
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

// blockReason is why t's blockers hold it back, "" when none does; prose and owner gates are never guessed satisfied.
func blockReason(t Task, blocked string, byID map[string]Task) string {
	ids, ok := blockerIDs(blocked, byID)
	if !ok {
		return "prose blocker: " + strings.Trim(blocked, " .")
	}
	if m := ownerGate().FindString(t.Title); m != "" {
		return "waits on the owner: " + m
	}
	for _, id := range ids {
		if !byID[id].closed() {
			return "blocked by " + id + " (open)"
		}
	}
	return ""
}

// blockerIDs resolves each word of a Blocked by value to a task of byID, notes in parentheses dropped; false on prose.
func blockerIDs(val string, byID map[string]Task) ([]string, bool) {
	if v := strings.Trim(val, " .—–-"); v == "" || strings.EqualFold(v, "none") {
		return nil, true
	}
	var ids []string
	words := strings.FieldsFunc(ownParen().ReplaceAllString(val, " "), func(r rune) bool { return strings.ContainsRune(" ,;&", r) })
	if len(words) == 0 {
		return nil, false // only a parenthetical: prose
	}
	for _, w := range words {
		w = strings.TrimRight(w, ".")
		if strings.EqualFold(w, "and") || strings.EqualFold(w, "phase") {
			continue
		}
		id := ""
		for _, c := range []string{w, "Phase " + w, "Phase " + strings.ToUpper(w)} {
			if _, ok := byID[c]; ok {
				id = c
				break
			}
		}
		if id == "" {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

// claimReason names the first claim, in keys order, on one of owns; "" when none is held.
func claimReason(root string, owns []string, held map[string][]string, keys []string) string {
	for _, own := range owns {
		own = relPath(root, own)
		for _, p := range keys {
			if !strings.EqualFold(own, "none") && OwnsOverlap(own, p) {
				return "owns claimed: " + p + " by " + strings.Join(held[p], ", ")
			}
		}
	}
	return ""
}

// relClaims keys claims by repo-relative path; a lane's path drops its ~/worktrees/<repo>/<slug>/ prefix.
func relClaims(root string, claims map[string][]string) map[string][]string {
	out := map[string][]string{}
	for p, who := range claims {
		rel := relPath(root, p)
		out[rel] = append(out[rel], who...)
	}
	for p, who := range out { // the main and a lane copy of one file name the same claimant twice
		slices.Sort(who)
		out[p] = slices.Compact(who)
	}
	return out
}

// relPath is p relative to root or to the lane holding it, slashes forward; any other path stays absolute.
func relPath(root, p string) string {
	_, rel := checkoutPath(root, p)
	return rel
}

// checkoutPath splits p into the checkout holding it (root or one of its lanes; "" for neither) and the path in it.
func checkoutPath(root, p string) (string, string) {
	p = path.Clean(filepath.ToSlash(paths.ExpandHome(p)))
	if !path.IsAbs(p) || root == "" {
		return "", p
	}
	for _, base := range []string{root, paths.Canon(root)} {
		if rel, ok := strings.CutPrefix(p, filepath.ToSlash(base)+"/"); ok {
			return root, rel
		}
	}
	home, _ := os.UserHomeDir() // LaneRoot canonicalizes home; a claim may name the symlinked form
	for _, lanes := range []string{paths.LaneRoot(root), filepath.Join(home, "worktrees", filepath.Base(paths.LaneRoot(root)))} {
		lanes = filepath.ToSlash(lanes)
		if rest, ok := strings.CutPrefix(p, lanes+"/"); ok {
			if slug, rel, ok := strings.Cut(rest, "/"); ok {
				return lanes + "/" + slug, rel
			}
		}
	}
	return "", p
}

// repoClaims is live with each claim on a file clean in its checkout released, plus every unlanded lane's files.
func repoClaims(ctx context.Context, root string, live map[string][]string) (map[string][]string, error) {
	type result struct {
		owns map[string][]string
		err  error
	}
	lanes := make(chan result, 1)
	go func() {
		owns, err := laneOwns(ctx, root)
		lanes <- result{owns, err}
	}()
	out := dirtyClaims(ctx, root, live)
	r := <-lanes
	for p, who := range r.owns {
		out[p] = append(out[p], who...)
	}
	return out, r.err
}

// laneFilesCache holds root's lane commit files and net claims, one file per plans home; stale keys are pruned.
func laneFilesCache(root string) string {
	return filepath.Join(paths.StateDir(), "plans", "lane-files-"+Name(root)+".json")
}

// laneCache: commit -> files; lane key -> net-changed file -> tip blob; "<tip>@<head>" -> files off HEAD.
type laneCache struct {
	Commits map[string][]string          `json:"commits"`
	Tips    map[string]map[string]string `json:"tips"`
	Claims  map[string][]string          `json:"claims"`
}

// lane is one worktree-* branch with commits past HEAD: its tip, those commits, and its bases on HEAD's side.
type lane struct {
	name, tip      string
	commits, bases []string
}

// key names l's net change: its tip and bases, so a lane whose earlier commits landed is computed afresh.
func (l lane) key() string { return l.tip + ":" + strings.Join(l.bases, ",") }

// laneOwns maps each file an unlanded lane still changes (repo-relative) to "lane <slug>".
func laneOwns(ctx context.Context, root string) (map[string][]string, error) {
	head, lanes, err := readLanes(ctx, root)
	if err != nil {
		return nil, err
	}
	c := laneCache{}
	if b, err := os.ReadFile(laneFilesCache(root)); err == nil {
		_ = json.Unmarshal(b, &c) // a torn or older cache is refilled from git
	}
	next, pending := carryCache(c, lanes, head)
	if len(pending) > 0 {
		if err := netClaims(ctx, root, head, pending, next); err != nil {
			return nil, err
		}
	}
	if len(pending) > 0 || len(next.Commits) != len(c.Commits) || len(next.Tips) != len(c.Tips) || len(next.Claims) != len(c.Claims) {
		_ = paths.WriteJSONNoSync(laneFilesCache(root), next) // a lost write costs the next run a git read
	}
	return laneClaimants(lanes, head, next.Claims), nil
}

// carryCache keeps c's entries the current lanes still use and lists the lanes with no claim for this HEAD.
func carryCache(c laneCache, lanes []lane, head string) (laneCache, []lane) {
	next := laneCache{Commits: map[string][]string{}, Tips: map[string]map[string]string{}, Claims: map[string][]string{}}
	var pending []lane
	for _, l := range lanes {
		if files, ok := c.Claims[l.tip+"@"+head]; ok {
			next.Claims[l.tip+"@"+head] = files
		} else {
			pending = append(pending, l)
		}
		if net, ok := c.Tips[l.key()]; ok {
			next.Tips[l.key()] = net
		}
		for _, sha := range l.commits {
			if files, ok := c.Commits[sha]; ok {
				next.Commits[sha] = files
			}
		}
	}
	return next, pending
}

// laneClaimants inverts each lane's claimed files into file -> "lane <slug>" names.
func laneClaimants(lanes []lane, head string, claims map[string][]string) map[string][]string {
	out := map[string][]string{}
	for _, l := range lanes {
		for _, f := range claims[l.tip+"@"+head] {
			out[f] = append(out[f], "lane "+strings.TrimPrefix(l.name, "worktree-"))
		}
	}
	return out
}

// readLanes is HEAD's sha and each worktree-* branch with commits past it, in two git calls.
func readLanes(ctx context.Context, root string) (string, []lane, error) {
	refs, err := proc.RunGit(ctx, root, "show-ref", "--head", "--heads")
	if err != nil {
		return "", nil, err
	}
	raw, err := proc.RunGit(ctx, root, "rev-list", "--parents", "--branches=worktree-*", "--not", "HEAD", "--")
	if err != nil {
		return "", nil, err
	}
	parents := map[string][]string{} // every commit past HEAD on any lane, with its parents
	for l := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			parents[f[0]] = f[1:]
		}
	}
	head := ""
	var lanes []lane
	for l := range strings.SplitSeq(refs, "\n") {
		sha, ref, _ := strings.Cut(l, " ")
		if ref == "HEAD" {
			head = sha
		} else if name := strings.TrimPrefix(ref, "refs/heads/"); strings.HasPrefix(name, "worktree-") && parents[sha] != nil {
			lanes = append(lanes, walkLane(name, sha, parents))
		}
	}
	return head, lanes, nil
}

// walkLane collects tip's commits past HEAD; a parent outside parents is reachable from HEAD, so it is a base.
func walkLane(name, tip string, parents map[string][]string) lane {
	l, seen, stack := lane{name: name, tip: tip}, map[string]bool{}, []string{tip}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[sha] {
			continue
		}
		seen[sha] = true
		if ps, ok := parents[sha]; ok {
			l.commits, stack = append(l.commits, sha), append(stack, ps...)
		} else if !slices.Contains(l.bases, sha) {
			l.bases = append(l.bases, sha)
		}
	}
	slices.Sort(l.bases)
	return l
}

// netClaims fills into.Claims for lanes: files whose lane blob matches HEAD's or a base's are released.
func netClaims(ctx context.Context, root, head string, lanes []lane, into laneCache) error {
	fresh := slices.DeleteFunc(slices.Clone(lanes), func(l lane) bool { _, ok := into.Tips[l.key()]; return ok })
	if err := tipNets(ctx, root, fresh, into); err != nil {
		return err
	}
	var files []string // each net-changed file once: HEAD's blob is the same for every lane
	for _, l := range lanes {
		files = append(files, slices.Collect(maps.Keys(into.Tips[l.key()]))...)
	}
	slices.Sort(files)
	files = slices.Compact(files)
	specs := make([]string, len(files))
	for i, f := range files {
		specs[i] = head + ":" + f
	}
	oids, err := blobIDs(ctx, root, specs)
	if err != nil {
		return err
	}
	onHead := make(map[string]string, len(files))
	for i, f := range files {
		onHead[f] = oids[i]
	}
	for _, l := range lanes {
		claimed := []string{}
		for f, oid := range into.Tips[l.key()] {
			if onHead[f] != oid {
				claimed = append(claimed, f)
			}
		}
		slices.Sort(claimed)
		into.Claims[l.tip+"@"+head] = claimed
	}
	return nil
}

// tipNets fills into.Tips for lanes: each file whose tip blob matches none of the lane's bases, with that blob.
func tipNets(ctx context.Context, root string, lanes []lane, into laneCache) error {
	var missing []string
	for _, l := range lanes {
		missing = append(missing, slices.DeleteFunc(slices.Clone(l.commits), func(s string) bool { _, ok := into.Commits[s]; return ok })...)
	}
	if err := commitFiles(ctx, root, missing, into.Commits); err != nil {
		return err
	}
	var specs []string // per lane file: the tip, then each base
	for _, l := range lanes {
		for _, f := range laneFiles(l, into.Commits) {
			for _, rev := range append([]string{l.tip}, l.bases...) {
				specs = append(specs, rev+":"+f)
			}
		}
	}
	oids, err := blobIDs(ctx, root, specs)
	if err != nil {
		return err
	}
	for _, l := range lanes {
		net, n := map[string]string{}, 1+len(l.bases)
		for _, f := range laneFiles(l, into.Commits) {
			if !slices.Contains(oids[1:n], oids[0]) {
				net[f] = oids[0]
			}
			oids = oids[n:]
		}
		into.Tips[l.key()] = net
	}
	return nil
}

// laneFiles is the sorted union of the files l's commits changed; a name with a newline cannot be asked for.
func laneFiles(l lane, commits map[string][]string) []string {
	var files []string
	for _, sha := range l.commits {
		files = append(files, commits[sha]...)
	}
	slices.Sort(files)
	return slices.DeleteFunc(slices.Compact(files), func(f string) bool { return strings.Contains(f, "\n") })
}

// blobIDs is each spec's object id ("missing" when absent) from one cat-file call, in spec order.
func blobIDs(ctx context.Context, root string, specs []string) ([]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "cat-file", "--batch-check=%(objectname)")
	cmd.Env, cmd.WaitDelay, cmd.Stdin = proc.GitEnv(), proc.ExecWaitDelay, strings.NewReader(strings.Join(specs, "\n")+"\n")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != len(specs) {
		return nil, fmt.Errorf("git cat-file: %d answers for %d specs", len(lines), len(specs))
	}
	for i, l := range lines {
		if strings.HasSuffix(l, " missing") {
			lines[i] = "missing"
		}
	}
	return lines, nil
}

// commitFiles adds each commit's changed files to into, in one git call; a failure adds none.
func commitFiles(ctx context.Context, root string, shas []string, into map[string][]string) error {
	if len(shas) == 0 {
		return nil
	}
	args := append([]string{"log", "--no-walk=unsorted", "--no-renames", "--diff-merges=first-parent", "--name-only", "--format=%x00%H"}, shas...)
	raw, err := proc.RunGit(ctx, root, append(args, "--")...)
	if err != nil {
		return err
	}
	for _, rec := range strings.Split(raw, "\x00")[1:] {
		sha, files, _ := strings.Cut(rec, "\n")
		into[sha] = slices.DeleteFunc(strings.Split(files, "\n"), func(f string) bool { return f == "" })
	}
	return nil
}

// treeDirty is which of rels (rename sources too) have an uncommitted diff in dir; nil when status fails.
func treeDirty(ctx context.Context, dir string, rels []string) map[string]bool {
	args := []string{"status", "--porcelain", "-z", "--untracked-files=all", "--"}
	for _, r := range rels {
		args = append(args, ":(literal)"+r)
	}
	raw, err := proc.RunGit(ctx, dir, args...)
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, e := range proc.StatusEntries(raw) {
		set[e.Path], set[e.From] = true, true
	}
	return set
}

// dirtyClaims drops claims on files with no uncommitted diff: one status per checkout, on its claimed paths only.
func dirtyClaims(ctx context.Context, root string, claims map[string][]string) map[string][]string {
	byTree := map[string][]string{} // checkout -> claimed paths in it
	for p := range claims {
		if dir, rel := checkoutPath(root, p); dir != "" {
			byTree[dir] = append(byTree[dir], rel)
		}
	}
	dirty := make(map[string]map[string]bool, len(byTree))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for dir, rels := range byTree {
		wg.Go(func() {
			set := treeDirty(ctx, dir, rels)
			mu.Lock()
			dirty[dir] = set
			mu.Unlock()
		})
	}
	wg.Wait()
	out := map[string][]string{}
	for p, who := range claims {
		dir, rel := checkoutPath(root, p)
		if set := dirty[dir]; dir == "" || set == nil || set[rel] {
			out[p] = who
		}
	}
	return out
}

// dispatchCommand is the next command for a ready row: the ask over its brief, else reserving one.
func dispatchCommand(r ReadyTask) string {
	agent := cmp.Or(r.Leaf, "claude")
	if r.Brief == NeedsBrief {
		return textutil.ShellJoin([]string{"claude-code", "agents", "brief", "--path", agent})
	}
	words := []string{"claude-code", "agents", "ask"}
	if slices.Contains(carriedCLI, agent) {
		words = append(words, "--carry")
	}
	words = append(words, "--background", "--agent", agent)
	if agent != "pi" {
		words = append(words, "--tools")
	}
	return textutil.ShellJoin(words) + " - < " + textutil.QuotePath(r.Brief)
}
