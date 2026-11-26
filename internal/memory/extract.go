package memory

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

type ExtractEntry struct {
	Title   string   `json:"title"`
	Text    string   `json:"text"`
	Sources []string `json:"sources"`
}

type ExtractRewriteAnswer struct {
	Entries []ExtractEntry `json:"entries"`
	Purge   []string       `json:"purge"`
}

var (
	extractIDPattern    = regexp.MustCompile(`z?[0-9a-f]{8}`)
	extractDatePattern  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	extractPathPattern  = regexp.MustCompile(`(?:/Users/|/home/|/tmp/|/var/|/etc/|/opt/|~/|\$HOME/)[^\s\x60"'<>()[\]{}*|#]+`)
	extractSHAPattern   = regexp.MustCompile(`[0-9a-f]{7,40}`)
	extractQuotePattern = regexp.MustCompile(`"[^"]+"|\x{201c}[^\x{201d}]+\x{201d}|(?:^|[\s(])'[^'\n]{3,}'`)
	extractTickPattern  = regexp.MustCompile("`[^`\\n]+`")
	extractOpenPattern  = regexp.MustCompile(`^\s*(?:- \[ \]|NEXT:)|TODO|UNRESOLVED`)
)

// CheckExtractLoss refuses a rewrite that drops source facts needed for recall.
func CheckExtractLoss(sources map[string][]byte, ans ExtractRewriteAnswer, live func(string) bool) error {
	entries := make(map[string]string, len(sources))
	for _, entry := range ans.Entries {
		for _, source := range entry.Sources {
			entries[source] += "\n" + entry.Text
		}
	}
	paths := make([]string, 0, len(sources))
	for source := range sources {
		paths = append(paths, source)
	}
	slices.Sort(paths)
	for _, source := range paths {
		purged := slices.Contains(ans.Purge, source)
		body := string(sources[source])
		if err := extractLossTokens(source, body, entries[source], purged, live); err != nil {
			return err
		}
		if err := extractLossLines(source, body, entries[source], purged); err != nil {
			return err
		}
	}
	return nil
}

func extractLossTokens(source, body, entry string, purged bool, live func(string) bool) error {
	for _, rule := range []struct {
		kind string
		re   *regexp.Regexp
	}{
		{"id", extractIDPattern}, {"date", extractDatePattern},
		{"path", extractPathPattern}, {"commit sha", extractSHAPattern},
		{"search term", extractTickPattern},
	} {
		for _, match := range rule.re.FindAllStringIndex(body, -1) {
			item := extractLossItem(body, match, rule.kind)
			if !extractLossToken(body, match, rule.kind, live) {
				continue
			}
			if purged || !strings.Contains(entry, item) {
				return fmt.Errorf("extract loss %s: dropped %s %q", source, rule.kind, item)
			}
		}
	}
	return nil
}

