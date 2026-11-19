package memory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	migSubject     = "memory: migrate to one bank"
	migIndexFile   = "MEMORY.md"
	migOriginsFile = ".origins"
	migLayoutName  = ".layout"
)

// migLogTyped merge by dated blocks; migSectionTyped merge by `## ` sections.
var (
	migLogTyped     = []string{"decisionLog", "conventions", "sessionHistory"}
	migSectionTyped = []string{"activeContext", "progress", "projectContext"}
)

var (
	// ErrMigrateBlocked is a migration refused before any write (exit 1).
	ErrMigrateBlocked = errors.New("migrate refused")

	migIDRE     = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`<!-- id:\S+ -->`) })
	migDateRE   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^## (\d{4}-\d{2}-\d{2})`) })
	migTemplate = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\(.*\)$`) })
)

// MigrateOptions is one `memory migrate` (flat 1 → one bank); DryRun lists the plan and blockers and writes nothing.
type MigrateOptions struct {
	DryRun bool
	Out    io.Writer
}

type migPlan struct {
	renames  [][2]string       // store-relative from, to
	writes   map[string]string // merged typed files
	entries  []string          // index.md entries with their link target repointed, appended after the moves
	removes  []string
	links    map[string]string // old rel -> new rel, for index entries
	merged   map[string]int
	kept     []string
	blockers []string
	topics   int
	rows     []string          // folder-pass rows: rename/merge/drop
	refs     map[string]string // folder-pass old rel -> new rel, for reference rewriting
	gone     map[string]bool   // legacy rels removed by a merge or drop
	counts   [3]int            // renames, merges, drops
	perKey   bool              // the store is flat 1: the per-key merge runs too
}

// Migrate stamps `flat 3`: a flat 1 store is merged into one bank, then folder entries lose their repo prefix.
func Migrate(o MigrateOptions) error {
	return WithBankLock(0, func() error { return migrate(o) })
}

func migrate(o MigrateOptions) error {
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	dir := StoreDir()
	stampRaw, _ := os.ReadFile(filepath.Join(dir, migLayoutName))
	stamp := strings.TrimSpace(string(stampRaw))
	if stamp == layoutFlat && !migHasPrefixed(dir) {
		_, _ = fmt.Fprintln(out, "already migrated")
		return nil
	}
	p, err := planMigration(dir, stamp)
	if err != nil {
		return err
	}
	p.print(out)
	if len(p.blockers) > 0 {
		return fmt.Errorf("%w: %d blocker(s)", ErrMigrateBlocked, len(p.blockers))
	}
	if o.DryRun {
		return nil
	}
	return p.apply(dir)
}

func (p *migPlan) print(out io.Writer) {
	for _, b := range p.blockers {
		_, _ = fmt.Fprintln(out, "blocker:", b)
	}
	for _, k := range p.kept {
		_, _ = fmt.Fprintln(out, "collision kept:", k)
	}
	if p.perKey {
		var line strings.Builder
		fmt.Fprintf(&line, "topics renamed %d, collisions kept %d", p.topics, len(p.kept))
		for _, t := range append(slices.Clone(migLogTyped), migSectionTyped...) {
			fmt.Fprintf(&line, ", %s +%d", t, p.merged[t])
		}
		_, _ = fmt.Fprintln(out, line.String())
	}
	p.printFolders(out)
}

func planMigration(dir, stamp string) (*migPlan, error) {
	p := &migPlan{writes: map[string]string{}, links: map[string]string{}, merged: map[string]int{},
		refs: map[string]string{}, gone: map[string]bool{}, perKey: stamp == layoutPerKey}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("memory migrate: %w", err)
	}
	if !slices.Contains([]string{layoutPerKey, layoutFlat2, layoutFlat}, stamp) {
		p.blockers = append(p.blockers, fmt.Sprintf("layout is %q, want %q, %q or %q", stamp, layoutPerKey, layoutFlat2, layoutFlat))
	}
	p.gitBlockers(dir)
	have := map[string]bool{}
	var orig []string
	for _, e := range ents {
		switch {
		case !e.IsDir():
			have[e.Name()] = true
			orig = append(orig, e.Name())
		}
	}
	var keys []string
	if p.perKey {
		keys = p.planMerges(dir, orig, have)
		p.planTopics(dir, keys, orig, have)
		p.addTypedLinks(keys)
	}
	p.planFolders(dir)
	p.planRefs(dir)
	p.planEntries(dir)
	return p, nil
}

func migGit(dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}

// gitBlockers refuses a dirty tree or a missing commit identity: the final commit must not fail after the moves.
func (p *migPlan) gitBlockers(dir string) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return
	}
	if out, err := migGit(dir, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		p.blockers = append(p.blockers, "store git tree is dirty (commit or discard first)")
	}
	if _, err := migGit(dir, "var", "GIT_COMMITTER_IDENT"); err != nil {
		p.blockers = append(p.blockers, "git has no committer identity")
	}
}

func migKeyOf(name string) (key, typed string, ok bool) {
	for _, t := range append(slices.Clone(migLogTyped), migSectionTyped...) {
		if k, found := strings.CutSuffix(name, "--"+t+".md"); found && k != "" {
			return k, t, true
		}
	}
	return "", "", false
}

// read returns the file's text; a missing file is "", any other error is a blocker so no unread content is lost.
func (p *migPlan) read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.blockers = append(p.blockers, "unreadable: "+path+": "+err.Error())
	}
	return string(b)
}

// planMerges folds each <key>--<typed>.md into <typed>.md and returns the repo keys seen.
func (p *migPlan) planMerges(dir string, orig []string, have map[string]bool) []string {
	var keys []string
	for _, n := range orig {
		key, typed, ok := migKeyOf(n)
		if !ok {
			continue
		}
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
		src := p.read(filepath.Join(dir, n))
		if textutil.CredentialRE().MatchString(src) {
			p.blockers = append(p.blockers, "credential-shaped content: "+n)
			continue
		}
		target, ok := p.writes[typed+".md"]
		if !ok {
			target = p.read(filepath.Join(dir, typed+".md"))
		}
		var added int
		if slices.Contains(migLogTyped, typed) {
			target, added = migMergeBlocks(target, src)
		} else {
			target, added = migMergeSections(target, src)
		}
		if added > 0 {
			p.writes[typed+".md"] = target
		}
		p.merged[typed] += added
		p.removes = append(p.removes, n)
		delete(have, n)
	}
	return keys
}

// planTopics renames <key>--<name>.md to <name>.md; a differing file already there stays and is reported.
func (p *migPlan) planTopics(dir string, keys, orig []string, have map[string]bool) {
	slices.SortFunc(keys, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, n := range orig {
		if !have[n] || !strings.HasSuffix(n, ".md") || n == migIndexFile {
			continue
		}
		for _, k := range keys {
			to, ok := strings.CutPrefix(n, k+"--")
			if !ok || to == "" {
				continue
			}
			p.renameTopic(dir, n, to, have)
			break
		}
	}
}

func (p *migPlan) renameTopic(dir, from, to string, have map[string]bool) {
	switch {
	case !have[to]:
		have[to] = true
		p.renames = append(p.renames, [2]string{from, to})
		p.links[from] = to
		p.topics++
	case p.read(filepath.Join(dir, from)) == p.read(filepath.Join(dir, to)) && !slices.Contains(p.removes, to):
		p.removes = append(p.removes, from)
		p.links[from] = to
		p.topics++
	default:
		p.kept = append(p.kept, from)
	}
}

// addTypedLinks maps each merged <key>--<typed>.md to <typed>.md.
func (p *migPlan) addTypedLinks(keys []string) {
	for _, k := range keys {
		for _, t := range append(slices.Clone(migLogTyped), migSectionTyped...) {
			p.links[k+"--"+t+".md"] = t + ".md"
		}
	}
}

// mapTarget is where a store-relative link target lands once every file and folder rename is applied.
func (p *migPlan) mapTarget(t string) string {
	if n, ok := p.refs[t]; ok {
		return n
	}
	if n, ok := p.links[t]; ok {
		return n
	}
	if f, rest, ok := strings.Cut(t, "--"); ok && slices.Contains(migRenameDirs, f) { // legacy `folder--name`
		if n, ok := p.refs[f+"/"+rest]; ok {
			return n
		}
	}
	var hits []string
	for old, n := range p.refs {
		if !strings.Contains(t, "/") && path.Base(old) == t {
			hits = append(hits, n)
		}
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return p.postRel(t)
}

// planEntries queues each stored or pending entry repointed past the renames; originals stay, index.md only grows.
func (p *migPlan) planEntries(dir string) {
	if err := checkIndexFiles(); err != nil {
		p.blockers = append(p.blockers, err.Error())
		return
	}
	all, gen := decodeCatalog(p.read(filepath.Join(dir, catalogFile))), lastGenerated()
	for _, f := range append(keptCopies(), filepath.Join(dir, migIndexFile)) {
		in, _ := inputEntries(p.read(f), gen)
		all = append(all, in...)
	}
	for _, e := range all {
		if m := pointerRE().FindStringSubmatchIndex(textutil.FirstLine(e)); m != nil {
			if t := e[m[2]:m[3]]; p.mapTarget(t) != t {
				p.entries = append(p.entries, e[:m[2]]+p.mapTarget(t)+e[m[3]:])
			}
		}
	}
}

func (p *migPlan) apply(dir string) error {
	at := func(rel string) string { return filepath.Join(dir, rel) }
	var err error
	step := func(rel string, f func() error) {
		if err == nil {
			if err = f(); err != nil {
				err = fmt.Errorf("%s: %w; store %s is uncommitted, `git -C %s status` shows the partial move "+
					"(recover with `git -C %s checkout -- . && git -C %s clean -fd`)", rel, err, dir, dir, dir, dir)
			}
		}
	}
	// Renames first (writes target post-rename paths), then writes, then removes: a source goes only after all succeeded.
	for _, r := range p.renames {
		step(r[0], func() error { return os.Rename(at(r[0]), at(r[1])) })
	}
	for name, body := range p.writes {
		step(name, func() error { return paths.AtomicWrite(at(name), []byte(body), 0o644) })
	}
	for _, rel := range append(p.removes, migOriginsFile) {
		step(rel, func() error {
			if e := os.Remove(at(rel)); !errors.Is(e, fs.ErrNotExist) {
				return e
			}
			return nil
		})
	}
	step(migLayoutName, func() error { return paths.AtomicWrite(at(migLayoutName), []byte(layoutFlat+"\n"), 0o644) })
	if p.perKey {
		step("sweep notes", sweepNotes) // last move: loose notes into feedback/ and notes/, MEMORY.md repointed
	}
	step(catalogFile, func() error { _, err := appendToCatalog(p.entries); return err })
	step("commit", func() error { return migCommit(dir, p.perKey) }) // only after every move succeeded
	if err != nil {
		return fmt.Errorf("memory migrate: %w", err)
	}
	return nil
}

func migCommit(dir string, perKey bool) error {
	subject := migRenameSubject
	if perKey {
		subject = migSubject
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return nil
	}
	if out, err := migGit(dir, "add", "-A"); err != nil {
		return fmt.Errorf("git add: %s", strings.TrimSpace(out))
	}
	if out, err := migGit(dir, "-c", "commit.gpgsign=false", "commit", "-q", "--no-verify", "-m", subject); err != nil {
		return fmt.Errorf("git commit: %s", strings.TrimSpace(out))
	}
	return nil
}

// migSplitBlocks splits text at `## ` headings into the preamble and its blocks, trailing blanks trimmed.
func migSplitBlocks(text string) (pre string, blocks []string) {
	var cur []string
	flush := func() {
		if cur == nil {
			return
		}
		blocks = append(blocks, strings.TrimRight(strings.Join(cur, "\n"), " \t\n"))
	}
	var head []string
	for l := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(l, "## ") {
			flush()
			cur = []string{l}
			continue
		}
		if cur == nil {
			head = append(head, l)
		} else {
			cur = append(cur, l)
		}
	}
	flush()
	return strings.TrimRight(strings.Join(head, "\n"), " \t\n"), blocks
}

