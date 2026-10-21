package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// ApplyResult summarizes one apply run; Complete means every non-skip item is retired with no conflict/changed step.
type ApplyResult struct {
	Manifest, Journal, Commit                      string
	Copied, Deleted, Quarantined, Skipped, Resumed int
	Conflicts, Changed                             []string
	Complete                                       bool
}

// ErrNotQuiesced: watcher claims or unresolved pending harvests exist; the Go lock cannot stop old workers.
var ErrNotQuiesced = errors.New("memory migrate: live harness claims present — stop them and diagnose before --apply")

// applyFailAfter is a test seam: >0 returns an injected error after that many successful copies.
var applyFailAfter int

var errInjected = errors.New("memory migrate: injected failure (test seam)")

// stepBlocked: a dependency (its dir's origin record) did not succeed; retried once it does.
const stepBlocked = "blocked"

// phaseSettle follows commit: typed imports get their bank metadata through the journaled transaction and a second
// commit, so retirement verifies HEAD (descending from the raw baseline) instead of the baseline alone.
const phaseSettle = "settle"

// bankMeta is each typed file's frontmatter description (plan §1a MEMORY.md contract); the name is the file stem.
var bankMeta = map[string]string{
	"projectContext.md": "orientation and do-not-regress invariants",
	"activeContext.md":  "current focus, open questions, blockers, next steps",
	"progress.md":       "what works, in progress, what's next, known issues",
	"decisionLog.md":    "dated decisions and rationale, newest first",
	"conventions.md":    "learned patterns not yet promoted to rules",
	"sessionHistory.md": "newest-first session summaries",
}

type applier struct {
	m          Manifest
	j          *migrateJournal
	jpath      string
	base       string // manifest basename
	out        io.Writer
	res        *ApplyResult
	wrote      int      // successful copies this run, for applyFailAfter
	superseded []string // other manifests' unfinished journals this run sets aside
}

// Apply consumes manifestPath under the maintenance marker and bank lock; a re-run resumes from its journal.
func Apply(manifestPath string, out io.Writer) (ApplyResult, error) {
	return apply(manifestPath, out, false)
}

// ApplySuperseding is Apply for a manifest re-made after disambiguation: another manifest's unfinished journal no
// longer blocks it and is kept whole as *.journal.superseded.json. A dest that journal already copied is listed by
// the new manifest as a copy whose dest holds equal bytes, so it verifies, commits and retires like any other.
func ApplySuperseding(manifestPath string, out io.Writer) (ApplyResult, error) {
	return apply(manifestPath, out, true)
}

func apply(manifestPath string, out io.Writer, supersede bool) (ApplyResult, error) {
	res := ApplyResult{Manifest: manifestPath}
	m, err := LoadManifest(manifestPath)
	if err != nil {
		return res, err
	}
	msum, _, err := sha256File(manifestPath)
	if err != nil {
		return res, err
	}
	base := filepath.Base(manifestPath)
	res.Journal = filepath.Join(MigrateStateDir(), base+".journal.json")
	var old []string
	if err := otherJournalIncomplete(res.Journal); err != nil {
		if !supersede {
			return res, err
		}
		if old, err = supersededJournals(res.Journal); err != nil {
			return res, err
		}
	}
	if names := liveClaims(); len(names) > 0 {
		return res, fmt.Errorf("%w: %s", ErrNotQuiesced, strings.Join(names, ", "))
	}
	j, err := loadOrStartJournal(res.Journal, manifestPath, msum)
	if err != nil {
		return res, err
	}
	a := &applier{m: m, j: j, jpath: res.Journal, base: base, out: out, res: &res, superseded: old}
	if err := SetMaintenance("memory migrate " + base); err != nil {
		return res, err
	}
	cleared := false
	defer func() {
		if !cleared {
			_ = ClearMaintenance()
		}
	}()
	err = WithBankLock(BankLockWait, func() error {
		defer func() { _ = ClearMaintenance(); cleared = true }() // marker down before the lock goes
		return a.run()
	})
	res.Commit = j.Commit
	return res, err
}