// extractLiveSHA is true for a token with a digit, or a prefix of an object in the repo.
func extractLiveSHA(repo storeRepo) (func(string) bool, error) {
	out, err := repo.run("cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
	names := splitLines(out)
	return func(item string) bool {
		return strings.ContainsAny(item, "0123456789") ||
			slices.ContainsFunc(names, func(name string) bool { return strings.HasPrefix(name, item) })
	}, err
}

func extractLossItem(body string, match []int, kind string) string {
	item := body[match[0]:match[1]]
	if kind == "search term" {
		return strings.Trim(item, "`")
	}
	if kind != "path" {
		return item
	}
	return extractPathItem(body, match, item)
}

func extractPathItem(body string, match []int, item string) string {
	line := body[strings.LastIndex(body[:match[0]], "\n")+1 : match[0]]
	strike := strings.Contains(line, "~~") || strings.HasSuffix(line, "~") && strings.HasPrefix(item, "~/Users/")
	if strike && strings.HasSuffix(item, "~~") {
		item = strings.TrimSuffix(item, "~~")
	}
	if strings.HasSuffix(line, "~") && strings.HasPrefix(item, "~/Users/") {
		item = strings.TrimPrefix(item, "~")
	}
	if strings.Contains(line, "_") && strings.HasSuffix(item, "_") {
		item = strings.TrimSuffix(item, "_")
	}
	return strings.TrimRight(item, ".,:;!?")
}

func extractLossToken(body string, match []int, kind string, live func(string) bool) bool {
	item := body[match[0]:match[1]]
	if kind == "search term" {
		return true
	}
	if kind == "date" {
		return true
	}
	if !extractTokenBounded(body, match) {
		return false
	}
	if kind != "commit sha" {
		return true
	}
	if len(item) == 8 {
		return false
	}
	if live != nil {
		return live(item)
	}
	return strings.ContainsAny(item, "0123456789")
}

func extractTokenBounded(body string, match []int) bool {
	if match[0] > 0 && extractWordByte(body[match[0]-1]) {
		return false
	}
	return match[1] == len(body) || !extractWordByte(body[match[1]])
}

func extractWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func extractLossLines(source, body, entry string, purged bool) error {
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		kind := ""
		switch {
		case strings.HasPrefix(trimmed, ">"):
			kind = "owner words"
		case strings.HasPrefix(trimmed, "DECISION:"):
			kind = "decision"
		case extractOpenPattern.MatchString(line):
			kind = "open item"
		}
		if kind != "" && (purged || !strings.Contains(entry, line)) {
			return fmt.Errorf("extract loss %s: dropped %s %q", source, kind, line)
		}
		if err := extractOwnerQuoteLoss(source, line, entry, purged); err != nil {
			return err
		}
	}
	return nil
}

func extractOwnerQuoteLoss(source, line, entry string, purged bool) error {
	if !strings.Contains(strings.ToLower(line), "owner") {
		return nil
	}
	for _, quote := range extractQuotePattern.FindAllString(line, -1) {
		quote = strings.TrimLeft(quote, " \t(")
		if purged || !strings.Contains(entry, quote) {
			return fmt.Errorf("extract loss %s: dropped owner quote %q", source, quote)
		}
	}
	return nil
}

// ExtractRewriteSchema types a group's replacement entries and explicitly purged sources.
func ExtractRewriteSchema() string {
	entry := schemaObj{"type": "object", "additionalProperties": false,
		"required": []string{"title", "text", "sources"}, "properties": schemaObj{
			"title": schemaObj{"type": "string", "minLength": 1}, "text": schemaObj{"type": "string", "minLength": 1},
			"sources": schemaObj{"type": "array", "minItems": 1, "items": schemaObj{"type": "string"}},
		}}
	return jsonString(schemaObj{"type": "object", "additionalProperties": false,
		"required": []string{"entries", "purge"}, "properties": schemaObj{
			"entries": schemaObj{"type": "array", "minItems": 1, "items": entry},
			"purge":   schemaObj{"type": "array", "items": schemaObj{"type": "string"}},
		}})
}

type extractPromptFile struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// RewriteExtractGroup asks code mode for a typed replacement without changing the store.
func RewriteExtractGroup(ctx context.Context, storeRoot string, group ExtractGroup, modelName string) (rewrite ExtractRewriteAnswer, err error) {
	if group.Action != ExtractRewrite || len(group.Paths) == 0 {
		return rewrite, errors.New("extract rewrite requires a nonempty rewrite group")
	}
	if CodeModeRun == nil {
		return rewrite, errors.New("no code-mode agent registered")
	}
	model, err := FallbackModel(modelName)
	if err != nil {
		return rewrite, fmt.Errorf("extract rewrite model: %w", err)
	}
	root, err := os.OpenRoot(storeRoot)
	if err != nil {
		return rewrite, fmt.Errorf("extract rewrite store: %w", err)
	}
	defer root.Close()
	files, allowed, err := extractPromptFiles(root, group.Paths)
	if err != nil {
		return rewrite, err
	}
	data, err := json.Marshal(files)
	if err != nil {
		return rewrite, fmt.Errorf("extract rewrite prompt: %w", err)
	}
	prompt := "Consolidate these memory files into entries. Keep the owner's quotes, decisions, and open tasks verbatim. " +
		"Each entry must name its source paths; explicitly purge only files with no facts to retain. " +
		"Write nothing and return only the typed JSON answer. Files: " + string(data)
	answer, who, err := runExtractRewrite(ctx, prompt, model)
	if err != nil {
		return rewrite, fmt.Errorf("extract rewrite on %s: %w", who, err)
	}
	return decodeExtractRewrite(answer, allowed)
}