func migDate(block string) string {
	head, _, _ := strings.Cut(block, "\n")
	if m := migDateRE().FindStringSubmatch(head); m != nil {
		return m[1]
	}
	return ""
}

// migInsertAt is the index of the first block dated before date, else the end (undated goes last).
func migInsertAt(blocks []string, date string) int {
	if date == "" {
		return len(blocks)
	}
	at := slices.IndexFunc(blocks, func(o string) bool { d := migDate(o); return d != "" && d < date })
	if at < 0 {
		return len(blocks)
	}
	return at
}

// migMergeBlocks inserts src's blocks into target newest first, skipping ids target already holds.
func migMergeBlocks(target, src string) (string, int) {
	pre, blocks := migSplitBlocks(target)
	spre, sblocks := migSplitBlocks(src)
	if strings.TrimSpace(target) == "" {
		pre = spre
	}
	known := map[string]bool{}
	for _, b := range blocks {
		known[b] = true
		if id := migIDRE().FindString(b); id != "" {
			known[id] = true
		}
	}
	added := 0
	for _, b := range sblocks {
		id := migIDRE().FindString(b)
		if known[b] || (id != "" && known[id]) {
			continue
		}
		known[b], known[id] = true, id != ""
		blocks = slices.Insert(blocks, migInsertAt(blocks, migDate(b)), b)
		added++
	}
	if added == 0 {
		return target, 0
	}
	return pre + "\n\n" + strings.Join(blocks, "\n\n") + "\n", added
}

