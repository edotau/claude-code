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
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// ReportInput is the JSON `memory report` reads: one markdown report and the decision lines it produced.
type ReportInput struct {
	Report    string   `json:"report"`
	Decisions []string `json:"decisions"`
}

// now is the clock seam for the dated report file names.
var now = time.Now

var labelUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// AppendReport appends in's report to dir/reports/<date>-<label>.md and each decision to <date>-decisions.md.
// Both files are created when missing (the decisions file even with no decisions); it returns their paths.
func AppendReport(dir, label string, in ReportInput) ([]string, error) {
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

// appendReportLocked is AppendReport's body; the caller holds the bank lock.
func appendReportLocked(dir, label string, in ReportInput) ([]string, error) {
	reports := filepath.Join(dir, "reports")
	if err := os.MkdirAll(reports, 0o700); err != nil {
		return nil, err
	}
	day := now().Format("2006-01-02")
	reportPath := filepath.Join(reports, day+"-"+label+".md")
	decisionsPath := filepath.Join(reports, day+"-decisions.md")
	prev, prevErr := os.ReadFile(reportPath)
	existed := prevErr == nil
	if prevErr != nil && !errors.Is(prevErr, fs.ErrNotExist) {
		return nil, prevErr
	}
	if err := appendText(reportPath, "", strings.TrimRight(in.Report, "\n")+"\n\n"); err != nil {
		return nil, err
	}
	if err := appendText(decisionsPath, "# Decisions\n\n", decisionLines(day, label, in.Decisions)); err != nil {
		// roll the report back so a retry does not append it twice
		return nil, errors.Join(err, restoreReport(reportPath, prev, existed))
	}
	if _, err := formatFiles([]string{reportPath, decisionsPath}, false); err != nil {
		return nil, err
	}
	if err := commitStore("memory: report " + label); err != nil {
		return nil, err
	}
	return []string{reportPath, decisionsPath}, nil
}

// restoreReport undoes the report append: removes the file if it was created, else rewrites its prior bytes.
func restoreReport(path string, prev []byte, existed bool) error {
	if !existed {
		return os.Remove(path)
	}
	return paths.AtomicWrite(path, prev, 0o600)
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
		b.WriteString("- " + day + " · " + label + " · " + text + "\n")
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