func runExtractRewrite(ctx context.Context, prompt, model string) (answer, who string, err error) {
	dir, err := os.MkdirTemp("", "memory-extract-")
	if err != nil {
		return "", "", fmt.Errorf("extract rewrite workdir: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			err = errors.Join(err, removeErr)
		}
	}()
	return CodeModeRun(ctx, prompt, dir, ExtractRewriteSchema(), model)
}

func extractPromptFiles(root *os.Root, paths []string) ([]extractPromptFile, map[string]bool, error) {
	files, allowed := make([]extractPromptFile, 0, len(paths)), make(map[string]bool, len(paths))
	for _, rel := range paths {
		if !fs.ValidPath(rel) || rel == "." || strings.Contains(rel, "\\") || allowed[rel] {
			return nil, nil, fmt.Errorf("extract rewrite invalid group path %q", rel)
		}
		allowed[rel] = true
		data, err := root.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			return nil, nil, fmt.Errorf("extract rewrite read %s: %w", rel, err)
		}
		files = append(files, extractPromptFile{Path: rel, Text: string(data)})
	}
	return files, allowed, nil
}

func decodeExtractRewrite(answer string, allowed map[string]bool) (ExtractRewriteAnswer, error) {
	var result ExtractRewriteAnswer
	decoder := json.NewDecoder(strings.NewReader(answer))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return ExtractRewriteAnswer{}, fmt.Errorf("extract rewrite answer: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ExtractRewriteAnswer{}, errors.New("extract rewrite answer has trailing content")
	}
	if len(result.Entries) == 0 || result.Purge == nil {
		return ExtractRewriteAnswer{}, errors.New("extract rewrite needs entries and purge")
	}
	for i := range result.Entries {
		result.Entries[i].Title = strings.TrimSpace(result.Entries[i].Title)
		result.Entries[i].Text = strings.TrimSpace(result.Entries[i].Text)
	}
	if err := validateExtractRewrite(result, allowed); err != nil {
		return ExtractRewriteAnswer{}, err
	}
	return result, nil
}

func validateExtractRewrite(result ExtractRewriteAnswer, allowed map[string]bool) error {
	seen := make(map[string]bool, len(allowed))
	for _, entry := range result.Entries {
		if strings.TrimSpace(entry.Title) == "" || strings.TrimSpace(entry.Text) == "" || len(entry.Sources) == 0 {
			return errors.New("extract rewrite entry needs title, text, and sources")
		}
		entrySeen := make(map[string]bool, len(entry.Sources))
		for _, source := range entry.Sources {
			if err := checkExtractSource(source, allowed, entrySeen); err != nil {
				return err
			}
			seen[source] = true
		}
	}
	for _, source := range result.Purge {
		if err := checkExtractSource(source, allowed, seen); err != nil {
			return err
		}
	}
	if len(seen) != len(allowed) {
		return errors.New("extract rewrite leaves a group file unaccounted for")
	}
	return nil
}

func checkExtractSource(source string, allowed, seen map[string]bool) error {
	if !allowed[source] {
		return fmt.Errorf("extract rewrite source %q is outside the group", source)
	}
	if seen[source] {
		return fmt.Errorf("extract rewrite source %q is repeated", source)
	}
	seen[source] = true
	return nil
}

