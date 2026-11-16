package quality

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/memhook"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// Bounds so a large worktree cannot stall Stop past its 30s timeout.
const (
	stopFormatMaxFiles = 50
	stopFormatMaxSize  = 1 << 20
	stopFormatBudget   = 25 * time.Second
	stopFormatTimeout  = 30 * time.Second
	// git status gets what the format budget leaves of the 30s Stop timeout, so the two never add past it.
	stopFormatGitTimeout = stopFormatTimeout - stopFormatBudget
	stopFormatWorkers    = 4
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
	ctx, cancel := context.WithTimeout(context.Background(), stopFormatGitTimeout)
	defer cancel()
	stampPath := filepath.Join(paths.StateDir(), "stop-format", textutil.SHA256Hex([]byte(cwd))[:16]+".json")
	changed := changedFiles(ctx, cwd)
	if ctx.Err() != nil { // a git timeout is not a clean tree: keep the stamps
		return core.Result{}
	}
	if len(changed) == 0 {
		_ = os.Remove(stampPath) // clean tree: drop stale stamps and skip the transcript scan
		return core.Result{}
	}
	prev := loadStampFile(stampPath)
	edited, offset, err := scanEdited(prev, in.TranscriptPath)
	if err != nil { // a partial scan still formats what it found
		fmt.Fprintf(os.Stderr, "format-stop: transcript scan: %v\n", err)
	}
	allow := allowSet(cwd, edited)
	carry, missing := prev.Stamps, map[skipKey]int(nil)
	if len(allow) > 0 {
		var eligible []string
		eligible, carry = selectForFormat(cwd, changed, prev.Stamps, allow)
		var stamps map[string]string
		stamps, missing = formatAll(eligible)
		maps.Copy(carry, stamps)
	}
	if offset > 0 { // persist the scan offset even when nothing was edited, so the next Stop skips the old bytes
		out := stampFile{Schema: stopFormatSchema, Stamps: carry, Transcript: in.TranscriptPath, Offset: offset, Edited: edited}
		if data, err := json.Marshal(out); err == nil {
			_ = paths.AtomicWrite(stampPath, data, 0o600)
		}
	}
	return core.Result{SystemMessage: missingNotes(in.SessionID, missing)}
}

// allowSet is the edited files as the real absolute paths selectForFormat matches against.
func allowSet(cwd string, edited []string) map[string]bool {
	allow := map[string]bool{}
	for _, f := range edited {
		if !filepath.IsAbs(f) {
			f = filepath.Join(cwd, f)
		}
		allow[paths.Real(f)] = true // /var vs /private/var spellings of one file must meet in the allow set
	}
	return allow
}

// skipKey is one absent formatter and the extension of a file it left unformatted.
type skipKey struct{ tool, ext string }

// missingNotes is one note per formatter not installed, naming each extension it skipped, once per session.
func missingNotes(session string, missing map[skipKey]int) string {
	byTool := map[string][]string{}
	for _, k := range slices.SortedFunc(maps.Keys(missing), func(a, b skipKey) int {
		return cmp.Or(strings.Compare(a.tool, b.tool), strings.Compare(a.ext, b.ext))
	}) {
		byTool[k.tool] = append(byTool[k.tool], fmt.Sprintf("%d %s file(s)", missing[k], k.ext))
	}
	var notes []string
	for _, tool := range slices.Sorted(maps.Keys(byTool)) {
		if first, err := core.SeenAdd("hooks/format-stop", session, tool); session == "" || err != nil || first {
			notes = append(notes, fmt.Sprintf("format-stop: %s not found; %s left unformatted", tool,
				strings.Join(byTool[tool], ", ")))
		}
	}
	return strings.Join(notes, "\n")
}

// selectForFormat returns dirty session-written files whose stamp changed, plus stamps to carry forward.
func selectForFormat(cwd string, changed []string, prev map[string]string, allow map[string]bool) ([]string, map[string]string) {
	var eligible []string
	carry := map[string]string{}
	modified := map[string]time.Time{}
	for _, rel := range changed {
		full := paths.Real(filepath.Join(cwd, rel))
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

// formatAll formats files concurrently (deadline: running stay unstamped, queued skip); missing counts per absent tool.
func formatAll(files []string) (stamps map[string]string, missing map[skipKey]int) {
	ctx, cancel := context.WithTimeout(context.Background(), stopFormatBudget)
	defer cancel()
	var mu sync.Mutex
	out, skipped := map[string]string{}, map[skipKey]int{}
	sem := make(chan struct{}, stopFormatWorkers)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, f := range files {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if ctx.Err() != nil { // a slot freed at the deadline must not start a new formatter
				return
			}
			ok, tool := formatFile(f)
			fi, err := os.Stat(f)
			mu.Lock()
			defer mu.Unlock()
			if tool != "" {
				skipped[skipKey{tool, strings.ToLower(filepath.Ext(f))}]++
			} else if ok && err == nil {
				out[f] = stampKey(fi)
			}
		})
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	mu.Lock()
	defer mu.Unlock()
	return maps.Clone(out), maps.Clone(skipped)
}

// stampKey is mtime+size: cheaper than a hash and enough to skip a no-op re-format.
func stampKey(fi os.FileInfo) string {
	return strconv.FormatInt(fi.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(fi.Size(), 10)
}

// stampFile is the stamp cache on disk; a v0 bare map or a newer schema decodes as an empty cache.
type stampFile struct {
	Schema int               `json:"schema"`
	Stamps map[string]string `json:"stamps"`
	// Transcript, Offset and Edited resume the edit scan: the files edited in Transcript before byte Offset.
	Transcript string   `json:"transcript,omitempty"`
	Offset     int64    `json:"offset,omitempty"`
	Edited     []string `json:"edited,omitempty"`
}

// scanEdited is the files edited in transcript, reading only what prev's offset has not seen; next resumes it.
func scanEdited(prev stampFile, transcriptPath string) (edited []string, next int64, err error) {
	offset, known := prev.Offset, prev.Edited
	if fi, serr := os.Stat(transcriptPath); prev.Transcript != transcriptPath || serr != nil || fi.Size() < offset {
		offset, known = 0, nil
	}
	added, next, err := transcript.EditedFilesFrom(transcriptPath, offset)
	return slices.Compact(slices.Sorted(slices.Values(slices.Concat(known, added)))), next, err
}

func loadStampFile(path string) stampFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return stampFile{}
	}
	var f stampFile
	if _, err := schemas.Decode(stopFormatFormat, data, &f); err != nil {
		return stampFile{}
	}
	return f
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
