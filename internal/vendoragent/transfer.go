package vendoragent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// transferScanBuf is the bufio.Scanner buffer cap; a long transcript line must not truncate silently.
const transferScanBuf = 4 * 1024 * 1024

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

// contentPart is one element of a message.content array; only {type:"text"} parts carry transcript text.
type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// turnLine is the subset of a transcript JSONL line Handoff reads.
type turnLine struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// turnText extracts the plain text of one line's message.content, which is either a bare string or
// an array of parts (only "text" parts kept; tool_use/tool_result/thinking parts are skipped).
func turnText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// readTurns reads source's JSONL, keeping user/assistant lines with non-empty text.
func readTurns(source string) ([]string, error) {
	f, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), transferScanBuf)
	var turns []string
	for sc.Scan() {
		var line turnLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue
		}
		role := line.Type
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(turnText(line.Message.Content))
		if text == "" {
			continue
		}
		turns = append(turns, strings.ToUpper(role)+": "+text)
	}
	if err := sc.Err(); err != nil {
		return nil, err
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