// ApplyExtractRewrite replaces one validated group in a single memory-repo commit.
func ApplyExtractRewrite(repo storeRepo, group ExtractGroup, ans ExtractRewriteAnswer, now time.Time) (sha string, err error) {
	if err := extractApplyPreflight(repo, group, now); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(repo.Dir)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	before, after, successors, notes, err := extractApplyPrepare(repo, root, group, ans, now)
	if err != nil {
		return "", err
	}
	written := make(map[string]appliedWrite)
	if err := extractWriteChanges(root, after, before, written); err != nil {
		return "", extractApplyRollback(written, err)
	}
	sha, committed, err := extractCommit(repo, after, group.Paths, notes, successors)
	if err != nil && !committed {
		return "", extractApplyRollback(written, err)
	}
	return sha, err
}

func extractApplyPrepare(repo storeRepo, root *os.Root, group ExtractGroup, ans ExtractRewriteAnswer, now time.Time) (map[string][]byte, map[string][]byte, map[string]string, []string, error) {
	files, allowed, err := extractPromptFiles(root, group.Paths)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if len(ans.Entries) == 0 || ans.Purge == nil {
		return nil, nil, nil, nil, extractChecked{errors.New("extract rewrite needs entries and purge")}
	}
	if err := validateExtractRewrite(ans, allowed); err != nil {
		return nil, nil, nil, nil, extractChecked{err}
	}
	sources := make(map[string][]byte, len(files))
	for _, file := range files {
		sources[file.Path] = []byte(file.Text)
	}
	live, err := extractLiveSHA(repo)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := CheckExtractLoss(sources, ans, live); err != nil {
		return nil, nil, nil, nil, extractChecked{err}
	}
	notes, successors, err := extractNotePaths(repo, root, ans.Entries, now)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	before, after, err := extractApplyChanges(repo, root, group.Paths, successors)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := extractCleanSources(repo, slices.Collect(maps.Keys(before))); err != nil {
		return nil, nil, nil, nil, err
	}
	for i, entry := range ans.Entries {
		before[notes[i]] = nil
		after[notes[i]] = []byte("# " + strings.TrimSpace(entry.Title) + "\n\n" + strings.TrimSpace(entry.Text) + "\n\nSources:\n- " + strings.Join(entry.Sources, "\n- ") + "\n")
	}
	return before, after, successors, notes, nil
}

func extractApplyPreflight(repo storeRepo, group ExtractGroup, now time.Time) error {
	if group.Action != ExtractRewrite || len(group.Paths) == 0 || now.IsZero() || !repo.Exists() {
		return errors.New("extract apply requires a rewrite group, time, and memory repo")
	}
	for _, rel := range group.Paths {
		if err := repo.validateRel(rel); err != nil {
			return err
		}
	}
	staged, err := repo.run("diff", "--cached", "--name-only", "-z")
	if err != nil {
		return err
	}
	if staged != "" {
		return errors.New("extract apply requires an unstaged memory index")
	}
	return extractCleanSources(repo, group.Paths)
}

func extractCleanSources(repo storeRepo, rels []string) error {
	dirty, err := repo.run(append([]string{"--no-optional-locks", "status", "--porcelain", "--"}, rels...)...)
	if err != nil {
		return err
	}
	if dirty != "" {
		return fmt.Errorf("extract apply refuses uncommitted source edits: %s", strings.ReplaceAll(dirty, "\n", "; "))
	}
	return nil
}

func extractCommit(repo storeRepo, after map[string][]byte, sources, notes []string, successors map[string]string) (string, bool, error) {
	var body strings.Builder
	for _, source := range sources {
		target := successors[source]
		if target == "" {
			target = "(purged)"
		}
		fmt.Fprintf(&body, "%s -> %s\n", source, target)
	}
	rels := make([]string, 0, len(after))
	for rel := range after {
		rels = append(rels, rel)
	}
	slices.Sort(rels)
	return repo.Commit(rels, fmt.Sprintf("memory: extract %d files into %d notes", len(sources), len(notes)), body.String())
}

