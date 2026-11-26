package memory

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
)

// upgradeBankFiles are the root files a step rewrites; every other root file is only round-trip checked.
var upgradeBankFiles = []string{"projectContext.md", "activeContext.md", "progress.md", "decisionLog.md",
	"conventions.md", "sessionHistory.md"}

// upgradeDirs are the store folders a step rewrites; reports, handoffs and worktrees stay documents.
var upgradeDirs = []string{"archive", "notes", "feedback", "techdebt", "knowledge"}

// UpgradeOptions is one `memory upgrade`; To 0 means storeVersion.
type UpgradeOptions struct {
	DryRun    bool
	To        int
	Model     string // the code-mode fallback's model: "" opus, astra, or a catalog id; see FallbackModel
	NoResolve bool   // skip the residue fallback
}

// UpgradeSkip is a file the upgrade left untouched, with the reason.
type UpgradeSkip struct{ Path, Reason string }

// UpgradeChange is one file a step rewrote (or would): op name to count.
type UpgradeChange struct {
	Path   string
	Counts map[string]int
}

// UpgradeStepResult is one step's outcome; Commit is empty on a dry run.
type UpgradeStepResult struct {
	From    int
	Name    string
	Changed []UpgradeChange
	Commit  string
}

// UpgradeReport is the whole run; Partial reports a skipped file (exit 4).
type UpgradeReport struct {
	DryRun   bool
	From, To int
	Steps    []UpgradeStepResult
	Skipped  []UpgradeSkip
	DupIDs   []string
	Resolved string // the code-mode fallback's receipt: who answered or why it left the residue
}

// Partial is true when any file was left untouched or an id sits in two files.
func (r UpgradeReport) Partial() bool { return len(r.Skipped)+len(r.DupIDs) > 0 }

// upgradeProgress is state/harness/memory/upgrade.json: the step and files this run wrote before the commit.
type upgradeProgress struct {
	Step  int      `json:"step"`
	Files []string `json:"files"`
}

func upgradeProgressPath() string { return filepath.Join(paths.MemoryStateDir(), "upgrade.json") }

func loadUpgradeProgress() upgradeProgress {
	var p upgradeProgress
	if b, err := os.ReadFile(upgradeProgressPath()); err == nil {
		_ = json.Unmarshal(b, &p)
	}
	return p
}

func saveUpgradeProgress(p upgradeProgress) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.MemoryStateDir(), 0o700); err != nil {
		return err
	}
	return paths.AtomicWrite(upgradeProgressPath(), b, 0o600)
}

// storeStampVersion reads N from the store's "flat N" stamp.
func storeStampVersion() (int, error) {
	b, err := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), layoutFile))
	if err != nil {
		return 0, fmt.Errorf("memory store has no layout stamp: %w", err)
	}
	n, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "flat ")
	v, err := strconv.Atoi(n)
	if !ok || err != nil {
		return 0, fmt.Errorf("memory store layout %q is not \"flat N\"", strings.TrimSpace(string(b)))
	}
	return v, nil
}

// upgradeFile is one .md of the store; Rewrite marks the files a step may change.
type upgradeFile struct {
	Rel     string
	Rewrite bool
}

func isTopArchive(rel string) bool {
	rest, ok := strings.CutPrefix(rel, "archive/")
	return ok && !strings.Contains(rest, "/")
}

func upgradeRewritable(rel string) bool {
	top, rest, nested := strings.Cut(rel, "/")
	if !nested {
		return slices.Contains(upgradeBankFiles, top)
	}
	if top == "archive" {
		_, ok := ArchiveSource(rest)
		return ok && !strings.Contains(rest, "/")
	}
	return slices.Contains(upgradeDirs, top)
}

