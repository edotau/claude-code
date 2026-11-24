package agent

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/rtkwrap"
)

// BriefInput supplies the authored task and the leaf's execution contract.
type BriefInput struct {
	Task   string
	Agent  string
	Owned  []string
	Report string
	Verify string
}

// ReviewLoopRule is the standing rule every code-writing dispatch carries.
const ReviewLoopRule = `Review loop (standing rule): after your change, get it reviewed for bugs; fix the findings and review again.
Aim for a review with no CRITICAL, HIGH or MEDIUM and at most 4 LOW (none is better), in at most 3 review rounds;
if the bar is not met, hand back with the last review verbatim for the main session to evaluate.
A test your change breaks: fix it now if small (repair the test only when the test is wrong; never delete or skip
it); if large, hand back naming it for a test-repair leaf.`

// reviewLoopBlock is what WithReviewLoop appends; a replayed prompt drops it so each run decides again.
const reviewLoopBlock = "\n\n" + ReviewLoopRule + "\n"

// WithoutReviewLoop is prompt minus the appended rule: what replay, cutoff keys and task classification read.
func WithoutReviewLoop(prompt string) string { return strings.ReplaceAll(prompt, reviewLoopBlock, "") }

// MarkReviewLoop flags a code-writing candidate by its typed inputs and adds the rule when the first hop can write.
func MarkReviewLoop(req RunOptions) RunOptions {
	req.ReviewLoop = reviewLoopCandidate(req)
	req.Prompt = WithReviewLoop(req)
	return req
}

// reviewLoopCandidate is a dispatch whose own flags and brief ask for code: tools, no read-only brief, no rubric.
func reviewLoopCandidate(req RunOptions) bool {
	p := WithoutReviewLoop(req.Prompt)
	return req.Tools && !req.NoTools && !IsReadOnlyBrief(p) && req.Rubric == "" &&
		!strings.HasPrefix(p, rubricLead) // a rubric run is a review, kept so on replay
}

// WithReviewLoop is req.Prompt plus ReviewLoopRule, once, when the run can write code.
func WithReviewLoop(req RunOptions) string {
	p := WithoutReviewLoop(req.Prompt)
	if strings.Contains(p, ReviewLoopRule) || !reviewLoopCandidate(req) || !writesCode(req) {
		return p
	}
	return p + reviewLoopBlock
}

// hopPrompt is the prompt with the rule iff ask marked it and this hop can write.
func hopPrompt(req RunOptions) string {
	p := WithoutReviewLoop(req.Prompt)
	if !req.ReviewLoop || !req.Tools || req.NoTools || strings.Contains(p, ReviewLoopRule) {
		return p
	}
	return p + reviewLoopBlock
}

// writesCode reports whether req has write tools.
func writesCode(req RunOptions) bool { return req.Tools && !req.NoTools }

//go:embed brief.tmpl
var briefTemplate string

func (input BriefInput) validate() error {
	if len(input.Owned) == 0 {
		return errors.New("implementation brief requires at least one owned path")
	}
	for _, path := range input.Owned {
		if strings.TrimSpace(path) == "" {
			return errors.New("owned path cannot be blank")
		}
	}
	if strings.ContainsAny(input.Agent, "\r\n") {
		return errors.New("agent must fit on one line")
	}
	return nil
}

// RenderBrief resolves report paths against the memory bank.
func RenderBrief(input BriefInput) (string, error) {
	if err := input.validate(); err != nil {
		return "", err
	}
	if input.Owned == nil {
		input.Owned = []string{}
	}
	owned, err := json.Marshal(input.Owned)
	if err != nil {
		return "", fmt.Errorf("encode brief ownership: %w", err)
	}
	tmpl, err := template.New("brief").Parse(briefTemplate)
	if err != nil {
		return "", fmt.Errorf("parse brief template: %w", err)
	}
	var out bytes.Buffer
	data := struct {
		BriefInput
		OwnedLine, ReportsDir string
		Codemode              bool // HARNESS_CODEMODE: the brief's rtk line drops out
	}{input, string(owned), paths.MemorySubDir("reports"), rtkwrap.CodeMode()}
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render brief: %w", err)
	}
	return out.String(), nil
}

// ParseOwned reads the first Owned: line, accepting rendered JSON or legacy whitespace-separated paths.
func ParseOwned(brief string) []string {
	for line := range strings.SplitSeq(brief, "\n") {
		value, ok := strings.CutPrefix(line, "Owned:")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "null" {
			return nil
		}
		if strings.HasPrefix(value, "[") {
			var owned []string
			if json.Unmarshal([]byte(value), &owned) != nil {
				return nil
			}
			return owned
		}
		return strings.Fields(value)
	}
	return nil
}

// IsReadOnlyBrief reports whether the brief's header (before its first blank line) has a Mode: read-only line.
func IsReadOnlyBrief(brief string) bool {
	for line := range strings.SplitSeq(strings.TrimSpace(brief), "\n") {
		if strings.TrimSpace(line) == "" {
			return false
		}
		if v, ok := strings.CutPrefix(line, "Mode:"); ok && strings.HasPrefix(strings.TrimSpace(v), "read-only") {
			return true
		}
	}
	return false
}

// ReadBrief returns the brief file's text; "" is an empty brief and "-" reads stdin.
func ReadBrief(path string) (string, error) {
	switch path {
	case "":
		return "", nil
	case "-":
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

var briefDir = os.TempDir
var briefRandom = rand.Reader
var briefAgent = regexp.MustCompile(`^[a-zA-Z0-9_-]*$`)
var briefName = regexp.MustCompile(`^claude-code-brief-[0-9]{8}(?:-[a-zA-Z0-9_-]+)?-[0-9a-f]{32}\.md$`)

// NewBriefPath reserves an empty private file before returning its absolute path.
func NewBriefPath(name string) (string, error) {
	if !briefAgent.MatchString(name) {
		return "", fmt.Errorf("invalid brief agent %q", name)
	}

	dir, err := filepath.Abs(briefDir())
	if err != nil {
		return "", fmt.Errorf("brief directory: %w", err)
	}
	now := time.Now()
	if err := pruneBriefs(dir, now); err != nil {
		return "", err
	}
	prefix := "claude-code-brief-" + now.Format("20060102")
	if name != "" {
		prefix += "-" + name
	}
	return reserveBrief(dir, prefix)
}

func reserveBrief(dir, prefix string) (string, error) {
	for {
		var random [16]byte
		if _, err := io.ReadFull(briefRandom, random[:]); err != nil {
			return "", fmt.Errorf("brief randomness: %w", err)
		}
		path := filepath.Join(dir, fmt.Sprintf("%s-%x.md", prefix, random))
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create brief: %w", err)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("close brief: %w", err)
		}
		return path, nil
	}
}

// briefMaxAge is how old a brief file gets before the sweep removes it.
const briefMaxAge = 7 * 24 * time.Hour

func pruneBriefs(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list briefs: %w", err)
	}
	for _, entry := range entries {
		if !briefName.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat brief: %w", err)
		}
		if !info.ModTime().Before(now.Add(-briefMaxAge)) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("prune brief: %w", err)
		}
	}
	return nil
}