func extractNotePaths(repo storeRepo, root *os.Root, entries []ExtractEntry, now time.Time) ([]string, map[string]string, error) {
	notes := make([]string, 0, len(entries))
	successors := make(map[string]string)
	used := make(map[string]bool)
	for _, entry := range entries {
		base := "notes/extract-" + now.UTC().Format(dateLayout) + "-" + extractSlug(entry.Title)
		for i := 1; ; i++ {
			rel := base + ".md"
			if i > 1 {
				rel = fmt.Sprintf("%s-%d.md", base, i)
			}
			_, err := root.Lstat(rel)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, nil, err
			}
			if used[rel] || err == nil {
				continue
			}
			if err := repo.validateRel(rel); err != nil {
				return nil, nil, err
			}
			used[rel] = true
			notes = append(notes, rel)
			for _, source := range entry.Sources {
				successors[source] = rel
			}
			break
		}
	}
	return notes, successors, nil
}

func extractSlug(title string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(title) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			lastDash = false
		} else if b.Len() > 0 && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "note"
	}
	if len(slug) > 80 {
		slug = strings.TrimRight(slug[:80], "-")
	}
	return slug
}

func extractApplyChanges(repo storeRepo, root *os.Root, sources []string, successors map[string]string) (map[string][]byte, map[string][]byte, error) {
	before, after := make(map[string][]byte), make(map[string][]byte)
	for _, rel := range sources {
		data, err := extractReadRegular(root, rel)
		if err != nil {
			return nil, nil, fmt.Errorf("extract source %s: %w", rel, err)
		}
		before[rel], after[rel] = data, nil
	}
	out, err := repo.run("ls-files", "-z")
	if err != nil {
		return nil, nil, err
	}
	tracked := textutil.SplitNUL(out)
	for _, source := range sources {
		if !slices.Contains(tracked, source) {
			return nil, nil, fmt.Errorf("extract source %s is not tracked", source)
		}
	}
	if err := extractRepointTracked(repo, root, tracked, before, after, successors); err != nil {
		return nil, nil, err
	}
	return before, after, nil
}

func extractRepointTracked(repo storeRepo, root *os.Root, tracked []string, before, after map[string][]byte, successors map[string]string) error {
	for _, rel := range tracked {
		if !strings.HasSuffix(rel, ".md") {
			continue
		}
		if _, removed := before[rel]; removed {
			continue
		}
		if err := repo.validateRel(rel); err != nil {
			return err
		}
		data, err := extractReadRegular(root, rel)
		if err != nil {
			return fmt.Errorf("extract link source %s: %w", rel, err)
		}
		updated := extractRepoint(rel, string(data), successors)
		if updated != string(data) {
			before[rel], after[rel] = data, []byte(updated)
		}
	}
	return nil
}

func extractReadRegular(root *os.Root, rel string) ([]byte, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("extract path %s is not a regular file", rel)
	}
	return root.ReadFile(rel)
}

func extractRepoint(rel, body string, successors map[string]string) string {
	sources := make([]string, 0, len(successors))
	for source := range successors {
		sources = append(sources, source)
	}
	slices.SortFunc(sources, func(a, b string) int { return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a, b)) })
	for _, source := range sources {
		target := successors[source]
		relSource, err := filepath.Rel(filepath.Dir(rel), source)
		if err != nil {
			continue
		}
		relTarget, err := filepath.Rel(filepath.Dir(rel), target)
		if err != nil {
			continue
		}
		body = replaceExtractPath(body, source, target)
		body = replaceExtractPath(body, filepath.ToSlash(relSource), filepath.ToSlash(relTarget))
	}
	return body
}

func replaceExtractPath(body, from, to string) string {
	pathByte := func(b byte) bool {
		return b >= 128 || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
			strings.ContainsRune("_./-~%+$", rune(b))
	}
	var out strings.Builder
	for start := 0; ; {
		at := strings.Index(body[start:], from)
		if at < 0 {
			out.WriteString(body[start:])
			return out.String()
		}
		at += start
		end := at + len(from)
		out.WriteString(body[start:at])
		left := at == 0 || !pathByte(body[at-1]) ||
			at >= 2 && body[at-2:at] == "./" && (at == 2 || !pathByte(body[at-3]))
		right := end == len(body) || !pathByte(body[end]) ||
			body[end] == '.' && (end+1 == len(body) || !pathByte(body[end+1]))
		if left && right {
			out.WriteString(to)
		} else {
			out.WriteString(from)
		}
		start = end
	}
}