// supersededJournals lists every other unfinished journal (an unreadable one is an error, never silently replaced).
func supersededJournals(jpath string) ([]string, error) {
	var old []string
	matches, _ := filepath.Glob(filepath.Join(MigrateStateDir(), "*.journal.json"))
	for _, p := range matches {
		if p == jpath {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var j struct {
			Phase string `json:"phase"`
		}
		if err := json.Unmarshal(data, &j); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if j.Phase != journalComplete {
			old = append(old, p)
		}
	}
	return old, nil
}

// adoptSuperseded persists this journal first (the guard keeps pausing), then sets each superseded journal aside under
// a name neither the guard nor otherJournalIncomplete reads: kept whole, no longer blocking, never read as finished.
func (a *applier) adoptSuperseded() error {
	if len(a.superseded) == 0 {
		return nil
	}
	if err := a.saveJournal(); err != nil {
		return err
	}
	for _, p := range a.superseded {
		aside := strings.TrimSuffix(p, ".json") + ".superseded.json"
		if err := os.Rename(p, aside); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "supersede: %s -> %s\n", filepath.Base(p), filepath.Base(aside))
	}
	return nil
}

// run walks the journal phases copy → commit → settle → retire → complete, resuming at the recorded one; a phase
// with an unresolved item halts there, so the startup guard stays up and the next run retries it.
func (a *applier) run() error {
	if err := a.adoptSuperseded(); err != nil {
		return err
	}
	repo := StoreRepo()
	if err := repo.Ensure(); err != nil {
		return err
	}
	phases := []struct {
		name, next string
		do         func() error
	}{
		{phaseCopy, phaseCommit, a.copyPhase},
		{phaseCommit, phaseSettle, func() error { return a.commitPhase(repo) }},
		{phaseSettle, phaseRetire, a.settlePhase},
	}
	for _, p := range phases {
		if a.j.Phase != p.name {
			continue
		}
		if err := p.do(); err != nil {
			return err
		}
		if a.unresolved() {
			return a.finish()
		}
		if err := a.advance(p.next); err != nil {
			return err
		}
	}
	if a.j.Phase == phaseRetire {
		if err := a.retirePhase(repo); err != nil {
			return err
		}
	}
	return a.finish()
}

func (a *applier) advance(phase string) error {
	a.j.Phase = phase
	return a.saveJournal()
}

// unresolved reports a journal step left in conflict, changed or blocked: cutover waits until it succeeds.
func (a *applier) unresolved() bool {
	for _, st := range a.j.Steps {
		switch st.State {
		case stepConflict, stepChanged, stepBlocked:
			return true
		}
	}
	return false
}

// finish tallies; a complete journal advances to complete, anything else stays at its phase for the next run.
func (a *applier) finish() error {
	a.tally()
	if a.res.Complete {
		return a.advance(journalComplete)
	}
	fmt.Fprintf(a.out, "blocked: journal stays at phase %s until every conflict/changed/blocked entry is resolved\n",
		a.j.Phase)
	return nil
}

// action is the item's effective action: a conflict skip that kept its dest is re-evaluated on disk as a copy, since
// the diverging dest may have been removed or aligned since the dry run (a destless conflict needs a new manifest).
func (a *applier) action(it MigrateItem) string {
	if it.Action == ActionSkip && it.Dest != "" && it.Source != "" && strings.HasPrefix(it.Reason, "conflict:") {
		return ActionCopy
	}
	return it.Action
}

// unmetDep names the first dependency not journaled as done; deps precede their dependents in manifest order.
func (a *applier) unmetDep(it MigrateItem) string {
	for _, d := range it.Deps {
		switch a.j.Steps[d].State {
		case stepCopied, stepVerified, stepQuarantined, stepRetired, stepDeleted:
		default:
			return d
		}
	}
	return ""
}

