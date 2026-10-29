package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/transcript"
)

// Bounds so a large worktree cannot stall Stop past its 30s timeout.
const (
	stopFormatMaxFiles = 50
	stopFormatMaxSize  = 1 << 20
	stopFormatBudget   = 25 * time.Second
	stopFormatTimeout  = 30 * time.Second
	stopFormatWorkers  = 4
)

// `stop-format` (Stop, format group): formats files dirty in git and written by this session.
func StopFormat(r io.Reader) int { stopFormat(ParseInput(r)); return ExitProceed }

// stopFormat formats files that are dirty in git AND that this session's transcript shows it writing;
// a dirty file Claude never wrote is the user's hand edit and is left alone.
func stopFormat(in *Input) int {
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if cwd == "" {
		return ExitProceed
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopFormatTimeout)
	defer cancel()
	stampPath := filepath.Join(paths.StateDir(), "stop-format", cwdKey(cwd)+".json")
	changed := changedFiles(ctx, cwd)
	if len(changed) == 0 {
		_ = os.Remove(stampPath) // clean tree: drop stale stamps and skip the transcript scan
		return ExitProceed
	}
	allow := map[string]bool{}
	for _, f := range transcript.EditedFiles(in.TranscriptPath, stopFormatMaxFiles) {
		if abs, err := filepath.Abs(f); err == nil {
			allow[abs] = true
		}
	}
	if len(allow) == 0 {
		return ExitProceed
	}
	eligible, carry := selectForFormat(cwd, changed, loadStamps(stampPath), allow)
	maps.Copy(carry, formatAll(eligible))
	if data, err := json.Marshal(stampFile{Schema: stopFormatSchema, Stamps: carry}); err == nil {
		_ = paths.AtomicWrite(stampPath, data, 0o600)
	}
	return ExitProceed
}

// selectForFormat returns dirty session-written files whose stamp changed, plus stamps to carry forward.
func selectForFormat(cwd string, changed []string, prev map[string]string, allow map[string]bool) ([]string, map[string]string) {
	var eligible []string
	carry := map[string]string{}
	for _, rel := range changed {
		full := filepath.Join(cwd, rel)
		if !allow[full] {
			continue
		}
		fi, err := os.Stat(full)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > stopFormatMaxSize {
			continue
		}
		if key := stampKey(fi); prev[full] == key {
			carry[full] = key
			continue
		}
		if len(eligible) < stopFormatMaxFiles {
			eligible = append(eligible, full)
		}
	}
	return eligible, carry
}

// formatAll formats files concurrently under the budget; files still running at the deadline stay unstamped.
func formatAll(files []string) map[string]string {
	var mu sync.Mutex
	out := map[string]string{}
	sem := make(chan struct{}, stopFormatWorkers)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, f := range files {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if !formatFile(f) {
				return
			}
			if fi, err := os.Stat(f); err == nil {
				mu.Lock()
				out[f] = stampKey(fi)
				mu.Unlock()
			}
		})
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(stopFormatBudget):
	}
	mu.Lock()
	defer mu.Unlock()
	return maps.Clone(out)
}

// stampKey is mtime+size: cheaper than a hash and enough to skip a no-op re-format.
func stampKey(fi os.FileInfo) string {
	return strconv.FormatInt(fi.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(fi.Size(), 10)
}

func cwdKey(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	return hex.EncodeToString(sum[:8])
}

// stampFile is the stamp cache on disk; a v0 bare map or a newer schema decodes as an empty cache.
type stampFile struct {
	Schema int               `json:"schema"`
	Stamps map[string]string `json:"stamps"`
}

func loadStamps(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	var f stampFile
	if _, err := schemas.Decode(stopFormatFormat, data, &f); err != nil || f.Stamps == nil {
		return map[string]string{}
	}
	return f.Stamps
}

// changedFiles lists modified, staged and untracked paths relative to cwd; -z avoids git's C-quoting.
func changedFiles(ctx context.Context, cwd string) []string {
	data, err := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain=v1", "-z", "--untracked-files=all").Output()
	if err != nil || len(data) == 0 {
		return nil
	}
	root := repoRootOf(cwd)
	if root == "" {
		return nil
	}
	var out []string
	records := strings.Split(string(data), "\x00")
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) < 4 {
			continue
		}
		status, path := record[:2], record[3:]
		if strings.ContainsAny(status, "RC") {
			i++
		}
		if status == "  " || status == "!!" {
			continue
		}
		rel, err := filepath.Rel(cwd, filepath.Join(root, path))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			out = append(out, rel)
		}
	}
	return out
}