// upgradeInventory round-trips every .md of the store; failures are skipped, archives with no source key listed.
func upgradeInventory(read func(string) ([]byte, error)) (files []upgradeFile, skips []UpgradeSkip, err error) {
	root := paths.MemoryStoreDir()
	err = filepath.WalkDir(root, func(p string, de fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if de.IsDir() && de.Name() == ".git" {
			return filepath.SkipDir
		}
		if de.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		b, rerr := read(rel)
		switch {
		case rerr != nil:
			return rerr
		case !bytes.Equal(ParseDoc(rel, b).Render(), b):
			skips = append(skips, UpgradeSkip{rel, "round-trip differs"})
		case isTopArchive(rel) && !upgradeRewritable(rel):
			skips = append(skips, UpgradeSkip{rel, "no source key"})
		default:
			files = append(files, upgradeFile{rel, upgradeRewritable(rel)})
		}
		return nil
	})
	return files, skips, err
}

// requireCleanStore refuses a dirty memory repo unless every dirty path is this run's own recorded work.
func requireCleanStore(prog upgradeProgress) error {
	repo := StoreRepo()
	if !repo.Exists() {
		return errors.New("memory store is not a git repo: refusing to upgrade")
	}
	st, err := repo.Status()
	if err != nil {
		return err
	}
	var dirty []string
	for _, e := range st {
		if e.Path != layoutFile && !slices.Contains(prog.Files, e.Path) {
			dirty = append(dirty, e.Path)
		}
	}
	if len(dirty) > 0 {
		return fmt.Errorf("memory store has uncommitted changes (%s): commit or revert them first", strings.Join(dirty[:min(len(dirty), 5)], ", "))
	}
	return nil
}

// upgrader carries one run: the dry-run overlay, the global id map and the clock.
type upgrader struct {
	dry     bool
	overlay map[string][]byte
	ids     map[string]bool
	now     time.Time
	prog    upgradeProgress
}

func (u *upgrader) read(rel string) ([]byte, error) {
	if b, ok := u.overlay[rel]; ok {
		return b, nil
	}
	return os.ReadFile(filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(rel)))
}

// claimIDs registers the ids a file gained; a minted id already used elsewhere fails the file.
func (u *upgrader) claimIDs(old, out Doc) error {
	had := map[string]bool{}
	for _, e := range old.Entries {
		had[e.ID] = true
	}
	for _, e := range out.Entries {
		if e.ID == "" || had[e.ID] {
			continue
		}
		if u.ids[e.ID] {
			return fmt.Errorf("minted id %s is already in the store", e.ID)
		}
		u.ids[e.ID] = true
	}
	return nil
}

// idOwners maps every entry id of the rewritable files to the files holding it, and lists ids in two files.
func (u *upgrader) idOwners(files []upgradeFile) (dups []string, err error) {
	owners := map[string][]string{}
	for _, f := range files {
		if !f.Rewrite {
			continue
		}
		b, rerr := u.read(f.Rel)
		if rerr != nil {
			return nil, rerr
		}
		for _, e := range ParseDoc(f.Rel, b).Entries {
			if e.ID != "" && !slices.Contains(owners[e.ID], f.Rel) {
				owners[e.ID] = append(owners[e.ID], f.Rel)
			}
		}
	}
	u.ids = map[string]bool{}
	for id, fs := range owners {
		u.ids[id] = true
		if len(fs) > 1 {
			dups = append(dups, id+": "+strings.Join(fs, ", "))
		}
	}
	slices.Sort(dups)
	return dups, nil
}

// stepStable is true when the step's plan over b changes nothing (a plan op that applies to nothing counts as stable).
func stepStable(step upgradeStep, rel string, b []byte) bool {
	d := ParseDoc(rel, b)
	plan := step.Plan(d)
	if len(plan.Ops) == 0 {
		return true
	}
	out, _ := applyUpgrade(d, plan, Env{Now: time.Now()})
	return bytes.Equal(out.Render(), b)
}

func bulletCount(b []byte) int {
	n := 0
	for ln := range strings.SplitSeq(string(b), "\n") {
		if strings.HasPrefix(ln, "- ") {
			n++
		}
	}
	return n
}

