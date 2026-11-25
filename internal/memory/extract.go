package memory

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/textutil"
)

type ExtractAction string

const (
	ExtractKeep    ExtractAction = "keep"
	ExtractRewrite ExtractAction = "rewrite"
	ExtractPurge   ExtractAction = "purge"
)

type ExtractReason string

const (
	ExtractProtected       ExtractReason = "protected"
	ExtractLinked          ExtractReason = "linked"
	ExtractPinned          ExtractReason = "pinned"
	ExtractRecalled        ExtractReason = "recalled"
	ExtractYoung           ExtractReason = "younger_than_7_days"
	ExtractEmpty           ExtractReason = "empty"
	ExtractDuplicate       ExtractReason = "duplicate"
	ExtractDuplicateSource ExtractReason = "duplicate_source"
	ExtractMissingBranch   ExtractReason = "missing_branch"
	ExtractResultInGit     ExtractReason = "result_in_git"
	ExtractLowValue        ExtractReason = "low_value"
	ExtractRecallUnknown   ExtractReason = "recall_unknown"
)

// ExtractFacts supplies proof, not guesses; known=false must be used for an unidentified branch or run.
type ExtractFacts interface {
	WorktreeBranchExists(rel string, data []byte) (exists, known bool, err error)
	ReportResultInGit(rel string, data []byte) (inGit, known bool, err error)
}

// ExtractOptions has no implicit paths; access logs include their rotated generations, plans are explicit files.
type ExtractOptions struct {
	Now          time.Time
	Facts        ExtractFacts
	AccessLogs   []string
	LinkSources  []string
	StoreAliases []string // original roots when classifying a copy containing absolute links
}

type ExtractSignals struct {
	LinkedBy         []string  `json:"linked_by,omitempty"`
	Pinned           bool      `json:"pinned"`
	YoungerThan7Days bool      `json:"younger_than_7_days"`
	RecallKnown      bool      `json:"recall_known"`
	RecalledRecently bool      `json:"recalled_recently"`
	RecallDays       int       `json:"recall_days"` // distinct UTC days, across all supplied log history
	LastRecalled     time.Time `json:"last_recalled,omitzero"`
	BranchKnown      bool      `json:"branch_known"`
	BranchExists     bool      `json:"branch_exists"`
	ResultKnown      bool      `json:"result_known"`
	ResultInGit      bool      `json:"result_in_git"`
}

type ExtractRow struct {
	Path        string         `json:"path"` // store-relative, slash-separated
	Folder      string         `json:"folder"`
	Size        int64          `json:"size"` // bytes
	Age         time.Duration  `json:"age"`  // since filesystem modification; future dates clamp to zero
	Action      ExtractAction  `json:"action"`
	Reason      ExtractReason  `json:"reason"`
	Signals     ExtractSignals `json:"signals"`
	SHA256      string         `json:"sha256"`
	DuplicateOf string         `json:"duplicate_of,omitempty"`
}

// ExtractGroup contains only candidates of one action and UTC modification week; keep rows are never batched.
type ExtractGroup struct {
	Week        string        `json:"week"`
	Action      ExtractAction `json:"action"`
	Paths       []string      `json:"paths"`
	Bytes       int64         `json:"bytes"`
	YoungestAge time.Duration `json:"youngest_age"`
	RecallDays  int           `json:"recall_days"` // maximum per-file distinct recall days
}

type ExtractTotal struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

type ExtractFolderTotal struct {
	Folder        string `json:"folder"`
	Budget        int    `json:"budget"` // zero means unbudgeted
	OverBudget    int    `json:"over_budget"`
	RewriteExcess int    `json:"rewrite_excess"` // excess after proposed purges; continuous rewrite trigger
	ExtractTotal
	Actions map[ExtractAction]ExtractTotal `json:"actions"`
	Groups  []ExtractGroup                 `json:"groups,omitempty"`
}

type ExtractResult struct {
	Rows    []ExtractRow         `json:"rows"`
	Folders []ExtractFolderTotal `json:"folders"`
}

var extractFolderBudgets = map[string]int{"reports": 400, "worktrees": 150, "handoffs": 60, "archive": 200}

type extractFile struct {
	row  ExtractRow
	data []byte
	refs []string
	mod  time.Time
}