func extractApplyRollback(written map[string]appliedWrite, cause error) error {
	var rollbackErr error
	for _, w := range written {
		rollbackErr = errors.Join(rollbackErr, w.restoreIfOurs())
	}
	if rollbackErr != nil {
		return &AfterCommitError{errors.Join(cause, rollbackErr)}
	}
	return cause
}

func extractWriteChanges(root *os.Root, changes, before map[string][]byte, written map[string]appliedWrite) error {
	rels := make([]string, 0, len(changes))
	for rel := range changes {
		rels = append(rels, rel)
	}
	slices.Sort(rels)
	for _, rel := range rels {
		if err := extractWriteOne(root, rel, changes[rel], before, written); err != nil {
			return err
		}
	}
	return nil
}

func extractWriteOne(root *os.Root, rel string, data []byte, before map[string][]byte, written map[string]appliedWrite) (err error) {
	snap, err := snapshotFile(filepath.Join(root.Name(), rel))
	if err != nil {
		return err
	}
	if snap.existed != (before[rel] != nil) || !bytes.Equal(snap.prev, before[rel]) {
		return fmt.Errorf("extract: %s changed before write", rel)
	}
	written[rel] = appliedWrite{change: storeChange{Rel: rel, Data: data, Delete: data == nil}, snap: snap}
	if data == nil {
		if cur, err := root.ReadFile(rel); err != nil || !bytes.Equal(cur, snap.prev) {
			return errors.Join(fmt.Errorf("extract: %s changed before removal", rel), err)
		}
		if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("extract remove %s: %w", rel, err)
		}
		return nil
	}
	if err := root.MkdirAll(path.Dir(rel), 0o700); err != nil {
		return fmt.Errorf("extract mkdir %s: %w", rel, err)
	}
	mode := cmp.Or(snap.mode, os.FileMode(0o600))
	tmp := rel + ".tmp-" + rand.Text()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("extract write %s: %w", rel, err)
	}
	defer func() {
		if removeErr := root.Remove(tmp); !errors.Is(removeErr, fs.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Chmod(mode), f.Sync(), f.Close()); err != nil {
		return err
	}
	cur, readErr := root.ReadFile(rel)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return readErr
	}
	if (readErr == nil) != snap.existed || !bytes.Equal(cur, snap.prev) {
		return fmt.Errorf("extract: %s changed before install", rel)
	}
	if !snap.existed {
		return root.Link(tmp, rel)
	}
	return root.Rename(tmp, rel)
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
	var found []fs.DirEntry
	var rels []string
	walkErr := fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("extract walk %s: %w", rel, err)
		}
		if d.IsDir() {
			if skipExtractDir(rel, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !skipExtractFile(rel, d) {
			found, rels = append(found, d), append(rels, rel)
		}
		return nil
	})
	files := make([]extractFile, len(rels))
	err := extractEach(len(rels), func(i int) error {
		data, err := root.ReadFile(filepath.FromSlash(rels[i]))
		if err != nil {
			return fmt.Errorf("extract read %s: %w", rels[i], err)
		}
		info, err := found[i].Info()
		if err != nil {
			return fmt.Errorf("extract stat %s: %w", rels[i], err)
		}
		files[i] = newExtractFile(rels[i], data, info.ModTime(), now)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, walkErr
}

// extractEach runs fn over 0..n-1 on all CPUs and returns the lowest index's error, as a sequential loop would.
func extractEach(n int, fn func(i int) error) error {
	errs := make([]error, n)
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(n, runtime.GOMAXPROCS(0)) {
		wg.Go(func() {
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				errs[i] = fn(i)
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
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