// planFile runs one step over one file in memory: its new bytes and op counts; nil bytes mean no change.
func (u *upgrader) planFile(step upgradeStep, rel string) (out []byte, counts map[string]int, err error) {
	data, err := u.read(rel)
	if err != nil {
		return nil, nil, err
	}
	d := ParseDoc(rel, data)
	plan := step.Plan(d)
	if len(plan.Ops) == 0 {
		return nil, nil, nil
	}
	nd, res := applyUpgrade(d, plan, Env{Now: u.now, NewID: contentIDs(d)})
	counts = map[string]int{}
	for _, r := range res {
		if r.Outcome != DreamOK {
			return nil, nil, fmt.Errorf("op %s %s: %s", r.Op.Op, r.Outcome, r.Reason)
		}
		counts[r.Op.Op]++
	}
	if err := u.claimIDs(d, ParseDoc(rel, nd.Render())); err != nil {
		return nil, nil, err
	}
	out = nd.Render()
	switch {
	case bytes.Equal(out, data):
		return nil, nil, nil
	case !bytes.Equal(ParseDoc(rel, out).Render(), out):
		return nil, nil, errors.New("postcheck: round-trip differs")
	case !stepStable(step, rel, out):
		return nil, nil, errors.New("postcheck: step is not idempotent here")
	case bulletCount(out) < bulletCount(data):
		return nil, nil, errors.New("postcheck: bullet count dropped")
	}
	return out, counts, nil
}

// upgradeOne plans a file and, off a dry run, writes it under the store lock; it records the file first for resume.
func (u *upgrader) upgradeOne(step upgradeStep, rel string) (UpgradeChange, bool, error) {
	var ch UpgradeChange
	var changed bool
	run := func() error {
		out, counts, err := u.planFile(step, rel)
		if err != nil || out == nil {
			return err
		}
		ch, changed = UpgradeChange{rel, counts}, true
		if u.dry {
			u.overlay[rel] = out
			return nil
		}
		if !slices.Contains(u.prog.Files, rel) {
			u.prog.Files = append(u.prog.Files, rel)
		}
		if err := saveUpgradeProgress(u.prog); err != nil {
			return err
		}
		return paths.AtomicWrite(filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(rel)), out, 0o644)
	}
	if u.dry {
		return ch, changed, run()
	}
	err := WithBankLock(BankLockWait, run)
	return ch, changed, err
}

// runStep upgrades every rewritable file by one step and, off a dry run, stamps and commits once.
func (u *upgrader) runStep(step upgradeStep, files []upgradeFile, rep *UpgradeReport) error {
	if u.prog.Step != step.From {
		u.prog = upgradeProgress{Step: step.From}
	}
	sr := UpgradeStepResult{From: step.From, Name: step.Name}
	for _, f := range files {
		if !f.Rewrite {
			continue
		}
		ch, changed, err := u.upgradeOne(step, f.Rel)
		if err != nil {
			rep.Skipped = append(rep.Skipped, UpgradeSkip{f.Rel, err.Error()})
			continue
		}
		if changed {
			sr.Changed = append(sr.Changed, ch)
		}
	}
	if !u.dry {
		sha, err := u.commitStep(step)
		if err != nil {
			return err
		}
		sr.Commit = sha
	}
	rep.Steps = append(rep.Steps, sr)
	return nil
}

// commitStep bumps the stamp and makes the step's one commit, scoped to the files it wrote.
func (u *upgrader) commitStep(step upgradeStep) (sha string, err error) {
	err = WithBankLock(BankLockWait, func() error {
		stamp := filepath.Join(paths.MemoryStoreDir(), layoutFile)
		if err := paths.AtomicWrite(stamp, []byte("flat "+strconv.Itoa(step.From+1)+"\n"), 0o644); err != nil {
			return err
		}
		forgetLayout()
		scope := append(slices.Clone(u.prog.Files), layoutFile)
		subject := fmt.Sprintf("upgrade: flat %d -> %d (%s)", step.From, step.From+1, step.Name)
		if err := commitStore(subject, scope...); err != nil {
			return err
		}
		sha, _ = StoreRepo().headSHA()
		return nil
	})
	if err != nil {
		return "", err
	}
	u.prog = upgradeProgress{}
	return sha, os.Remove(upgradeProgressPath())
}

func upgradeStepFrom(v int) (upgradeStep, bool) {
	i := slices.IndexFunc(upgradeSteps, func(s upgradeStep) bool { return s.From == v })
	if i < 0 {
		return upgradeStep{}, false
	}
	return upgradeSteps[i], true
}