// ClassifyExtract is read-only; low-value rewrite proposals do not themselves trigger an under-budget dream.
func ClassifyExtract(storeRoot string, opts ExtractOptions) (ExtractResult, error) {
	if opts.Now.IsZero() {
		return ExtractResult{}, errors.New("extract: Now is required")
	}
	root, err := os.OpenRoot(storeRoot)
	if err != nil {
		return ExtractResult{}, fmt.Errorf("extract store: %w", err)
	}
	defer root.Close()
	files, err := extractFiles(root, opts.Now)
	if err != nil {
		return ExtractResult{}, err
	}
	if err := extractLinks(files, storeRoot, opts); err != nil {
		return ExtractResult{}, err
	}
	if err := extractRecall(files, opts); err != nil {
		return ExtractResult{}, err
	}
	for i := range files {
		if err := extractDecide(&files[i], opts.Facts); err != nil {
			return ExtractResult{}, err
		}
		r := &files[i].row
		if r.Action == ExtractPurge && !r.Signals.RecallKnown {
			r.Action, r.Reason = ExtractRewrite, ExtractRecallUnknown
		}
	}
	extractDuplicates(files)
	return extractResult(files), nil
}

func extractFiles(root *os.Root, now time.Time) ([]extractFile, error) {
	var files []extractFile
	err := fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("extract walk %s: %w", rel, err)
		}
		if d.IsDir() {
			if skipExtractDir(rel, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if skipExtractFile(rel, d) {
			return nil
		}
		data, err := root.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			return fmt.Errorf("extract read %s: %w", rel, err)
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("extract stat %s: %w", rel, err)
		}
		files = append(files, newExtractFile(rel, data, info.ModTime(), now))
		return nil
	})
	return files, err
}

func skipExtractDir(rel, name string) bool {
	return rel != "." && (strings.HasPrefix(name, ".") || skipImportDir(".", name))
}

func skipExtractFile(rel string, d fs.DirEntry) bool {
	return !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") || foldSkips(rel) && rel != routerFile
}

func newExtractFile(rel string, data []byte, mod, now time.Time) extractFile {
	folder, _, nested := strings.Cut(rel, "/")
	if !nested {
		folder = "."
	}
	fm, _ := textutil.ParseFrontmatter(string(data))
	meta, _ := fm["metadata"].(map[string]any)
	r := ExtractRow{Path: rel, Folder: folder, Size: int64(len(data)), Age: max(0, now.Sub(mod)),
		SHA256: textutil.SHA256Hex(data), Action: ExtractRewrite, Reason: ExtractLowValue}
	r.Signals.Pinned = meta["pinned"] == true || fm["pinned"] == true
	r.Signals.YoungerThan7Days = r.Age < 7*24*time.Hour
	refs := []string{rel}
	if id, ok := fm["id"].(string); ok {
		refs = append(refs, id)
	}
	mode := segNative
	switch {
	case folder == "archive", folder == "." && slices.Contains(bankFiles, path.Base(rel)):
		mode = segEntries
	case folder == "knowledge":
		mode = segKnowledge
	}
	for _, block := range parseBlocks(mode, path.Base(rel), data) {
		refs = append(refs, blockRef(block, ""))
	}
	return extractFile{row: r, data: data, refs: refs, mod: mod}
}

func extractTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("`\"'[]()<>#;,|*!", r)
	})
}

func extractLinks(files []extractFile, storeRoot string, opts ExtractOptions) error {
	byRef := map[string][]int{}
	for i, f := range files {
		for _, ref := range f.refs {
			byRef[ref] = append(byRef[ref], i)
		}
	}
	roots := extractLinkRoots(storeRoot, opts.StoreAliases)
	for _, f := range files {
		if extractLinkSource(f.row.Path) {
			markExtractLinks(files, byRef, f.row.Path, string(f.data), path.Dir(f.row.Path), roots)
		}
	}
	for _, source := range opts.LinkSources {
		data, err := os.ReadFile(source)
		if err != nil {
			return fmt.Errorf("extract link source %s: %w", source, err)
		}
		markExtractLinks(files, byRef, source, string(data), filepath.ToSlash(filepath.Dir(source)), roots)
	}
	return nil
}

func extractLinkSource(rel string) bool {
	base, _ := StripRepoPrefix(path.Base(rel))
	return !strings.Contains(rel, "/") && (base == nativeIndexFile || base == routerFile || slices.Contains(bankFiles, base)) ||
		strings.HasPrefix(rel, "plans/")
}

func extractLinkRoots(store string, aliases []string) []string {
	roots := []string{"~/.claude/memory", "$HOME/.claude/memory", "${HOME}/.claude/memory", "memory"}
	for _, root := range append([]string{store}, aliases...) {
		roots = append(roots, filepath.ToSlash(filepath.Clean(root)))
		if abs, err := filepath.Abs(root); err == nil {
			roots = append(roots, filepath.ToSlash(abs))
		}
	}
	return roots
}

