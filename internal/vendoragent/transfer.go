package vendoragent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

// maxTurns is the number of trailing user/assistant turns Handoff keeps before trimming for size.
const maxTurns = 40

// handoffCap is the byte budget Handoff trims the transcript to.
const handoffCap = 64 * 1024

// DefaultSource finds the newest *.jsonl Claude Code session file for cwd, under Claude Code's own
// projects/<slug> dir (ConfigDir()/projects/<slug>) — never memory.NativeDir, which redirects into the
// memory store post-cutover and holds no transcripts.
func DefaultSource(cwd string) (string, error) {
	dir := filepath.Join(paths.ConfigDir(), "projects", memory.ProjectSlug(cwd))
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("no Claude session for %s; pass --source", cwd)
	}
	var newest string
	var newestMod int64
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if mt := info.ModTime().Unix(); newest == "" || mt > newestMod {
			newest, newestMod = e.Name(), mt
		}
	}
	if newest == "" {
		return "", fmt.Errorf("no Claude session for %s; pass --source", cwd)
	}
	return filepath.Join(dir, newest), nil
}

// turnLine is the subset of a transcript JSONL line Handoff reads.
type turnLine struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// readTurns reads source's JSONL, keeping user/assistant lines with non-empty text.
func readTurns(source string) ([]string, error) {
	f, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ring := make([]string, maxTurns) // ring buffer of the last maxTurns qualifying turns
	n, next := 0, 0
	transcript.EachLine(f, func(raw []byte) {
		var line turnLine
		if err := json.Unmarshal(raw, &line); err != nil {
			return
		}
		role := line.Type
		if role != "user" && role != "assistant" {
			return
		}
		text := strings.TrimSpace(transcript.ContentText(line.Message.Content))
		if text == "" {
			return
		}
		ring[next] = strings.ToUpper(role) + ": " + text
		next = (next + 1) % maxTurns
		n++
	})
	if n == 0 {
		return nil, nil
	}
	kept := min(n, maxTurns)
	turns := make([]string, kept)
	start := next % maxTurns
	if n <= maxTurns {
		start = 0
	}
	for i := 0; i < kept; i++ {
		turns[i] = ring[(start+i)%maxTurns]
	}
	return turns, nil
}

// Handoff builds the hand-off prompt for cwd from source's transcript: the last maxTurns turns,
// trimmed from the oldest until the transcript fits handoffCap, framed with note (or a default).
func Handoff(source, cwd, note string) (string, error) {
	turns, err := readTurns(source)
	if err != nil {
		return "", err
	}
	if len(turns) > maxTurns {
		turns = turns[len(turns)-maxTurns:]
	}
	if note == "" {
		note = "pick up where the transcript ends"
	}
	for {
		transcript := strings.Join(turns, "\n\n")
		prompt := fmt.Sprintf("You are taking over a Claude Code session in %s. Transcript (oldest first):\n\n%s\n\nContinue the work: %s",
			cwd, transcript, note)
		if len(prompt) <= handoffCap || len(turns) <= 1 {
			return prompt, nil
		}
		turns = turns[1:]
	}
}

// TransferArgs builds the `ask` argv for a transfer: --tools lets the vendor edit, --background
// detaches unless wait asks for the foreground answer.
func TransferArgs(vendor, cwd, prompt string, wait bool) []string {
	args := []string{"--agent", vendor, "--tools", "--workdir", cwd}
	if !wait {
		args = append(args, "--background")
	}
	return append(args, prompt)
}

// ValidateSource checks --source looks like a transcript: a .jsonl path that exists.
func ValidateSource(source string) error {
	if !strings.HasSuffix(source, ".jsonl") {
		return fmt.Errorf("--source %q: not a .jsonl file", source)
	}
	if _, err := os.Stat(source); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("--source %q: not found", source)
		}
		return err
	}
	return nil
}
