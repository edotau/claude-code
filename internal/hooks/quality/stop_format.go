package quality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/memhook"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
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

// StopFormat formats git-dirty files this session wrote; a dirty file it never wrote is the user's and left alone.
func StopFormat(in *core.Input) core.Result {
	in = memhook.AgentInput(in) // copilot, opencode and codex have no Claude-shaped transcript of their own
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if cwd == "" {
		return core.Result{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopFormatTimeout)
	defer cancel()
	stampPath := filepath.Join(paths.StateDir(), "stop-format", cwdKey(cwd)+".json")
	changed := changedFiles(ctx, cwd)
	if len(changed) == 0 {
		_ = os.Remove(stampPath) // clean tree: drop stale stamps and skip the transcript scan
		return core.Result{}
	}
	allow := map[string]bool{}
	edited, err := transcript.EditedFiles(in.TranscriptPath, math.MaxInt)
	if err != nil { // a partial scan still formats what it found
		fmt.Fprintf(os.Stderr, "format-stop: transcript scan: %v\n", err)
	}
	for _, f := range edited {
		if !filepath.IsAbs(f) {
			f = filepath.Join(cwd, f)
		}
		if abs, err := filepath.Abs(f); err == nil {
			allow[abs] = true
		}
	}
	if len(allow) == 0 {
		return core.Result{}
	}
	eligible, carry := selectForFormat(cwd, changed, loadStamps(stampPath), allow)
	maps.Copy(carry, formatAll(eligible))
	if data, err := json.Marshal(stampFile{Schema: stopFormatSchema, Stamps: carry}); err == nil {
		_ = paths.AtomicWrite(stampPath, data, 0o600)
	}
	return core.Result{}
}

// selectForFormat returns dirty session-written files whose stamp changed, plus stamps to carry forward.
func selectForFormat(cwd string, changed []string, prev map[string]string, allow map[string]bool) ([]string, map[string]string) {
	var eligible []string
	carry := map[string]string{}
	modified := map[string]time.Time{}
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
		eligible = append(eligible, full)
		modified[full] = fi.ModTime()
	}
	sort.SliceStable(eligible, func(i, j int) bool { return modified[eligible[i]].After(modified[eligible[j]]) })
	eligible = eligible[:min(len(eligible), stopFormatMaxFiles)]
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
	data, err := proc.RunGit(ctx, cwd, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil || len(data) == 0 {
		return nil
	}
	root, _ := paths.CheckoutOf(cwd)
	if root == "" {
		return nil
	}
	var out []string
	for _, e := range proc.StatusEntries(data) {
		if e.Code == "  " || e.Code == "!!" {
			continue
		}
		if rel, ok := core.RelWithin(cwd, filepath.Join(root, e.Path)); ok {
			out = append(out, rel)
		}
	}
	return out
}

// stopFormatSchema is the stop-format stamp cache's current schema version.
const stopFormatSchema = 1

var stopFormatFormat = schemas.Register(schemas.Format{
	Name: "stop-format", Owner: "internal/hooks/quality", Kind: schemas.Cache,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "stop-format", "*.json") },
	New:     func() any { return &stampFile{} },
	Version: stopFormatSchema, Keep: "rebuildable stamps; v0 bare map is dropped, not migrated",
})