func extractLinkRef(token string, roots []string) string {
	token = strings.TrimPrefix(strings.Trim(strings.TrimRight(token, ".:~"), "{}"), "~~")
	if decoded, err := url.PathUnescape(token); err == nil {
		token = decoded
	}
	if file, _, ok := strings.Cut(token, ".md:"); ok {
		token = file + ".md"
	}
	token = strings.TrimPrefix(token, "file://")
	for _, root := range roots {
		if rel, ok := strings.CutPrefix(token, root+"/"); ok {
			return rel
		}
	}
	for strings.HasPrefix(token, "../") {
		token = strings.TrimPrefix(token, "../")
	}
	return strings.TrimPrefix(token, "memory/")
}

func markExtractLinks(files []extractFile, byRef map[string][]int, source, body, dir string, roots []string) {
	for _, token := range extractTokens(body) {
		for _, ref := range []string{extractLinkRef(token, roots), extractLinkRef(path.Join(dir, token), roots)} {
			for _, i := range byRef[ref] {
				if files[i].row.Path != source && !slices.Contains(files[i].row.Signals.LinkedBy, source) {
					files[i].row.Signals.LinkedBy = append(files[i].row.Signals.LinkedBy, source)
				}
			}
		}
	}
}

func extractRecall(files []extractFile, opts ExtractOptions) error {
	days := map[string]map[string]bool{}
	newest := map[string]time.Time{}
	for _, log := range opts.AccessLogs {
		err := readExtractAccess(log, func(ts time.Time, entry, reason string) {
			if reason != accessRecall || ts.After(opts.Now) {
				return
			}
			if days[entry] == nil {
				days[entry] = map[string]bool{}
			}
			days[entry][ts.UTC().Format(dateLayout)] = true
			if ts.After(newest[entry]) {
				newest[entry] = ts
			}
		})
		if err != nil {
			return err
		}
	}
	for i := range files {
		setExtractRecall(&files[i], days, newest, opts)
	}
	return nil
}

func setExtractRecall(f *extractFile, days map[string]map[string]bool, newest map[string]time.Time, opts ExtractOptions) {
	seen := map[string]bool{}
	s := &f.row.Signals
	s.RecallKnown = len(opts.AccessLogs) > 0
	for _, ref := range f.refs {
		for day := range days[ref] {
			seen[day] = true
		}
		if newest[ref].After(s.LastRecalled) {
			s.LastRecalled = newest[ref]
		}
	}
	s.RecallDays = len(seen)
	s.RecalledRecently = !s.LastRecalled.IsZero() && s.LastRecalled.After(opts.Now.AddDate(0, 0, -hotWindowDays))
}

// The existing iterator swallows read errors; reuse its parser but fail closed on unreadable recall evidence.
func readExtractAccess(log string, fn func(time.Time, string, string)) error {
	found := false
	for i := accessGenerations; i >= 0; i-- {
		data, err := readIfExists(accessGeneration(log, i))
		if err != nil {
			return fmt.Errorf("extract access log %s: %w", log, err)
		}
		found = found || data != nil
		for line := range strings.SplitSeq(string(data), "\n") {
			if ts, entry, reason, session := parseAccessLine(line); !ts.IsZero() && session == "" {
				fn(ts, entry, reason)
			}
		}
	}
	if !found {
		return fmt.Errorf("extract access log %s: %w", log, fs.ErrNotExist)
	}
	return nil
}

func extractDecide(f *extractFile, facts ExtractFacts) error {
	r, s := &f.row, &f.row.Signals
	switch {
	case extractLinkSource(r.Path), r.Path == catalogFile, !strings.HasSuffix(r.Path, ".md"):
		r.Action, r.Reason = ExtractKeep, ExtractProtected
	case len(s.LinkedBy) > 0:
		r.Action, r.Reason = ExtractKeep, ExtractLinked
	case s.Pinned:
		r.Action, r.Reason = ExtractKeep, ExtractPinned
	case s.RecalledRecently:
		r.Action, r.Reason = ExtractKeep, ExtractRecalled
	case s.YoungerThan7Days:
		r.Action, r.Reason = ExtractKeep, ExtractYoung
	case len(f.data) == 0:
		r.Action, r.Reason = ExtractPurge, ExtractEmpty
	case facts != nil:
		return extractGitFact(f, facts)
	}
	return nil
}

