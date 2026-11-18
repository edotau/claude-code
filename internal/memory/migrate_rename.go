package memory

import (
	"bufio"
	"context"
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
	"time"

	"github.com/edotau/claude-code/internal/textutil"
)

const (
	migRenameSubject = "memory: un-prefix folder names (flat 3)"
	migBodyLines     = 60
	migDateLayout    = "2006-01-02"
)

var (
	// migRenameDirs hold entries that carried a `<key>--` repo prefix; migDatedDirs also get a leading date.
	migRenameDirs = []string{"archive", "handoffs", "reports", "worktrees", "knowledge"}
	migDatedDirs  = []string{"handoffs", "reports", "worktrees"}

	migKeyRE      = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*--`) })
	migDashDateRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\d{4}-\d{2}-\d{2}`) })
	migCompactRE  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})-`) })
)

func migValidDate(s string) bool {
	_, err := time.Parse(migDateLayout, s)
	return err == nil
}

func migStripKeys(name string) (keys []string, rest string) {
	rest = name
	for {
		k := migKeyRE().FindString(rest)
		if k == "" || len(k) == len(rest) {
			return keys, rest
		}
		keys = append(keys, k[:len(k)-2])
		rest = rest[len(k):]
	}
}

// StripRepoPrefix drops every leading `<key>--` and returns the first key; a name with none comes back whole.
func StripRepoPrefix(name string) (rest, key string) {
	keys, rest := migStripKeys(name)
	if len(keys) == 0 {
		return name, ""
	}
	return rest, keys[0]
}

// DatedName strips the repo prefix and leads the name with a date: its own, one inside it, else now.
func DatedName(name string, now time.Time) string {
	rest, _ := StripRepoPrefix(name)
	return migDatedName(rest, func() string { return now.Format(migDateLayout) })
}

func migDatedName(rest string, fallback func() string) string {
	if len(rest) > 10 && rest[10] == '-' && migValidDate(rest[:10]) {
		return rest
	}
	if m := migCompactRE().FindStringSubmatch(rest); m != nil && migValidDate(m[1]+"-"+m[2]+"-"+m[3]) {
		return m[1] + "-" + m[2] + "-" + m[3] + rest[8:]
	}
	for _, d := range migDashDateRE().FindAllString(rest, -1) {
		if migValidDate(d) {
			return d + "-" + rest
		}
	}
	return fallback() + "-" + rest
}

// migHasPrefixed reports whether any folder entry still carries a repo prefix.
func migHasPrefixed(dir string) bool {
	for _, folder := range migRenameDirs {
		ents, _ := os.ReadDir(filepath.Join(dir, folder))
		for _, e := range ents {
			if keys, _ := migStripKeys(e.Name()); len(keys) > 0 {
				return true
			}
		}
	}
	return false
}

// migDates lazily reads each path's first-add date from one git log pass.
type migDates struct {
	dir    string
	loaded bool
	m      map[string]string
}

func (d *migDates) get(rel string) string {
	if !d.loaded {
		d.loaded, d.m = true, map[string]string{}
		args := []string{"-C", d.dir, "-c", "core.quotepath=off", "log", "--diff-filter=A", "--no-renames", "--name-only", "--format=%x00%as"}
		out, _ := exec.CommandContext(context.Background(), "git", args...).Output()
		cur := ""
		for l := range strings.SplitSeq(string(out), "\n") {
			switch {
			case strings.HasPrefix(l, "\x00"):
				cur = l[1:]
			case l != "":
				d.m[l] = cur // newest first, so the last write is the first add
				if p := strings.SplitN(l, "/", 3); len(p) == 3 {
					d.m[p[0]+"/"+p[1]] = cur
				}
			}
		}
	}
	return d.m[rel]
}

// migBodyDate is the first valid date in a file's first lines.
func migBodyDate(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for n := 0; n < migBodyLines && sc.Scan(); n++ {
		for _, d := range migDashDateRE().FindAllString(sc.Text(), -1) {
			if migValidDate(d) {
				return d
			}
		}
	}
	_ = sc.Err() // an unreadable or over-long line just ends the scan with no date
	return ""
}

// migEntryDate dates an undated entry from its body, git history, then mtime.
func migEntryDate(dir, rel string, e fs.DirEntry, dates *migDates) string {
	if !e.IsDir() {
		if d := migBodyDate(filepath.Join(dir, rel)); d != "" {
			return d
		}
	}
	if d := dates.get(rel); d != "" {
		return d
	}
	if info, err := e.Info(); err == nil {
		return info.ModTime().Format(migDateLayout)
	}
	return time.Now().Format(migDateLayout)
}

// planFolders plans the un-prefix pass over every store folder.
func (p *migPlan) planFolders(dir string) {
	dates := &migDates{dir: dir}
	for _, folder := range migRenameDirs {
		ents, err := os.ReadDir(filepath.Join(dir, folder))
		if err != nil {
			continue
		}
		taken := map[string]string{} // name -> source rel of the entry planned onto it, "" if on disk
		for _, e := range ents {
			taken[e.Name()] = ""
		}
		for _, e := range ents {
			keys, rest := migStripKeys(e.Name())
			if len(keys) == 0 {
				continue
			}
			to := rest
			if slices.Contains(migDatedDirs, folder) {
				to = migDatedName(rest, func() string { return migEntryDate(dir, folder+"/"+e.Name(), e, dates) })
			}
			p.planEntry(migEntry{dir: dir, folder: folder, name: e.Name(), key: keys[0], to: to, isDir: e.IsDir()}, taken)
		}
	}
}

// migEntry is one prefixed folder entry and the name it moves to.
type migEntry struct {
	dir, folder, name, key, to string
	isDir                      bool
}

func (p *migPlan) planEntry(en migEntry, taken map[string]string) {
	from, toRel := en.folder+"/"+en.name, en.folder+"/"+en.to
	claimant, busy := taken[en.to]
	if !busy {
		taken[en.to] = from
		p.addRename(from, toRel)
		return
	}
	tgt := filepath.Join(en.dir, toRel)
	if claimant != "" {
		tgt = filepath.Join(en.dir, claimant)
	}
	sameFile := !en.isDir && migIsFile(tgt)
	switch {
	case sameFile && en.folder == "archive" && claimant == "" && strings.HasSuffix(en.to, ".md"):
		p.planArchiveMerge(en.dir, from, toRel)
	case sameFile && p.read(tgt) == p.read(filepath.Join(en.dir, from)):
		p.rows = append(p.rows, fmt.Sprintf("drop %s (same as %s)", from, toRel))
		p.removes = append(p.removes, from)
		p.gone[from], p.refs[from] = true, toRel
		p.counts[2]++
	default:
		p.planSuffix(en, from, taken)
	}
}

// planSuffix renames a differing collision to `<name>-<key><ext>`; a taken suffix is a blocker.
func (p *migPlan) planSuffix(en migEntry, from string, taken map[string]string) {
	ext := ""
	if !en.isDir {
		ext = filepath.Ext(en.to)
	}
	alt := strings.TrimSuffix(en.to, ext) + "-" + en.key + ext
	if _, busy := taken[alt]; busy {
		p.blockers = append(p.blockers, fmt.Sprintf("collision: %s -> %s/%s and %s/%s are both taken", from, en.folder, en.to, en.folder, alt))
		return
	}
	taken[alt] = from
	p.addRename(from, en.folder+"/"+alt)
}

func migIsFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func (p *migPlan) addRename(from, to string) {
	p.rows = append(p.rows, fmt.Sprintf("rename %s -> %s", from, to))
	p.renames = append(p.renames, [2]string{from, to})
	p.refs[from] = to
	p.counts[0]++
}

// planArchiveMerge folds a legacy archive into the same-named one, skipping blocks the target or live file hold.
func (p *migPlan) planArchiveMerge(dir, from, toRel string) {
	src := p.read(filepath.Join(dir, from))
	if textutil.CredentialRE().MatchString(src) {
		p.blockers = append(p.blockers, "credential-shaped content: "+from)
		return
	}
	target, ok := p.writes[toRel]
	if !ok {
		target = p.read(filepath.Join(dir, toRel))
	}
	merged, added := migMergeArchive(target, src, p.liveOf(dir, path.Base(toRel)))
	if added > 0 {
		p.writes[toRel] = merged
	}
	p.rows = append(p.rows, fmt.Sprintf("merge %s -> %s (+%d blocks)", from, toRel, added))
	p.removes = append(p.removes, from)
	p.gone[from], p.refs[from] = true, toRel
	p.counts[1]++
}

// liveOf is the top-level file of the archive's type (decisionLog.md, ...), or "".
func (p *migPlan) liveOf(dir, base string) string {
	for _, t := range migLogTyped {
		if strings.HasPrefix(base, t+"-") {
			if s, ok := p.writes[t+".md"]; ok {
				return s
			}
			return p.read(filepath.Join(dir, t+".md"))
		}
	}
	return ""
}

func migHeadKey(block string) string {
	h, _, _ := strings.Cut(block, "\n")
	return strings.TrimSpace(migIDRE().ReplaceAllString(h, ""))
}

// migMergeArchive adds src's blocks absent from target and live; an id-less src merges by heading.
func migMergeArchive(target, src, live string) (string, int) {
	pre, blocks := migSplitBlocks(src)
	if len(blocks) == 0 {
		return migMergeParas(target, src, live)
	}
	byID := slices.ContainsFunc(blocks, migIDRE().MatchString)
	skip := map[string]bool{}
	if byID {
		_, lb := migSplitBlocks(live)
		for _, b := range lb {
			skip[migIDRE().FindString(b)] = true
		}
	} else {
		_, tb := migSplitBlocks(target)
		for _, b := range tb {
			skip[migHeadKey(b)] = true
		}
	}
	var keep []string
	for _, b := range blocks {
		k := migHeadKey(b)
		if byID {
			k = migIDRE().FindString(b)
		}
		if k == "" || !skip[k] {
			keep = append(keep, b)
		}
	}
	if len(keep) == 0 {
		return target, 0
	}
	return migMergeBlocks(target, pre+"\n\n"+strings.Join(keep, "\n\n")+"\n")
}

// migMergeParas appends src's paragraphs (bullet-style archives have no `## ` blocks) unknown to target and live.
func migMergeParas(target, src, live string) (string, int) {
	var add []string
	for para := range strings.SplitSeq(src, "\n\n") {
		para = strings.TrimSpace(para)
		id := migIDRE().FindString(para)
		switch {
		case para == "" || strings.HasPrefix(para, "# "):
		case id != "" && (strings.Contains(target, id) || strings.Contains(live, id)):
		case id == "" && strings.Contains(target, para):
		default:
			add = append(add, para)
		}
	}
	if len(add) == 0 {
		return target, 0
	}
	return strings.TrimRight(target, "\n") + "\n\n" + strings.Join(add, "\n\n") + "\n", len(add)
}

