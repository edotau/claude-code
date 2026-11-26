package plans

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
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
		if !open(t) || t.ID == "" {
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
		if open(byID[id]) {
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

// repoClaims is live with each claim on a file clean in its checkout released: only a live edit claims a file.
func repoClaims(ctx context.Context, root string, live map[string][]string) (map[string][]string, error) {
	return dirtyClaims(ctx, root, live), nil
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
	overlay := struct {
		Agent string `json:"agent"`
		Tools bool   `json:"tools,omitempty"`
	}{agent, agent != "pi"}
	obj, _ := json.Marshal(overlay) // a string and a bool always marshal
	words = append(words, "--background", "--overlay", string(obj))
	return textutil.ShellJoin(words) + " - < " + textutil.QuotePath(r.Brief)
}