// upgradeTarget validates the stamp against opt.To and returns the range to run.
func upgradeTarget(o UpgradeOptions) (from, to int, err error) {
	if from, err = storeStampVersion(); err != nil {
		return 0, 0, err
	}
	if from > storeVersion {
		return 0, 0, fmt.Errorf("store is flat %d, this binary writes flat %d: install a newer claude-code", from, storeVersion)
	}
	to = cmp.Or(o.To, storeVersion)
	if to < from || to > storeVersion {
		return 0, 0, fmt.Errorf("--to %d: must be between the store's flat %d and flat %d", to, from, storeVersion)
	}
	return from, to, nil
}

// MemoryUpgrade runs every step from the store's stamp to opt.To; a dry run writes nothing.
func MemoryUpgrade(o UpgradeOptions) (UpgradeReport, error) {
	rep := UpgradeReport{DryRun: o.DryRun}
	var err error
	// By design a store already at the target never resolves residue: no paid model on every run.
	if rep.From, rep.To, err = upgradeTarget(o); err != nil || rep.From == rep.To {
		return rep, err
	}
	u := &upgrader{dry: o.DryRun, overlay: map[string][]byte{}, now: time.Now(), prog: loadUpgradeProgress()}
	if !o.DryRun {
		if err := requireCleanStore(u.prog); err != nil {
			return rep, err
		}
	}
	files, skips, err := upgradeInventory(u.read)
	if err != nil {
		return rep, err
	}
	rep.Skipped = slices.Clone(skips)
	if rep.DupIDs, err = u.idOwners(files); err != nil {
		return rep, err
	}
	for v := rep.From; v < rep.To; v++ {
		step, ok := upgradeStepFrom(v)
		if !ok {
			return rep, fmt.Errorf("no upgrade step from flat %d", v)
		}
		if err := u.runStep(step, files, &rep); err != nil {
			return rep, err
		}
	}
	return u.finish(o, files, skips, rep)
}

// finish runs the postcheck, then the one-shot code-mode fallback over what the steps left unplaced.
func (u *upgrader) finish(o UpgradeOptions, files []upgradeFile, skips []UpgradeSkip, rep UpgradeReport) (UpgradeReport, error) {
	if err := u.postcheck(files, skips, rep); err != nil {
		return rep, err
	}
	rep.Resolved = resolveResidue(o, rep)
	return rep, nil
}

// StoreCopy is a throwaway git clone of the store for the fallback to read; a var so tests need no repo.
var StoreCopy = func(ctx context.Context) (string, func(), error) {
	dir, err := os.MkdirTemp("", "memory-fallback-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if out, cerr := exec.CommandContext(ctx, "git", "clone", "--quiet", "--local", paths.MemoryStoreDir(), dir).CombinedOutput(); cerr != nil {
		cleanup()
		return "", nil, fmt.Errorf("git clone of the store: %v: %s", cerr, strings.TrimSpace(string(out)))
	}
	return dir, cleanup, nil
}

// residueTimeout bounds the clone and the one code-mode dispatch.
const residueTimeout = 9 * time.Minute

// CodeModeRun is the one code-mode Pi dispatch behind the typed-write and residue fallbacks; ask registers it.
var CodeModeRun func(ctx context.Context, prompt, workdir, schema, model string) (answer, who string, err error)

// CodeFallback allows one CodeModeRun per job: a second call fails without dispatching.
type CodeFallback struct {
	Model string // "" is opus
	used  atomic.Bool
	last  atomic.Pointer[string] // how the one dispatch ended
}

type fallbackKey struct{}

// WithCodeFallback carries the job's one fallback so the runner and the typed writer share it.
func WithCodeFallback(ctx context.Context, f *CodeFallback) context.Context {
	return context.WithValue(ctx, fallbackKey{}, f)
}

// JobCodeFallback is the fallback WithCodeFallback set, or nil.
func JobCodeFallback(ctx context.Context) *CodeFallback {
	f, _ := ctx.Value(fallbackKey{}).(*CodeFallback)
	return f
}

