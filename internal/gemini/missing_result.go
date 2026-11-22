package gemini

import (
	"errors"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/textutil"
)

var errAgyMissingResult = errors.New("agy: stream ended without a result event")

const agyDiagnosticLines = 5

func agyMissingResultErr(err error, logPath, stderr string) error {
	if !errors.Is(err, errAgyMissingResult) || agent.CutoffOf(err) != "" {
		return err
	}
	if len(stderr) == agyLogTailBytes {
		_, stderr, _ = strings.Cut(stderr, "\n") // stderr's bounded buffer can also begin inside a secret
	}
	log, readErr := readTail(logPath, agyLogTailBytes)
	if readErr == nil && len(log) == agyLogTailBytes {
		_, log, _ = strings.Cut(log, "\n") // a clipped first line may hold only a secret's suffix
	}
	detail := redact.Text(textutil.RedactSecrets(strings.TrimSpace(stderr + "\n" + log)))
	if detail == "" {
		return err
	}
	lines := strings.Split(detail, "\n")
	for _, line := range lines {
		if strings.Contains(line, "AGY_ERROR:") {
			return fmt.Errorf("%w; agy diagnostics: %s", err, line)
		}
	}
	return fmt.Errorf("%w; agy log: %s", err, strings.Join(lines[max(0, len(lines)-agyDiagnosticLines):], "\n"))
}