func (p *migPlan) printFolders(out io.Writer) {
	for _, r := range p.rows {
		_, _ = fmt.Fprintln(out, r)
	}
	if len(p.rows) == 0 {
		_, _ = fmt.Fprintln(out, "nothing to rename")
	}
	_, _ = fmt.Fprintf(out, "folders: rename %d, merge %d, drop %d\n", p.counts[0], p.counts[1], p.counts[2])
}

// planRefs rewrites path-shaped mentions of renamed entries in every store .md except index.md.
func (p *migPlan) planRefs(dir string) {
	if len(p.refs) == 0 {
		return
	}
	byBase := map[string][]string{}
	for old := range p.refs {
		byBase[path.Base(old)] = append(byBase[path.Base(old)], old)
	}
	for _, c := range byBase {
		slices.Sort(c)
	}
	_ = filepath.WalkDir(dir, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			p.blockers = append(p.blockers, "unreadable: "+fp+": "+err.Error())
			return nil
		}
		rel, _ := filepath.Rel(dir, fp)
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		p.refFile(fp, filepath.ToSlash(rel), d, byBase)
		return nil
	})
}

func (p *migPlan) refFile(fp, rel string, d fs.DirEntry, byBase map[string][]string) {
	if !d.Type().IsRegular() || !strings.HasSuffix(rel, ".md") || storeLevelFile(rel) || p.gone[rel] {
		return
	}
	now := p.postRel(rel)
	text, ok := p.writes[now]
	if !ok {
		text = p.read(fp)
	}
	if out := p.rewriteText(text, path.Dir(now), byBase); out != text {
		p.writes[now] = out
	}
}

