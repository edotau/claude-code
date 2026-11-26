package memory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
)

// StoreCopy is a throwaway git clone of the store for the fallback to read; a var so tests need no repo.
var StoreCopy = func(ctx context.Context) (string, func(), error) {
	dir, err := os.MkdirTemp("", "memory-fallback-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if out, cerr := exec.CommandContext(ctx, "git", "clone", "--quiet", "--local", paths.MemoryStoreDir(), dir).CombinedOutput(); cerr != nil {
		cleanup()
		return "", nil, fmt.Errorf("git clone of the store: %v: %s", cerr, strings.TrimSpace(string(out)))
	}
	return dir, cleanup, nil
}

// CodeModeRun is the one code-mode Pi dispatch behind the typed-write and residue fallbacks; ask registers it.
var CodeModeRun func(ctx context.Context, prompt, workdir, schema, model string) (answer, who string, err error)

// CodeFallback allows one CodeModeRun per job: a second call fails without dispatching.
type CodeFallback struct {
	Model string // "" is opus
	used  atomic.Bool
	last  atomic.Pointer[string] // how the one dispatch ended
}

type fallbackKey struct{}

// WithCodeFallback carries the job's one fallback so the runner and the typed writer share it.
func WithCodeFallback(ctx context.Context, f *CodeFallback) context.Context {
	return context.WithValue(ctx, fallbackKey{}, f)
}

// JobCodeFallback is the fallback WithCodeFallback set, or nil.
func JobCodeFallback(ctx context.Context) *CodeFallback {
	f, _ := ctx.Value(fallbackKey{}).(*CodeFallback)
	return f
}

// FallbackModel maps the fallback's model name to a catalog id or alias; an unknown id is an error.
func FallbackModel(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = models.Opus
	}
	if !models.IsKnownSpelling(name) {
		return "", fmt.Errorf("model %q is not in the catalog", name)
	}
	return name, nil
}

// outcome is how the one dispatch ended, or that it is still running.
func (f *CodeFallback) outcome() string {
	if p := f.last.Load(); p != nil {
		return *p
	}
	return "still running"
}

// Used reports whether the job's one dispatch was taken.
func (f *CodeFallback) Used() bool { return f.used.Load() }

// Run asks the code-mode agent, over a git copy of the store, for a typed Batch; the workdir argument is ignored.
func (f *CodeFallback) Run(ctx context.Context, prompt, workdir, schema string) (answer, who string, err error) {
	if CodeModeRun == nil {
		return "", "", errors.New("code-mode fallback unavailable: this process registered no agent (a hook never dispatches one)")
	}
	if !f.used.CompareAndSwap(false, true) {
		return "", "", fmt.Errorf("code-mode fallback already used for this job (%s)", f.outcome())
	}
	dir, cleanup, err := StoreCopy(ctx)
	if err != nil {
		return "", "", err
	}
	defer cleanup()
	prompt = strings.ReplaceAll(prompt, paths.MemoryStoreDir(), dir) + "\n\nWrite nothing and run no memory command: answer with the typed batch only."
	answer, who, err = CodeModeRun(ctx, prompt, dir, schema, f.Model)
	if err == nil {
		if _, derr := decodeBatch(answer); derr != nil {
			err = fmt.Errorf("code-mode answer rejected: %w", derr)
		}
	}
	out := who + " answered"
	if err != nil {
		out, answer = who+" failed: "+err.Error(), ""
	}
	f.last.Store(&out)
	return answer, who, err
}

// Retry gives the one fallback a failed first answer and applies the corrected Batch; both failures are named.
func (f *CodeFallback) Retry(ctx context.Context, prompt, workdir, schema, first string, failed error, apply func(answer string) error) error {
	prompt += assets.MemoryRetryNote(failed.Error())
	answer, who, err := f.Run(ctx, prompt, workdir, schema)
	if err == nil {
		if err = apply(answer); err == nil {
			return nil
		}
	}
	return fmt.Errorf("typed write failed on %s (%v); code-mode fallback on %s failed: %w", cmp.Or(first, "the first model"), failed, cmp.Or(who, f.Model, "opus"), err)
}

// ValidateBatchAnswer reports why a typed answer is unusable; nil means it decodes and validates.
func ValidateBatchAnswer(answer string) error {
	_, err := decodeBatch(answer)
	return err
}