// FallbackModel maps the fallback's model name to a catalog id or alias; an unknown id is an error.
func FallbackModel(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = models.Opus
	}
	if !models.IsKnownSpelling(name) {
		return "", fmt.Errorf("model %q is not in the catalog", name)
	}
	return name, nil
}

// outcome is how the one dispatch ended, or that it is still running.
func (f *CodeFallback) outcome() string {
	if p := f.last.Load(); p != nil {
		return *p
	}
	return "still running"
}

// Used reports whether the job's one dispatch was taken.
func (f *CodeFallback) Used() bool { return f.used.Load() }

// Run asks the code-mode agent, over a git copy of the store, for a typed Batch; the workdir argument is ignored.
func (f *CodeFallback) Run(ctx context.Context, prompt, workdir, schema string) (answer, who string, err error) {
	if CodeModeRun == nil {
		return "", "", errors.New("code-mode fallback unavailable: this process registered no agent (a hook never dispatches one)")
	}
	if !f.used.CompareAndSwap(false, true) {
		return "", "", fmt.Errorf("code-mode fallback already used for this job (%s)", f.outcome())
	}
	dir, cleanup, err := StoreCopy(ctx)
	if err != nil {
		return "", "", err
	}
	defer cleanup()
	prompt = strings.ReplaceAll(prompt, paths.MemoryStoreDir(), dir) + "\n\nWrite nothing and run no memory command: answer with the typed batch only."
	answer, who, err = CodeModeRun(ctx, prompt, dir, schema, f.Model)
	if err == nil {
		if _, derr := decodeBatch(answer); derr != nil {
			err = fmt.Errorf("code-mode answer rejected: %w", derr)
		}
	}
	out := who + " answered"
	if err != nil {
		out, answer = who+" failed: "+err.Error(), ""
	}
	f.last.Store(&out)
	return answer, who, err
}

// Retry gives the one fallback a failed first answer and applies the corrected Batch; both failures are named.
func (f *CodeFallback) Retry(ctx context.Context, prompt, workdir, schema, first string, failed error, apply func(answer string) error) error {
	prompt += assets.MemoryRetryNote(failed.Error())
	answer, who, err := f.Run(ctx, prompt, workdir, schema)
	if err == nil {
		if err = apply(answer); err == nil {
			return nil
		}
	}
	return fmt.Errorf("typed write failed on %s (%v); code-mode fallback on %s failed: %w", cmp.Or(first, "the first model"), failed, cmp.Or(who, f.Model, "opus"), err)
}

// ValidateBatchAnswer reports why a typed answer is unusable; nil means it decodes and validates.
func ValidateBatchAnswer(answer string) error {
	_, err := decodeBatch(answer)
	return err
}

// residuePrompt lists what the pure steps could not place; the agent answers with a typed Batch only.
func residuePrompt(skipped []UpgradeSkip, dups []string) string {
	var sb strings.Builder
	for _, s := range skipped {
		fmt.Fprintf(&sb, "- %s: %s\n", s.Path, s.Reason)
	}
	for _, d := range dups {
		fmt.Fprintf(&sb, "- duplicate id %s\n", d)
	}
	return assets.MemoryResiduePrompt(sb.String())
}

// resolveResidue sends the residue to one code-mode agent over a store copy; a failed answer leaves the store as is.
func resolveResidue(o UpgradeOptions, rep UpgradeReport) string {
	if len(rep.Skipped)+len(rep.DupIDs) == 0 || o.NoResolve || CodeModeRun == nil {
		return ""
	}
	model, err := FallbackModel(o.Model)
	if err != nil {
		return "residue fallback skipped: " + err.Error()
	}
	prompt := residuePrompt(rep.Skipped, rep.DupIDs)
	if o.DryRun {
		return "would send to one code-mode agent on model " + model + ":\n" + prompt
	}
	ctx, cancel := context.WithTimeout(context.Background(), residueTimeout)
	defer cancel()
	_, base, err := newBankStore(false).Load() // versions of the snapshot the agent reads, not of what it leaves behind
	if err != nil {
		return "residue fallback failed: " + err.Error()
	}
	answer, who, err := (&CodeFallback{Model: model}).Run(ctx, prompt, "", BatchSchema())
	if err != nil {
		return "residue fallback failed (" + cmp.Or(who, model) + "): " + err.Error()
	}
	// By design any valid batch is applied and the residue stays listed (exit 4) so the owner sees what remains.
	if err := applyResidue(answer, base); err != nil {
		if _, after := errors.AsType[*AfterCommitError](err); after {
			return "batch applied by " + who + " (model " + model + ") but " + err.Error()
		}
		return "residue fallback failed (" + cmp.Or(who, model) + "): " + err.Error()
	}
	return "batch applied by " + who + " (model " + model + "); the residue stays listed"
}

