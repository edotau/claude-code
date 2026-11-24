package memory

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// RememberTypes maps --type to its file; a feedback-<class> type maps to feedbackDir, a note of its own.
var RememberTypes = map[string]string{
	"project":   "decisionLog.md",
	"user":      "conventions.md",
	"feedback":  "conventions.md",
	"reference": "conventions.md",
}

// FeedbackClasses are the classes of a feedback note, each with the sentence that tells the model when to pick it.
var FeedbackClasses = map[string]string{
	"correction": "the owner corrected an action, claim or output",
	"preference": "the owner stated how they want something done",
	"process":    "the owner changed a workflow, order or approval step",
	"tooling":    "the owner reported a tool, hook or verb misbehaving or missing",
}

const feedbackDir = "feedback/"

func init() {
	for c := range FeedbackClasses {
		RememberTypes["feedback-"+c] = feedbackDir
	}
}

// RememberTypeHelp lists the --type values; the feedback classes carry their when-to-pick sentence.
func RememberTypeHelp() string {
	var b strings.Builder
	b.WriteString("user, feedback, project, reference")
	for _, c := range slices.Sorted(maps.Keys(FeedbackClasses)) {
		b.WriteString("; feedback-" + c + " (a feedback/ note: " + FeedbackClasses[c] + ")")
	}
	return b.String()
}

// afterRememberWrite runs between Remember's stamped write and its settle, under the lock; tests race a rival there.
var afterRememberWrite = func() {}

// Remember writes fact under kind's file, settles to stamp an id, and returns that id.
func Remember(kind, fact string, now time.Time) (string, error) {
	return RememberSupersedes(kind, fact, "", now)
}