func (a *applier) saveJournal() error {
	data, err := json.MarshalIndent(a.j, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(a.jpath, append(data, '\n'), 0o600)
}

// mark prints one progress line (ids, kinds, paths — never contents) and journals the step.
func (a *applier) mark(it MigrateItem, state, destSum string) error {
	fmt.Fprintf(a.out, "%-11s %s %-14s %s", state, it.ID, it.Kind, it.Source)
	if it.Dest != "" {
		fmt.Fprintf(a.out, " -> %s", it.Dest)
	}
	fmt.Fprintln(a.out)
	a.j.Steps[it.ID] = journalStep{State: state, DestSHA256: destSum, At: time.Now()}
	switch state {
	case stepConflict:
		a.res.Conflicts = append(a.res.Conflicts, it.ID+" "+it.Source)
	case stepBlocked:
		a.res.Conflicts = append(a.res.Conflicts, it.ID+" "+it.Source+" (blocked by "+a.unmetDep(it)+")")
	case stepChanged:
		a.res.Changed = append(a.res.Changed, it.ID+" "+it.Source)
	}
	return a.saveJournal()
}

// copyPhase: every copy/quarantine item in manifest order (deps precede); skips and deletes are only recorded.
func (a *applier) copyPhase() error {
	for _, it := range a.m.Items {
		var err error
		act := a.action(it)
		switch {
		case act == ActionSkip && strings.HasPrefix(it.Reason, "conflict:"):
			err = a.mark(it, stepConflict, "") // destless conflict: only a new manifest (ApplySuperseding) can disambiguate
		case act == ActionSkip:
			err = a.mark(it, stepSkipped, "")
		case act != ActionCopy && act != ActionQuarantine: // deletes (and their dir deps) wait for retire
		case a.unmetDep(it) != "":
			err = a.mark(it, stepBlocked, "")
		case it.Inline != nil: // an origin record is a vault row: no dest file to guard
			err = a.originItem(it)
		case act == ActionCopy && a.badDest(it):
			err = a.mark(it, stepConflict, a.j.Steps[it.ID].DestSHA256)
		case act == ActionCopy && it.Kind == KindAccessLog: // append-only state: merge, never conflict on growth
			err = a.mergeAccessLog(it)
		case act == ActionCopy && it.Kind == KindIndexMerge:
			err = a.mergeIndex(it)
		default:
			err = a.importItem(it)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// badDest reports (and prints) a destination that would escape its root; the item stays a conflict until it is fixed.
func (a *applier) badDest(it MigrateItem) bool {
	_, _, err := destRel(it)
	if err != nil {
		fmt.Fprintf(a.out, "reject %s: %v\n", it.ID, err)
	}
	return err != nil
}

// destRel proves it.Dest lands under its root through plain directories (validateRel's walk: a symlinked ancestor
// could carry the copy anywhere): the store (committed; rel is the store path) or, for an access log, state/access.
func destRel(it MigrateItem) (string, bool, error) {
	root, inStore := StoreDir(), true
	switch it.Kind {
	case KindAccessLog:
		root, inStore = StateDir(), false
	case KindPlan:
		root, inStore = filepath.Join(paths.ConfigDir(), "plans"), false
	}
	rel, err := filepath.Rel(root, filepath.Clean(it.Dest))
	if err != nil {
		return "", false, err
	}
	if err := (Repo{Dir: root}).validateRel(rel); err != nil {
		return "", false, err
	}
	rel = filepath.ToSlash(rel)
	if it.Kind == KindAccessLog && !strings.HasPrefix(rel, "access/") {
		return "", false, fmt.Errorf("%s: not an allowed destination under %s", it.Dest, root)
	}
	return rel, inStore, nil
}

// importItem copies a source exclusively to its dest (or the quarantine tree) after re-proving its hash; a quarantine
// journals the verified copy BEFORE unlinking its source, so a crash between resumes retirement from that hash.
func (a *applier) importItem(it MigrateItem) error {
	dest, quarantine := it.Dest, it.Action == ActionQuarantine
	if quarantine {
		dest = filepath.Join(MigrateStateDir(), "quarantine", a.base, it.ID, filepath.Base(it.Source))
	}
	if a.resumed(it, dest) {
		if !quarantine || a.j.Steps[it.ID].State == stepQuarantined {
			return nil
		}
	} else {
		if h, _, err := sha256File(it.Source); err != nil || h != it.SHA256 {
			return a.mark(it, stepChanged, "")
		}
		state, err := copyExclusive(it.Source, dest, it.SHA256)
		if err != nil {
			return err
		}
		if err := a.recordCopy(it, state, it.SHA256); err != nil || state == stepConflict || !quarantine {
			return err
		}
	}
	return a.retireSource(it, stepQuarantined)
}

// mergeAccessLog writes the union of source and dest (prefix-aware, order kept), then retires the source.
func (a *applier) mergeAccessLog(it MigrateItem) error {
	src, err := os.ReadFile(it.Source)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	dst, err := os.ReadFile(it.Dest)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	merged := mergeAppendOnly(string(dst), string(src))
	if err := os.MkdirAll(filepath.Dir(it.Dest), 0o700); err != nil {
		return err
	}
	if err := paths.AtomicWrite(it.Dest, []byte(merged), 0o600); err != nil {
		return err
	}
	if err := os.Remove(it.Source); err != nil && !os.IsNotExist(err) {
		return err
	}
	return a.mark(it, stepRetired, sha256Hex([]byte(merged)))
}

// mergeIndex appends the source MEMORY.md's missing lines to dest (outside the bank region); the source retires later.
func (a *applier) mergeIndex(it MigrateItem) error {
	if a.resumed(it, it.Dest) {
		return nil
	}
	if h, _, err := sha256File(it.Source); err != nil || h != it.SHA256 {
		return a.mark(it, stepChanged, "")
	}
	src, err := os.ReadFile(it.Source)
	if err != nil {
		return err
	}
	dst, err := os.ReadFile(it.Dest)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	merged := mergeIndexLines(string(dst), string(src))
	if err := os.MkdirAll(filepath.Dir(it.Dest), 0o755); err != nil {
		return err
	}
	if err := paths.AtomicWrite(it.Dest, []byte(merged), 0o600); err != nil {
		return err
	}
	return a.recordCopy(it, stepCopied, sha256Hex([]byte(merged)))
}

const (
	bankBegin = "<!-- bank:begin"
	bankEnd   = "<!-- bank:end -->"
)

// mergeIndexLines appends src's non-blank lines dst lacks (src's own bank region skipped), keeping dst's region intact.
func mergeIndexLines(dst, src string) string {
	have := map[string]bool{}
	for l := range strings.SplitSeq(dst, "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add []string
	inBank := false
	for l := range strings.SplitSeq(src, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, bankBegin):
			inBank = true
		case inBank:
			inBank = !strings.HasPrefix(t, bankEnd)
		case t != "" && !have[t]:
			have[t] = true
			add = append(add, l)
		}
	}
	if len(add) == 0 {
		return dst
	}
	block := strings.Join(add, "\n") + "\n"
	begin := strings.Index(dst, bankBegin)
	if begin >= 0 && !strings.Contains(dst[begin:], bankEnd) { // a torn region: stay clear of it
		return dst[:begin] + block + dst[begin:]
	}
	if dst != "" && !strings.HasSuffix(dst, "\n") {
		dst += "\n"
	}
	return dst + block
}

// mergeAppendOnly keeps dst and appends src's lines it lacks; a pure prefix relation keeps the longer log.
func mergeAppendOnly(dst, src string) string {
	switch {
	case strings.HasPrefix(src, dst):
		return src
	case strings.HasPrefix(dst, src):
		return dst
	}
	have := map[string]bool{}
	for l := range strings.SplitSeq(dst, "\n") {
		have[l] = true
	}
	var out strings.Builder
	out.WriteString(strings.TrimRight(dst, "\n"))
	for l := range strings.SplitSeq(strings.TrimRight(src, "\n"), "\n") {
		if l != "" && !have[l] {
			out.WriteString("\n" + l)
		}
	}
	return out.String() + "\n"
}

// resumed trusts a journal step only after the dest still hashes to what it recorded.
func (a *applier) resumed(it MigrateItem, dest string) bool {
	st, ok := a.j.Steps[it.ID]
	if !ok || st.DestSHA256 == "" || (st.State != stepCopied && st.State != stepVerified && st.State != stepQuarantined) {
		return false
	}
	if h, _, err := sha256File(dest); err != nil || h != st.DestSHA256 {
		return false
	}
	a.res.Resumed++
	fmt.Fprintf(a.out, "%-11s %s %-14s %s\n", "resume", it.ID, it.Kind, it.Source)
	return true
}

func (a *applier) recordCopy(it MigrateItem, state, sum string) error {
	switch state {
	case stepCopied:
		a.res.Copied++
		a.wrote++
	case stepVerified:
		a.res.Resumed++
	}
	if err := a.mark(it, state, sum); err != nil {
		return err
	}
	if applyFailAfter > 0 && state == stepCopied && a.wrote >= applyFailAfter {
		return errInjected
	}
	return nil
}

// originItem records the store dir Dest's origin row; an existing record for another origin is a conflict.
func (a *applier) originItem(it MigrateItem) error {
	var o Origin
	if err := json.Unmarshal(it.Inline, &o); err != nil {
		return fmt.Errorf("%s: inline origin: %w", it.ID, err)
	}
	if err := os.MkdirAll(it.Dest, 0o755); err != nil {
		return err
	}
	state, err := stepCopied, error(nil)
	if _, rerr := ReadOrigin(it.Dest); rerr == nil {
		state, err = stepVerified, mergeOriginAliases(it.Dest, o)
	} else {
		err = WriteOrigin(it.Dest, o)
	}
	if errors.Is(err, ErrOriginConflict) {
		return a.mark(it, stepConflict, "")
	}
	if err != nil {
		return err
	}
	return a.recordCopy(it, state, sha256Hex(it.Inline))
}

// recheckDests re-proves every unretired import's dest — still under its root, still the journaled bytes — before it
// is staged, settled or its source removed; anything else is a conflict (both copies kept, re-checked next run).
// Returns the verified store rels, so every in-store import is either in them or in conflict.
func (a *applier) recheckDests() ([]string, map[string]string, error) {
	var rels []string
	items := map[string]string{}
	for _, it := range a.m.Items {
		st := a.j.Steps[it.ID]
		if a.action(it) != ActionCopy || st.State == stepRetired || it.Inline != nil { // origin rows are not staged
			continue
		}
		rel, inStore, err := destRel(it)
		if err == nil {
			if h, _, herr := sha256File(it.Dest); herr != nil || h != st.DestSHA256 {
				err = fmt.Errorf("%s: not the journaled bytes", it.Dest)
			}
		}
		if err != nil {
			if err := a.mark(it, stepConflict, st.DestSHA256); err != nil {
				return nil, nil, err
			}
			continue
		}
		if st.State == stepConflict {
			if err := a.mark(it, stepVerified, st.DestSHA256); err != nil {
				return nil, nil, err
			}
		}
		if inStore {
			rels = append(rels, rel)
			items[it.ID] = st.DestSHA256
		}
	}
	return rels, items, nil
}

// receiptPath is this apply's durable receipt inside the store, named by the journal's start time.
func (a *applier) receiptPath() string {
	return filepath.Join(ReceiptsDir(), "migrate-"+a.j.Started.UTC().Format(migrateStamp)+".json")
}

// verifyCommit proves HEAD descends from the baseline commit (the raw imports stay in history) and holds every rel
// with the bytes recheckDests just verified on disk — the settle commit's output, or the baseline's when it had nothing.
func (a *applier) verifyCommit(repo Repo, rels []string) error {
	head, err := repo.Head()
	if err != nil {
		return err
	}
	if a.j.Commit == "" || head == "" {
		return fmt.Errorf("memory migrate: no commit recorded for %d imported paths; re-run from the commit phase", len(rels))
	}
	if _, err := repo.run("merge-base", "--is-ancestor", a.j.Commit, head); err != nil {
		return fmt.Errorf("memory migrate: baseline commit %s is not an ancestor of HEAD %s", a.j.Commit, head)
	}
	out, err := repo.run(append([]string{"ls-tree", "-r", "-z", "--name-only", head, "--"}, rels...)...)
	if err != nil {
		return err
	}
	in := map[string]bool{}
	for p := range strings.SplitSeq(out, "\x00") {
		in[p] = true
	}
	for _, rel := range rels {
		if !in[rel] {
			return fmt.Errorf("memory migrate: commit %s lacks imported path %s", head, rel)
		}
	}
	if _, err := repo.run(append([]string{"diff", "--quiet", head, "--"}, rels...)...); err != nil {
		if exitCode(err) == 1 {
			return fmt.Errorf("memory migrate: commit %s does not hold the verified bytes of every import", head)
		}
		return err
	}
	return nil
}

// commitPhase re-hashes every dest, then stages the verified store paths plus one receipt and commits them.
func (a *applier) commitPhase(repo Repo) error {
	rels, items, err := a.recheckDests()
	if err != nil || a.unresolved() {
		return err
	}
	if len(rels) == 0 {
		fmt.Fprintln(a.out, "commit: nothing imported into the store")
		return nil
	}
	receipt := a.receiptPath()
	if !exists(receipt) {
		body, err := json.MarshalIndent(map[string]any{"manifest": a.j.Manifest, "manifestSha256": a.j.ManifestSHA256, "items": items}, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(ReceiptsDir(), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(receipt, append(body, '\n'), 0o600); err != nil {
			return err
		}
	}
	if rel, ok := InsideStore(receipt); ok {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	body := fmt.Sprintf("copied %d, resumed %d, quarantined %d, conflicts %d, changed %d\nmanifest %s",
		a.res.Copied, a.res.Resumed, a.res.Quarantined, len(a.res.Conflicts), len(a.res.Changed), a.base)
	sha, committed, err := repo.Commit(rels, fmt.Sprintf("migrate: import %d files from legacy stores", len(items)), body)
	if err != nil {
		return err
	}
	if !committed { // index already matched HEAD: a resume after the commit landed but before the journal recorded it
		if sha, err = repo.Head(); err != nil {
			return err
		}
	}
	a.j.Commit = sha
	fmt.Fprintf(a.out, "commit: %d paths, %s\n", len(rels), sha)
	return nil
}

// typedImport: a copied typed bank file, the only kind the settle phase transforms.
func (a *applier) typedImport(it MigrateItem) bool {
	st := a.j.Steps[it.ID].State
	return a.action(it) == ActionCopy && (it.Kind == KindGlobalBank || it.Kind == KindRepoBank) &&
		(st == stepCopied || st == stepVerified)
}

// bankFrontmatter is the exact block settle prepends to a typed file; deterministic, so a resume can recognize it.
func bankFrontmatter(name string) string {
	return "---\nname: " + strings.TrimSuffix(name, ".md") + "\ndescription: " + bankMeta[name] +
		"\ntype: project\nharness_kind: bank\n---\n"
}

// settlePhase is plan §4's metadata transformation and settle commit: every imported typed file that carries no
// frontmatter gets the bank block through the journaled transaction (preimage-checked, receipted, committed) and its
// journal hash moves to the settled bytes. Index/catalog regeneration lands with the Phase 2 writers.
func (a *applier) settlePhase() error {
	if err := recoverJournals(); err != nil { // a settle txn that wrote but never committed is finished first
		return err
	}
	if err := a.adoptSettled(); err != nil {
		return err
	}
	if _, _, err := a.recheckDests(); err != nil || a.unresolved() {
		return err
	}
	changes, byRel, err := a.settleChanges()
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Fprintln(a.out, "settle: nothing to transform")
		return nil
	}
	rec, err := commitChangesLocked("migrate: settle "+a.base, changes)
	if err != nil && !errors.Is(err, ErrDivergent) {
		return err
	}
	for _, c := range changes {
		it := byRel[c.Rel]
		if slices.Contains(rec.Divergent, c.Rel) {
			err = a.mark(it, stepConflict, a.j.Steps[it.ID].DestSHA256)
		} else { // applied, or skipped because an earlier attempt already wrote these bytes
			err = a.mark(it, stepVerified, sha256Hex(c.Data))
		}
		if err != nil {
			return err
		}
	}
	fmt.Fprintf(a.out, "settle: %d typed files, commit %s\n", len(rec.Applied)+len(rec.Skipped), rec.Commit)
	return nil
}

// settleChanges plans the block prepend for every typed import still without frontmatter, keyed back to its item.
func (a *applier) settleChanges() ([]Change, map[string]MigrateItem, error) {
	var changes []Change
	byRel := map[string]MigrateItem{}
	for _, it := range a.m.Items {
		if !a.typedImport(it) {
			continue
		}
		cur, err := os.ReadFile(it.Dest)
		if err != nil {
			return nil, nil, err
		}
		if strings.HasPrefix(string(cur), "---") { // keeps the frontmatter it brought along
			continue
		}
		rel, _, err := destRel(it)
		if err != nil {
			return nil, nil, err
		}
		data := append([]byte(bankFrontmatter(filepath.Base(it.Dest))), cur...)
		changes = append(changes, Change{Rel: rel, Data: data, Preimage: cur, Checked: true})
		byRel[rel] = it
	}
	return changes, byRel, nil
}

// adoptSettled re-marks a typed import whose dest already starts with the block settle prepends and whose remainder
// still hashes to the journaled copy: a run that crashed between the settle commit and the journal update.
func (a *applier) adoptSettled() error {
	for _, it := range a.m.Items {
		if !a.typedImport(it) {
			continue
		}
		cur, err := os.ReadFile(it.Dest)
		if err != nil {
			continue // recheckDests reports it
		}
		block := bankFrontmatter(filepath.Base(it.Dest))
		if strings.HasPrefix(string(cur), block) && sha256Hex(cur[len(block):]) == a.j.Steps[it.ID].DestSHA256 {
			if err := a.mark(it, stepVerified, sha256Hex(cur)); err != nil {
				return err
			}
		}
	}
	return nil
}

// retirePhase re-verifies dests and HEAD, then removes sources whose bytes still match the manifest and empty
// directories deepest-first; a changed source is re-checked on the next run.
func (a *applier) retirePhase(repo Repo) error {
	rels, _, err := a.recheckDests()
	if err != nil {
		return err
	}
	if len(rels) > 0 {
		if rel, ok := InsideStore(a.receiptPath()); ok {
			rels = append(rels, rel)
		}
		if err := a.verifyCommit(repo, rels); err != nil {
			return err
		}
	}
	var dirs []MigrateItem
	for _, it := range a.m.Items {
		st := a.j.Steps[it.ID].State
		var err error
		switch act := a.action(it); {
		case it.Kind == KindEmptyDir:
			dirs = append(dirs, it)
		case act == ActionCopy && it.Inline == nil && (st == stepCopied || st == stepVerified || st == stepChanged):
			err = a.retireSource(it, stepRetired)
		case act == ActionDelete && st != stepDeleted:
			err = a.retireSource(it, stepDeleted)
		}
		if err != nil {
			return err
		}
	}
	return a.retireDirs(dirs)
}

// retireDirs removes planned directories deepest-first; a non-empty one is journaled as skipped, never an error.
func (a *applier) retireDirs(dirs []MigrateItem) error {
	sort.SliceStable(dirs, func(i, k int) bool { return len(dirs[i].Source) > len(dirs[k].Source) })
	for _, it := range dirs {
		if a.j.Steps[it.ID].State == stepDeleted {
			continue
		}
		if ents, err := os.ReadDir(it.Source); err == nil && len(ents) > 0 {
			if err := a.mark(it, stepSkipped, ""); err != nil {
				return err
			}
			continue
		}
		if err := os.Remove(it.Source); err != nil && !os.IsNotExist(err) {
			return err
		}
		a.res.Deleted++
		if err := a.mark(it, stepDeleted, ""); err != nil {
			return err
		}
	}
	return nil
}

// retireSource unlinks it.Source when it still hashes to the manifest; an already-absent source counts as done.
func (a *applier) retireSource(it MigrateItem, state string) error {
	h, _, err := fileOrLinkHash(it)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return err
	case h != it.SHA256:
		return a.mark(it, stepChanged, a.j.Steps[it.ID].DestSHA256)
	default:
		if err := os.Remove(it.Source); err != nil {
			return err
		}
	}
	switch state {
	case stepDeleted:
		a.res.Deleted++
	case stepQuarantined:
		a.res.Quarantined++
	}
	return a.mark(it, state, a.j.Steps[it.ID].DestSHA256)
}

// fileOrLinkHash is the current hash of it.Source; a symlink item hashes its target and must still dangle.
func fileOrLinkHash(it MigrateItem) (string, int64, error) {
	if it.Kind != KindSymlink {
		return sha256File(it.Source)
	}
	target, err := os.Readlink(it.Source)
	if err != nil {
		return "", 0, err
	}
	if _, err := os.Stat(it.Source); err == nil {
		return "", 0, nil // no longer dangling: the empty hash reads as changed
	}
	return sha256Hex([]byte(target)), 0, nil
}

// tally derives Skipped and Complete from the journal; idempotent across runs.
func (a *applier) tally() {
	a.res.Complete, a.res.Skipped = true, 0
	for _, it := range a.m.Items {
		st := a.j.Steps[it.ID].State
		if st == stepSkipped {
			a.res.Skipped++
		}
		var ok bool
		switch act := a.action(it); {
		case act == ActionSkip:
			ok = st == stepSkipped
		case it.Inline != nil:
			ok = st == stepVerified || st == stepCopied
		case act == ActionCopy:
			ok = st == stepRetired
		case act == ActionDelete:
			ok = st == stepDeleted
		default:
			ok = st == stepQuarantined
		}
		a.res.Complete = a.res.Complete && ok
	}
}
