package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/assets"
)

const (
	dreamRtkTimeout = 60 * time.Second
	dreamRtkTop     = 10
)

var (
	rtkWordRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,32}$`)
	rtkFlagRe = regexp.MustCompile(`^-(?:[A-Za-z]|-[a-z][a-z-]{1,20})$`) // one bare flag; "-ePAT" or "--k=v" carry values
)

type rtkMiss struct {
	Command string `json:"command"`
	Count   int    `json:"count"`
}

// rtkMissWord keeps the command word and its first flag only, never paths or arguments.
func rtkMissWord(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 || !rtkWordRe.MatchString(f[0]) {
		return ""
	}
	if len(f) > 1 && rtkFlagRe.MatchString(f[1]) {
		return f[0] + " " + f[1]
	}
	return f[0]
}

// readDreamRtk lists the top commands run without rtk over the last 7 days.
func readDreamRtk() ([]rtkMiss, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dreamRtkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rtk", "discover", "--all", "--since", "7", "--format", "json")
	cmd.Env = append(os.Environ(), "RTK_SUPPRESS_HOOK_WARNING=1", "RTK_TELEMETRY_DISABLED=1")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("rtk discover skipped: %w", err)
	}
	var doc struct {
		Supported []rtkMiss `json:"supported"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		return nil, fmt.Errorf("rtk discover skipped: bad json: %w", err)
	}
	var out []rtkMiss
	for _, m := range doc.Supported {
		if w := rtkMissWord(m.Command); w != "" && m.Count > 0 && len(out) < dreamRtkTop {
			out = append(out, rtkMiss{Command: w, Count: m.Count})
		}
	}
	return out, nil
}

func writeDreamRtk(out io.Writer, misses []rtkMiss, err error) {
	fmt.Fprintf(out, "memory dream: rtk misses: %d command(s)\n", len(misses))
	for _, m := range misses {
		fmt.Fprintf(out, "memory dream: rtk miss %s x%d\n", m.Command, m.Count)
	}
	dreamStepError(out, "rtk discover", err)
}

// rtkPromptBlock asks the agent for one rtk candidate record from the misses; empty when there are none.
func rtkPromptBlock(misses []rtkMiss) string {
	if len(misses) == 0 {
		return ""
	}
	b, _ := json.Marshal(misses)
	return assets.MemoryRTKMisses(string(b))
}