// postRel is rel after the folder-entry renames (a file inside a renamed dir moves with it).
func (p *migPlan) postRel(rel string) string {
	parts := strings.SplitN(rel, "/", 3)
	if len(parts) < 2 {
		return rel
	}
	if n, ok := p.refs[parts[0]+"/"+parts[1]]; ok && !p.gone[parts[0]+"/"+parts[1]] {
		return path.Join(append([]string{n}, parts[2:]...)...)
	}
	return rel
}

func migPathByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_./~-", c) >= 0
}

// rewriteText maps each path-shaped token containing `--` that names a renamed entry.
func (p *migPlan) rewriteText(text, refdir string, byBase map[string][]string) string {
	var b strings.Builder
	i := 0
	for {
		k := strings.Index(text[i:], "--")
		if k < 0 {
			break
		}
		k += i
		l, r := k, k+2
		for l > i && migPathByte(text[l-1]) {
			l--
		}
		for r < len(text) && migPathByte(text[r]) {
			r++
		}
		b.WriteString(text[i:l])
		b.WriteString(p.mapToken(text[l:r], refdir, byBase))
		i = r
	}
	if i == 0 {
		return text
	}
	b.WriteString(text[i:])
	return b.String()
}

func (p *migPlan) mapToken(tok, refdir string, byBase map[string][]string) string {
	core := strings.TrimRight(tok, "./")
	tail := tok[len(core):]
	parts := strings.Split(core, "/")
	for i := range parts {
		if repl, ok := p.lookupPart(parts, i, refdir, byBase); ok {
			parts[i] = repl
			return strings.Join(parts, "/") + tail
		}
	}
	return tok
}

func (p *migPlan) lookupPart(parts []string, i int, refdir string, byBase map[string][]string) (string, bool) {
	c := parts[i]
	if f, rest, ok := strings.Cut(c, "--"); ok && slices.Contains(migRenameDirs, f) {
		if n, ok := p.refs[f+"/"+rest]; ok {
			return migRelTo(refdir, n), true
		}
	}
	if i > 0 && slices.Contains(migRenameDirs, parts[i-1]) {
		if n, ok := p.refs[parts[i-1]+"/"+c]; ok {
			return path.Base(n), true
		}
		return "", false
	}
	cands := byBase[c]
	top, _, _ := strings.Cut(refdir, "/")
	for _, o := range cands {
		if strings.HasPrefix(o, top+"/") {
			return migRelTo(refdir, p.refs[o]), true
		}
	}
	if len(cands) == 1 {
		return migRelTo(refdir, p.refs[cands[0]]), true
	}
	return "", false
}

func migRelTo(refdir, target string) string {
	if path.Dir(target) == refdir {
		return path.Base(target)
	}
	r, err := filepath.Rel(refdir, target)
	if err != nil {
		return target
	}
	return filepath.ToSlash(r)
}
