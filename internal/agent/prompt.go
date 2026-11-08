package agent

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/agent/retry"
	"net/http"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Preamble frames system text as instructions ahead of the task, for an agent with no system-prompt channel.
func Preamble(text string) string {
	return "System instructions (follow them for this whole task):\n" + text + "\n\nTask:\n"
}

// MaxPromptBytes keeps a prompt passed as one argv element under the kernel's 128 KiB MAX_ARG_STRLEN.
const MaxPromptBytes = 96 << 10

// RubricPrompt frames task against the review standard in file, quoted verbatim.
func RubricPrompt(file, task string) (string, error) {
	standard, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("rubric: %w", err)
	}
	prompt := "You are reviewing against a fixed standard, quoted in full below.\n\n" +
		"=== BEGIN REVIEW STANDARD (the rubric, NOT the code under review) ===\n" + string(standard) +
		"\n=== END REVIEW STANDARD ===\n\nTASK: " + task + "\n\nRules for your output:\n" +
		"- Apply the standard exactly as written, including every exemption it states.\n" +
		"- Report only violations you can point to, with file and approximate line, quoting the code.\n" +
		"- Do not report on the standard's own text or examples.\n" +
		"- If nothing violates the standard, say exactly: no findings.\n"
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
		fmt.Fprintf(&b, "\n@%s", p)
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
		fmt.Fprintf(&b, "\n\n<file path=%q>\n%s\n</file>", p, data)
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

var contentPolicyRE = regexp.MustCompile(`(?i)content[ _-]?(?:policy|filter)|safety (?:system|filter|polic)|` +
	`responsible ai|violates (?:our|the) (?:usage|content) polic|request entity too large|payload too large`)

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
	return contentPolicyRE.MatchString(err.Error())
}