func extractGitFact(f *extractFile, facts ExtractFacts) error {
	s := &f.row.Signals
	var err error
	switch f.row.Folder {
	case "worktrees":
		s.BranchExists, s.BranchKnown, err = facts.WorktreeBranchExists(f.row.Path, slices.Clone(f.data))
		if err == nil && s.BranchKnown && !s.BranchExists {
			f.row.Action, f.row.Reason = ExtractPurge, ExtractMissingBranch
		}
	case "reports":
		s.ResultInGit, s.ResultKnown, err = facts.ReportResultInGit(f.row.Path, slices.Clone(f.data))
		if err == nil && s.ResultKnown && s.ResultInGit {
			f.row.Action, f.row.Reason = ExtractPurge, ExtractResultInGit
		}
	}
	if err != nil {
		return fmt.Errorf("extract facts %s: %w", f.row.Path, err)
	}
	return nil
}

func extractCanonical(files []extractFile) map[string]int {
	canonical := map[string]int{}
	for i, f := range files {
		if f.row.Action == ExtractPurge {
			continue
		}
		j, ok := canonical[f.row.SHA256]
		if !ok || f.row.Action == ExtractKeep && files[j].row.Action != ExtractKeep {
			canonical[f.row.SHA256] = i
		}
	}
	return canonical
}

func extractDuplicates(files []extractFile) {
	canonical := extractCanonical(files)
	for i := range files {
		r := &files[i].row
		j, ok := canonical[r.SHA256]
		if !ok || i == j || r.Action != ExtractRewrite || !r.Signals.RecallKnown {
			continue
		}
		r.Action, r.Reason, r.DuplicateOf = ExtractPurge, ExtractDuplicate, files[j].row.Path
		if files[j].row.Action == ExtractRewrite {
			files[j].row.Action, files[j].row.Reason = ExtractKeep, ExtractDuplicateSource
		}
	}
}

func extractResult(files []extractFile) ExtractResult {
	folders := map[string]*ExtractFolderTotal{}
	for folder, budget := range extractFolderBudgets {
		folders[folder] = &ExtractFolderTotal{Folder: folder, Budget: budget, Actions: map[ExtractAction]ExtractTotal{}}
	}
	result := ExtractResult{}
	for _, f := range files {
		r := f.row
		total := folders[r.Folder]
		if total == nil {
			total = &ExtractFolderTotal{Folder: r.Folder, Actions: map[ExtractAction]ExtractTotal{}}
			folders[r.Folder] = total
		}
		total.Files++
		total.Bytes += r.Size
		a := total.Actions[r.Action]
		a.Files++
		a.Bytes += r.Size
		total.Actions[r.Action] = a
		if r.Action != ExtractKeep {
			addExtractGroup(total, r, f.mod)
		}
		result.Rows = append(result.Rows, r)
	}
	for _, total := range folders {
		if total.Budget > 0 {
			total.OverBudget = max(0, total.Files-total.Budget)
			total.RewriteExcess = max(0, total.Files-total.Actions[ExtractPurge].Files-total.Budget)
		}
		slices.SortFunc(total.Groups, compareExtractGroups)
		result.Folders = append(result.Folders, *total)
	}
	slices.SortFunc(result.Folders, func(a, b ExtractFolderTotal) int { return strings.Compare(a.Folder, b.Folder) })
	return result
}

func addExtractGroup(total *ExtractFolderTotal, row ExtractRow, mod time.Time) {
	year, week := mod.UTC().ISOWeek()
	key := fmt.Sprintf("%04d-W%02d", year, week)
	i := slices.IndexFunc(total.Groups, func(g ExtractGroup) bool { return g.Week == key && g.Action == row.Action })
	if i < 0 {
		i = len(total.Groups)
		total.Groups = append(total.Groups, ExtractGroup{Week: key, Action: row.Action, YoungestAge: row.Age})
	}
	g := &total.Groups[i]
	g.Paths = append(g.Paths, row.Path)
	g.Bytes += row.Size
	g.YoungestAge = min(g.YoungestAge, row.Age)
	g.RecallDays = max(g.RecallDays, row.Signals.RecallDays)
}

func compareExtractGroups(a, b ExtractGroup) int {
	return cmp.Or(cmp.Compare(b.YoungestAge, a.YoungestAge), cmp.Compare(a.RecallDays, b.RecallDays),
		strings.Compare(a.Week, b.Week), strings.Compare(string(a.Action), string(b.Action)))
}