// RememberSupersedes is Remember for a fact that replaces the live entry old: Go writes the `Supersedes` edge.
func RememberSupersedes(kind, fact, old string, now time.Time) (string, error) {
	file, ok := RememberTypes[kind]
	if !ok {
		return "", fmt.Errorf("--type %q: want %s", kind, RememberTypeHelp())
	}
	fact = strings.Join(strings.Fields(fact), " ") // one line: a newline plus "## date — x" would stamp a second entry
	if fact == "" {
		return "", errors.New("empty fact")
	}
	if file == feedbackDir {
		if old != "" {
			return "", errors.New("--supersedes: a feedback-<class> note replaces nothing; use --type feedback")
		}
		return rememberNote(strings.TrimPrefix(kind, "feedback-"), fact, now)
	}
	if old != "" { // before EnsureBank, so a refused id leaves no store behind
		var err error
		if old, err = liveEntryID(old); err != nil {
			return "", err
		}
	}
	err := EnsureBank("")
	if err != nil {
		return "", err
	}
	var id string
	err = withFlatLock(BankLockWait, func() error {
		var werr error
		id, werr = rememberLocked(file, kind, fact, old, now)
		return werr
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// rememberLocked is RememberSupersedes' write under the bank lock; every touched bank file rolls back on error.
func rememberLocked(file, kind, fact, old string, now time.Time) (string, error) {
	if old != "" { // a peer may have replaced it since the check before the lock
		if _, err := liveEntryID(old); err != nil {
			return "", err
		}
	}
	snap, err := snapshotPaths(Path(file), Path("decisionLog.md"), Path("conventions.md"))
	if err != nil {
		return "", err
	}
	before, _ := StoreRepo().headSHA()
	id, err := writeRememberFact(file, kind, fact, old, now)
	if err != nil {
		return "", err
	}
	err = errors.Join(stampAndSettle(file, now), requireReplaced(old, id))
	if err == nil {
		return id, nil
	}
	if rerr := restoreSnaps(snap); rerr != nil { // half restored: keep the settle commit as the recovery point
		return id, errors.Join(err, fmt.Errorf("roll back the write: %w", rerr))
	}
	err = fmt.Errorf("%w (write rolled back)", err)
	return id, errors.Join(err, dropCommitsSince(before, "settle: "+stateKey(), err))
}

// dropCommitsSince undoes the one commit settle made on before once the write failed, the files being restored already.
func dropCommitsSince(before, subject string, failed error) error {
	repo := StoreRepo()
	head, ok := repo.headSHA()
	if failed == nil || !ok || before == "" || head == before {
		return nil
	}
	n, err := repo.run("rev-list", "--count", before+".."+head)
	if err != nil || strings.TrimSpace(n) != "1" {
		return nil // a peer committed too, or HEAD moved off the pre-write commit: not ours to move
	}
	if got, err := repo.run("log", "-1", "--format=%s", head); err != nil || strings.TrimSpace(got) != subject {
		return nil
	}
	_, err = repo.run("reset", "--quiet", before)
	return err
}

// rememberNote writes a feedback/ note for class, indexes it and settles.
func rememberNote(class, fact string, now time.Time) (string, error) {
	if err := EnsureBank(""); err != nil {
		return "", err
	}
	var rel string
	err := withFlatLock(BankLockWait, func() error {
		var werr error
		rel, werr = writeNoteLocked(class, fact, now)
		return werr
	})
	return rel, err
}

// writeNoteLocked keeps the note and settle output on a failed settle: a rollback cannot tell a peer's bytes.
func writeNoteLocked(class, fact string, now time.Time) (string, error) {
	slug := noteSlug(fact)
	dir := paths.MemorySubDir("feedback")
	for n := 2; statOK(filepath.Join(dir, slug+".md")); n++ { // a taken name is never overwritten
		slug = noteSlug(fact) + "-" + strconv.Itoa(n)
	}
	rel := "feedback/" + slug + ".md"
	path := filepath.Join(dir, slug+".md")
	desc := strconv.Quote(textutil.Truncate(fact, 200))
	note := "---\nname: " + slug + "\ndescription: " + desc + "\nmetadata:\n  type: feedback\n  class: " + class +
		"\n  modified: " + now.UTC().Format(time.RFC3339) + "\n---\n\n" + fact + "\n"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := paths.AtomicWrite(path, []byte(note), 0o600); err != nil {
		return "", err
	}
	pointer := "- [" + slug + "](" + rel + ") — " + textutil.Truncate(fact, 160)
	_, err := settleBankWith(now, []string{pointer}, "memory: feedback "+slug)
	if err = tolerateIndex(err); err != nil {
		return rel, fmt.Errorf("%s written, settle failed (kept): %w", rel, err)
	}
	return rel, nil
}

// noteSlug is feedback-<first words of fact>: lower-case ASCII words joined by "-", at most 60 columns.
func noteSlug(fact string) string {
	words := strings.FieldsFunc(strings.ToLower(fact), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	slug := "feedback"
	for _, w := range words {
		if len(slug)+1+len(w) > 60 {
			break
		}
		slug += "-" + w
	}
	return slug
}

// liveEntryID resolves ref to the persisted id of a not-yet-replaced entry of decisionLog.md or conventions.md.
func liveEntryID(ref string) (string, error) {
	path, block, err := ShowBlock(ref)
	if errors.Is(err, errNoBlock) {
		return "", fmt.Errorf("--supersedes %q: no such entry (see memory search)", ref)
	}
	if err != nil {
		return "", err
	}
	if path != Path("decisionLog.md") && path != Path("conventions.md") {
		return "", fmt.Errorf("--supersedes %q: not an entry of decisionLog.md or conventions.md", ref)
	}
	if partlyStruck(textutil.FirstLine(block)) { // settle never replaces a half-valid entry
		return "", fmt.Errorf("--supersedes %q: the entry is partly struck, so it cannot be replaced", ref)
	}
	if by := replacedBy(block); by != "" {
		return "", fmt.Errorf("--supersedes %q: already replaced by %s; supersede that entry instead", ref, by)
	}
	id := ownerID(block)
	if id == "" {
		return "", fmt.Errorf("--supersedes %q: the entry has no id yet; run memory index first", ref)
	}
	return id, nil
}

// requireReplaced fails unless settle pointed entry old at newID: a hidden or malformed edge must not land.
func requireReplaced(old, newID string) error {
	if old == "" {
		return nil
	}
	_, block, err := ShowBlock(old)
	if err != nil {
		return err
	}
	if replacedBy(block) != newID {
		return fmt.Errorf("--supersedes %q: settle did not replace it", old)
	}
	return nil
}

// stampAndSettle stamps the other id files and settles, inside Remember's critical section: no writer slips in.
func stampAndSettle(file string, now time.Time) error {
	// Stamp ids directly: a fresh bank must get its id back to round-trip through `memory recall`.
	for name, prefix := range idFiles {
		if name == file { // already stamped in memory before its one write
			continue
		}
		if _, _, err := assignFileIDs(name, prefix, now); err != nil {
			return err
		}
	}
	afterRememberWrite()
	return settleLockedBank(now)
}

// writeRememberFact adds fact to the file, stamps its id in memory, writes the file once, and returns the id.
func writeRememberFact(file, kind, fact, old string, now time.Time) (string, error) {
	path := Path(file)
	data, err := readFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err // never rewrite the file from an unread one
	}
	line, text := rememberedText(path, file, kind, fact, old, string(data), now)
	text, _ = stampIDs(file, idFiles[file], text, now)
	id, err := lastStampedID(text, line, file == "decisionLog.md")
	if err != nil { // an unclosed fence above it or an id marker inside the fact: refuse before writing
		return "", fmt.Errorf("%s: the fact would get no id of its own: %w", file, err)
	}
	if err := paths.AtomicWrite(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// rememberedText is body with fact added: a dated heading on top of decisionLog.md, else a labeled bullet at the end.
func rememberedText(path, file, kind, fact, old, body string, now time.Time) (line, text string) {
	if strings.TrimSpace(body) == "" { // a missing typed file starts from its scaffold, never appendBlocks' archive header
		body = templates()[file]
	}
	if file == "decisionLog.md" {
		line = "## " + now.Format("2006-01-02") + " — " + fact + supersedesLine(old, "")
		header, blocks := entryBlocks(file, body) // newest-first: the new entry goes on top
		return line, rejoin(header, append([]string{line}, blocks...))
	}
	line = "- " + strings.ToUpper(kind[:1]) + kind[1:] + ": " + fact + supersedesLine(old, "  ")
	return line, appendedText(path, body, []string{line})
}

// supersedesLine is the body line settle reads as a replacement edge to old ("" when old is empty).
func supersedesLine(old, indent string) string {
	if old == "" {
		return ""
	}
	return "\n" + indent + "Supersedes " + old + "."
}

// lastStampedID finds the id stamped on line's first line in text: first match if newest-first, else the last.
func lastStampedID(text, line string, newestFirst bool) (string, error) {
	want, _, _ := strings.Cut(line, "\n")
	want = strings.TrimRight(want, " \t")
	lines := strings.Split(text, "\n")
	if !newestFirst {
		slices.Reverse(lines)
	}
	for _, ln := range lines {
		if id := blockID(ln); id != "" && strings.TrimRight(stripID(ln), " \t") == want {
			return id, nil
		}
	}
	return "", errors.New("id not found after settle")
}

// ReportInput is the JSON `memory report` reads: one markdown report and the decision lines it produced.
type ReportInput struct {
	Report    string   `json:"report"`
	Decisions []string `json:"decisions"`
}

// now is the clock seam for the dated report file names.
var now = time.Now

var labelUnsafe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`[^A-Za-z0-9._-]`)
})

// appendReport appends in's report to reports/<date>-<label>.md and its decisions to <date>-decisions.md; returns both.
func appendReport(label string, in ReportInput) ([]string, error) {
	if label == "" {
		return nil, errors.New("--label is required")
	}
	if strings.TrimSpace(in.Report) == "" {
		return nil, errors.New("report text is empty")
	}
	label = labelUnsafe().ReplaceAllString(label, "-")
	if strings.EqualFold(label, "decisions") { // would collide with the <date>-decisions.md file
		return nil, errors.New("--label decisions is reserved")
	}
	var out []string
	err := withFlatLock(BankLockWait, func() error {
		var err error
		out, err = appendReportLocked(label, in)
		return err
	})
	return out, err
}

// appendReportLocked is appendReport's body; the caller holds the bank lock.
func appendReportLocked(label string, in ReportInput) ([]string, error) {
	if err := os.MkdirAll(paths.MemorySubDir("reports"), 0o700); err != nil {
		return nil, err
	}
	day := now().Format("2006-01-02")
	reportPath := paths.MemorySub("reports", day+"-"+label+".md")
	decisionsPath := paths.MemorySub("reports", day+"-decisions.md")
	report, err := snapshotFile(reportPath)
	if err != nil {
		return nil, err
	}
	decisions, err := snapshotFile(decisionsPath)
	if err != nil {
		return nil, err
	}
	// rollback undoes both appends so a retry never writes the report or its decisions twice.
	rollback := func(err error) ([]string, error) {
		return nil, errors.Join(err, report.restore(), decisions.restore())
	}
	if err := appendText(reportPath, "", strings.TrimRight(in.Report, "\n")+"\n\n"); err != nil {
		return rollback(err)
	}
	if err := appendText(decisionsPath, "# Decisions\n\n", decisionLines(day, label, in.Decisions)); err != nil {
		return rollback(err)
	}
	if _, err := formatFiles([]string{reportPath, decisionsPath}, false); err != nil {
		return rollback(err)
	}
	if err := commitStore("memory: report "+label, storeRels([]string{reportPath, decisionsPath})...); err != nil {
		return rollback(err)
	}
	return []string{reportPath, decisionsPath}, nil
}

// decisionLines renders one `- <date> · <label> · <decision>` line per non-blank decision, newline-flattened.
func decisionLines(day, label string, decisions []string) string {
	var b strings.Builder
	flat := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")
	for _, d := range decisions {
		text := strings.TrimSpace(flat.Replace(d))
		if text == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(day)
		b.WriteString(" · ")
		b.WriteString(label)
		b.WriteString(" · ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	return b.String()
}

// appendText appends text to path, starting a new file with header; the file is rewritten atomically.
func appendText(path, header, text string) error {
	cur, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		cur = []byte(header)
	} else if err != nil {
		return err
	}
	if len(cur) > 0 && cur[len(cur)-1] != '\n' {
		cur = append(cur, '\n') // keep the new text on its own line
	}
	return paths.AtomicWrite(path, append(cur, text...), 0o600)
}

// ReportKinds are the `memory report --kind` values.
var ReportKinds = []string{"review", "implementation", "audit", "research"}

// ReadReportBody reads the raw report text from file, or from stdin when stdin is set.
func ReadReportBody(file string, stdin bool) ([]byte, error) {
	if stdin {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(file)
}

// JobReport writes body to a new reports/<day>-<kind>-<label>.md (a numeric suffix if taken) and returns its path.
func JobReport(day time.Time, kind, label string, body []byte) (string, error) {
	if strings.TrimSpace(string(body)) == "" {
		return "", errors.New("report text is empty")
	}
	if err := EnsureBank(""); err != nil {
		return "", err
	}
	label = strings.Trim(labelUnsafe().ReplaceAllString(label, "-"), "-.")
	label = strings.Trim(label[:min(len(label), 60)], "-.") // ASCII only here, so the cut is safe
	stem := day.Format("2006-01-02") + "-" + kind + "-" + cmp.Or(label, "report")
	var path string
	err := withFlatLock(BankLockWait, func() error {
		if err := os.MkdirAll(paths.MemorySubDir("reports"), 0o700); err != nil {
			return err
		}
		var err error
		if path, err = createUnique(stem, append(bytes.TrimRight(body, "\n"), '\n')); err != nil {
			return err
		}
		if _, err := formatFiles([]string{path}, false); err != nil {
			return errors.Join(err, os.Remove(path))
		}
		if err := commitStore("memory: report "+filepath.Base(path), storeRels([]string{path})...); err != nil {
			return errors.Join(err, os.Remove(path))
		}
		return nil
	})
	return path, err
}

// createUnique writes data to reports/<stem>.md, or <stem>-N.md for the first free N, and returns the path.
func createUnique(stem string, data []byte) (string, error) {
	for n := 1; ; n++ {
		path := paths.MemorySub("reports", stem+".md")
		if n > 1 {
			path = paths.MemorySub("reports", fmt.Sprintf("%s-%d.md", stem, n))
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		if err := errors.Join(werr, f.Close()); err != nil {
			return "", errors.Join(err, os.Remove(path))
		}
		return path, nil
	}
}

// ReadReportInput decodes the report JSON from file, or from stdin when stdin is set.
func ReadReportInput(file string, stdin bool) (ReportInput, error) {
	var in ReportInput
	data, err := ReadReportBody(file, stdin)
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return in, fmt.Errorf("bad input JSON: %w", err)
	}
	return in, nil
}
