package agent

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/assets"
)

// Preamble frames system text as instructions ahead of the task, for an agent with no system-prompt channel.
func Preamble(text string) string {
	return assets.AgentPreamble(text)
}

// WebViaCLI is the one line an agent with no native web tool on a Databricks row gets for web lookups.
var WebViaCLI = assets.AgentWebViaCLI()

// MaxPromptBytes keeps a prompt passed as one argv element under the kernel's 128 KiB MAX_ARG_STRLEN.
const MaxPromptBytes = 96 << 10

// rubricLead opens every rubric prompt; a replayed rubric review is recognised by it.
var rubricLead = assets.AgentRubricLead()

// rubricPrompt frames task against the review standard in file, quoted verbatim.
func rubricPrompt(file, task string) (string, error) {
	standard, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("rubric: %w", err)
	}
	prompt := rubricLead + assets.AgentRubricBody(string(standard), task)
	if len(prompt) > MaxPromptBytes {
		return "", fmt.Errorf("rubric: composed prompt is %d bytes, over the %d limit", len(prompt), MaxPromptBytes)
	}
	return prompt, nil
}

// AttachSuffix appends each path as "@<path>" after prompt, which copilot and agy resolve live from the file.
func AttachSuffix(prompt string, paths []string) string {
	var b strings.Builder
	b.WriteString(prompt)
	for _, p := range paths {
		b.WriteString(assets.AttachRef(p))
	}
	return b.String()
}

// ErrBadAttach marks an unreadable or binary --attach file: a request fault no other agent can fix.
var ErrBadAttach = errors.New("attach")

// AttachInline embeds each path as a <file path="…">content</file> block: the fallback with no live-read channel.
func AttachInline(prompt string, paths []string) (string, error) {
	if len(paths) == 0 {
		return prompt, nil
	}
	var b strings.Builder
	b.WriteString(prompt)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("%w %s: %w", ErrBadAttach, p, err)
		}
		if IsImage(data) {
			return "", fmt.Errorf("--%w %s: image; this path cannot deliver images (needs file access)", ErrBadAttach, p)
		}
		if !IsText(data) {
			return "", fmt.Errorf("--%w %s: binary file; attach text only", ErrBadAttach, p)
		}
		b.WriteString(assets.AttachFile(strconv.Quote(p), string(data)))
	}
	return b.String(), nil
}

// IsImage sniffs data for a png, jpeg, gif or webp header.
func IsImage(data []byte) bool {
	switch http.DetectContentType(data) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// SplitImages separates image paths (by content sniff) from the rest; an unreadable path stays in rest.
func SplitImages(paths []string) (images, rest []string) {
	for _, p := range paths {
		head := make([]byte, 512)
		f, err := os.Open(p)
		if err != nil {
			rest = append(rest, p)
			continue
		}
		n, _ := f.Read(head)
		_ = f.Close()
		if IsImage(head[:n]) {
			images = append(images, p)
		} else {
			rest = append(rest, p)
		}
	}
	return images, rest
}

// IsText reports whether data can ride inline in a prompt: valid UTF-8 with no NUL byte.
func IsText(data []byte) bool { return bytes.IndexByte(data, 0) < 0 && utf8.Valid(data) }

// ErrPromptTooLarge is the sentinel behind PromptTooLarge, so callers can exclude it with errors.Is.
var ErrPromptTooLarge = errors.New("prompt too large")

func PromptTooLarge(n, limit int) error {
	return fmt.Errorf("%w: %d bytes, over the %d limit", ErrPromptTooLarge, n, limit)
}

var tooLargeRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)request entity too large|payload too large`)
})

// requestFault is true when the request itself is at fault (bad attach, 413, oversized prompt, policy refusal).
func requestFault(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBadAttach) || errors.Is(err, ErrPromptTooLarge) {
		return true
	}
	if e, ok := errors.AsType[*retry.Error](err); ok && e.Status == 413 {
		return true
	}
	return retry.IsPolicyRefusal(err) || tooLargeRE().MatchString(err.Error())
}
