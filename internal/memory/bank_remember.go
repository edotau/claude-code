package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// RememberTypes maps --type to its file: "project" is a dated decisionLog.md entry, the rest conventions.md bullets.
var RememberTypes = map[string]string{
	"project":   "decisionLog.md",
	"user":      "conventions.md",
	"feedback":  "conventions.md",
	"reference": "conventions.md",
}

// afterRememberWrite runs between Remember's stamped write and its settle, under the lock; tests race a rival there.
var afterRememberWrite = func() {}

// Remember writes fact under kind's file, settles to stamp an id, and returns that id.
func Remember(root, kind, fact string, now time.Time) (string, error) {
	file, ok := RememberTypes[kind]
	if !ok {
		return "", fmt.Errorf("--type %q: want user, feedback, project, or reference", kind)
	}
	fact = strings.Join(strings.Fields(fact), " ") // one line: a newline plus "## date — x" would stamp a second entry
	if fact == "" {
		return "", errors.New("empty fact")
	}
	dir, err := EnsureRepoDir(root)
	if err != nil {
		return "", err
	}
	var id string
	err = WithBankLock(BankLockWait, func() error {
		snap, werr := snapshotPaths(filepath.Join(dir, file))
		if werr != nil {
			return werr
		}
		if id, werr = writeRememberFact(dir, file, kind, fact, now); werr != nil {
			return werr
		}
		return restoreOnError(stampAndSettle(root, dir, file, now), snap)
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// stampAndSettle stamps the other id files and settles, inside Remember's critical section: no writer slips in.
func stampAndSettle(root, dir, file string, now time.Time) error {
	// Stamp ids directly: a fresh bank must get its id back to round-trip through `memory recall`.
	for name, prefix := range idFiles {
		if name == file { // already stamped in memory before its one write
			continue
		}
		if _, err := assignFileIDs(dir, name, prefix, now); err != nil {
			return err
		}
	}
	afterRememberWrite()
	return settleLocked(root, now)
}

// writeRememberFact adds fact to dir's file, stamps its id in memory, writes the file once, and returns the id.
func writeRememberFact(dir, file, kind, fact string, now time.Time) (string, error) {
	path := filepath.Join(dir, file)
	data, err := readFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err // never rewrite the file from an unread one
	}
	line, text := rememberedText(path, file, kind, fact, string(data), now)
	text, _ = stampIDs(dir, file, idFiles[file], text, now)
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
func rememberedText(path, file, kind, fact, body string, now time.Time) (line, text string) {
	if strings.TrimSpace(body) == "" { // a missing typed file starts from its scaffold, never appendBlocks' archive header
		body = templates()[file]
	}
	if file == "decisionLog.md" {
		line = "## " + now.Format("2006-01-02") + " — " + fact
		header, blocks := entryBlocks(file, body) // newest-first: the new entry goes on top
		return line, rejoin(header, append([]string{line}, blocks...))
	}
	line = "- " + strings.ToUpper(kind[:1]) + kind[1:] + ": " + fact
	return line, appendedText(path, body, []string{line})
}

// lastStampedID finds the id stamped on line's first line in text: the first exact match in a newest-first file, else the last.
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

var labelUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// appendReport appends in's report to reports/<date>-<label>.md and its decisions to <date>-decisions.md; returns both.
func appendReport(dir, label string, in ReportInput) ([]string, error) {
	if label == "" {
		return nil, errors.New("--label is required")
	}
	if strings.TrimSpace(in.Report) == "" {
		return nil, errors.New("report text is empty")
	}
	label = labelUnsafe.ReplaceAllString(label, "-")
	if strings.EqualFold(label, "decisions") { // would collide with the <date>-decisions.md file
		return nil, errors.New("--label decisions is reserved")
	}
	var out []string
	err := WithBankLock(BankLockWait, func() error {
		var err error
		out, err = appendReportLocked(dir, label, in)
		return err
	})
	return out, err
}

// appendReportLocked is appendReport's body; the caller holds the bank lock.
func appendReportLocked(dir, label string, in ReportInput) ([]string, error) {
	reports := filepath.Join(dir, "reports")
	if err := os.MkdirAll(reports, 0o700); err != nil {
		return nil, err
	}
	day := now().Format("2006-01-02")
	reportPath := filepath.Join(reports, day+"-"+label+".md")
	decisionsPath := filepath.Join(reports, day+"-decisions.md")
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
		return nil, err
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

// ReadReportInput decodes the report JSON from file, or from stdin when stdin is set.
func ReadReportInput(file string, stdin bool) (ReportInput, error) {
	var in ReportInput
	var data []byte
	var err error
	if stdin {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return in, fmt.Errorf("bad input JSON: %w", err)
	}
	return in, nil
}