// applyResidue applies the fallback's validated Batch all-or-nothing against the store's current versions.
func applyResidue(answer string, base Versions) error {
	b, err := decodeBatch(answer)
	if err != nil {
		return err
	}
	_, err = newBankStore(false).Apply(b, base, time.Now())
	return err
}

// postcheck round-trips the store again and plans every step over every placed file: all must come back empty.
func (u *upgrader) postcheck(files []upgradeFile, pre []UpgradeSkip, rep UpgradeReport) error {
	_, skips, err := upgradeInventory(u.read)
	if err != nil {
		return err
	}
	for _, s := range skips {
		if !slices.Contains(pre, s) {
			return fmt.Errorf("postcheck: round-trip differs after upgrade: %s", s.Path)
		}
	}
	for _, f := range files {
		if !f.Rewrite || slices.ContainsFunc(rep.Skipped, func(s UpgradeSkip) bool { return s.Path == f.Rel }) {
			continue
		}
		b, err := u.read(f.Rel)
		if err != nil {
			return err
		}
		for _, s := range rep.Steps {
			if st, _ := upgradeStepFrom(s.From); !stepStable(st, f.Rel, b) {
				return fmt.Errorf("postcheck: %s still has %s work", f.Rel, s.Name)
			}
		}
	}
	return nil
}

// opLabels names step ops in the per-file summary.
var opLabels = map[string]string{"merge": "merged", "identify": "identified", "date": "dated", "close": "closed"}

// Print writes the per-file summary, the skipped files, duplicate ids and the totals.
func (r UpgradeReport) Print(w io.Writer) {
	if r.From == r.To {
		fmt.Fprintf(w, "memory upgrade: nothing to do (store is flat %d)\n", r.From)
		return
	}
	mode := ""
	if r.DryRun {
		mode = " (dry run: nothing written)"
	}
	fmt.Fprintf(w, "memory upgrade: flat %d -> %d%s\n", r.From, r.To, mode)
	files, totals := 0, map[string]int{}
	for _, s := range r.Steps {
		fmt.Fprintf(w, "step flat %d -> %d (%s)", s.From, s.From+1, s.Name)
		if s.Commit != "" {
			fmt.Fprintf(w, ": commit %.9s", s.Commit)
		}
		fmt.Fprintln(w)
		for _, c := range s.Changed {
			files++
			fmt.Fprintf(w, "  %s: %s\n", c.Path, countsText(c.Counts, totals))
		}
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(w, "skipped %s: %s\n", s.Path, s.Reason)
	}
	for _, d := range r.DupIDs {
		fmt.Fprintf(w, "duplicate id %s\n", d)
	}
	if r.Resolved != "" {
		fmt.Fprintf(w, "residue: %s\n", r.Resolved)
	}
	fmt.Fprintf(w, "totals: %d file(s) changed (%s), %d skipped\n", files, countsText(totals, nil), len(r.Skipped))
}

// countsText renders counts as "merged 2, dated 1" in a stable order and adds them into into when given.
func countsText(counts, into map[string]int) string {
	var parts []string
	for _, op := range slices.Sorted(maps.Keys(counts)) {
		label := cmp.Or(opLabels[op], op)
		parts = append(parts, fmt.Sprintf("%s %d", label, counts[op]))
		if into != nil {
			into[op] += counts[op]
		}
	}
	return cmp.Or(strings.Join(parts, ", "), "none")
}
