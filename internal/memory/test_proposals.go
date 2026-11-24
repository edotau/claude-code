package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// maxTestProposals caps techdebt/tests.jsonl; the oldest lines fall off first.
const maxTestProposals = 40

// TestProposal is one test an agent proposed but did not write.
type TestProposal struct {
	Pkg     string `json:"pkg"`
	Case    string `json:"case"`
	Reason  string `json:"reason"`
	Runtime string `json:"runtime,omitempty"`
	Source  string `json:"source,omitempty"`
}

var proposedFenceRE = regexp.MustCompile("(?s)```proposed_tests\\s*\n(.*?)\n```")

func (p TestProposal) key() string {
	return strings.TrimSpace(p.Pkg) + "\x00" + strings.TrimSpace(p.Case)
}

func parseProposalLine(line string) (TestProposal, error) {
	var p TestProposal
	dec := json.NewDecoder(strings.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return p, errors.New("trailing content after the object")
	}
	if strings.TrimSpace(p.Pkg) == "" || strings.TrimSpace(p.Case) == "" || strings.TrimSpace(p.Reason) == "" {
		return p, errors.New("pkg, case and reason are required")
	}
	return p, nil
}

// ParseTestProposals reads the ```proposed_tests fence of a report: one JSON object per line, source filled in.
func ParseTestProposals(report, source string) ([]TestProposal, error) {
	var out []TestProposal
	for _, m := range proposedFenceRE.FindAllStringSubmatch(report, -1) {
		for _, line := range strings.Split(m[1], "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			p, err := parseProposalLine(line)
			if err != nil {
				return nil, fmt.Errorf("proposed_tests line %q: %w", line, err)
			}
			p.Source = source
			out = append(out, p)
		}
	}
	return out, nil
}

func mergeProposals(add []TestProposal, old []byte) ([]TestProposal, error) {
	seen := map[string]bool{}
	var kept []TestProposal
	push := func(p TestProposal) {
		if k := p.key(); !seen[k] && len(kept) < maxTestProposals {
			seen[k] = true
			kept = append(kept, p)
		}
	}
	for _, p := range add {
		push(p)
	}
	for _, line := range bytes.Split(old, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var p TestProposal
		if err := json.Unmarshal(line, &p); err != nil {
			return nil, fmt.Errorf("existing proposal line %q: %w", line, err)
		}
		push(p)
	}
	return kept, nil
}

// AppendTestProposals puts add first in the file (newest first), drops repeats of pkg+case, keeps 40 lines.
func AppendTestProposals(file string, add []TestProposal) (n int, err error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return 0, err
	}
	err = paths.WithFileLock(file, func() error {
		n, err = appendLocked(file, add)
		return err
	})
	return n, err
}

func appendLocked(file string, add []TestProposal) (int, error) {
	old, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	kept, err := mergeProposals(add, old)
	if err != nil {
		return 0, err
	}
	var buf bytes.Buffer
	for _, p := range kept {
		b, err := json.Marshal(p)
		if err != nil {
			return 0, err
		}
		buf.Write(append(b, '\n'))
	}
	return len(kept), paths.AtomicWrite(file, buf.Bytes(), 0o644)
}