// migMergeSections appends src's non-template bullets under the same `## ` section of target.
func migMergeSections(target, src string) (string, int) {
	spre, sblocks := migSplitBlocks(src)
	if strings.TrimSpace(target) == "" {
		var t strings.Builder
		t.WriteString(spre)
		for _, b := range sblocks {
			h, _, _ := strings.Cut(b, "\n")
			fmt.Fprintf(&t, "\n\n%s", h)
		}
		target = t.String()
	}
	pre, blocks := migSplitBlocks(target)
	added := 0
	add := func(name string, body []string) {
		items := migItems(body)
		if name == "" {
			pre = strings.Join(append(strings.Split(pre, "\n"), items...), "\n")
		} else if i := slices.IndexFunc(blocks, func(b string) bool { return strings.HasPrefix(b, name+"\n") || b == name }); i >= 0 {
			blocks[i] = strings.Join(append([]string{blocks[i]}, items...), "\n")
		} else if len(items) > 0 {
			blocks = append(blocks, strings.Join(append([]string{name}, items...), "\n"))
		}
		added += len(items)
	}
	body := strings.Split(spre, "\n")
	add("", migDropHeader(body))
	for _, b := range sblocks {
		h, rest, _ := strings.Cut(b, "\n")
		add(h, strings.Split(rest, "\n"))
	}
	if added == 0 {
		return target, 0
	}
	parts := append([]string{pre}, blocks...)
	return strings.Join(parts, "\n\n") + "\n", added
}

func migDropHeader(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "# ") && !strings.HasPrefix(l, "> ") {
			out = append(out, l)
		}
	}
	return out
}

// migItems turns body lines into bullets, dropping blanks and template placeholders.
func migItems(body []string) []string {
	var out []string
	prevBlank := true
	for _, l := range body {
		t := strings.TrimSpace(l)
		switch {
		case t == "":
			prevBlank = true
			continue
		case migTemplate().MatchString(t):
		case strings.HasPrefix(l, "- ") || strings.HasPrefix(l, "* "):
			out = append(out, l)
		case strings.HasPrefix(l, " ") || !prevBlank:
			out = append(out, "  "+t)
		default:
			out = append(out, "- "+t)
		}
		prevBlank = false
	}
	return out
}
