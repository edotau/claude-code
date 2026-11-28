package jev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

// serveWait bounds how long a restarted server may take to load its model; reloadTries covers launchd's slow unload.
var (
	serveWait   = 3 * time.Minute
	reloadTries = 8
)

// health is the model line the server at target reports, and whether it answered at all.
func health(ctx context.Context, target string) (model string, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/health", nil)
	if err != nil {
		return "", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var h struct{ Model, Method string }
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&h) // an unreadable reply leaves the model empty
	return strings.TrimSpace(h.Model + " " + h.Method), resp.StatusCode == http.StatusOK
}

// plutil runs macOS's plist tool on the login item and returns its output.
func (j Job) plutil(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "plutil", append(args, j.Service)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("plutil %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// checkpointArg is the login item's argument list and the index of the value after --checkpoint.
func (j Job) checkpointArg(ctx context.Context) (args []string, at int, err error) {
	raw, err := j.plutil(ctx, "-extract", "ProgramArguments", "json", "-o", "-")
	if err != nil {
		return nil, 0, err
	}
	if err = json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, 0, fmt.Errorf("%s: ProgramArguments: %w", j.Service, err)
	}
	at = slices.Index(args, "--checkpoint") + 1
	if at == 0 || at >= len(args) {
		return nil, 0, fmt.Errorf("%s: ProgramArguments has no --checkpoint value", j.Service)
	}
	return args, at, nil
}

// Serving is the checkpoint the login item starts the server with ("" when no item is remembered or readable).
func (j Job) Serving(ctx context.Context) string {
	if j.Service == "" || runtime.GOOS != "darwin" {
		return ""
	}
	args, at, err := j.checkpointArg(ctx)
	if err != nil {
		return ""
	}
	return args[at]
}

// servable allows a run's base checkpoint or one whose report says it passed the gold gate.
func (j Job) servable(checkpoint string) error {
	reports, _ := filepath.Glob(filepath.Join(j.Out, "report*.json"))
	for _, file := range reports {
		var r struct {
			Base string `json:"base_checkpoint"`
			Gate struct {
				Passed     bool   `json:"passed"`
				Checkpoint string `json:"checkpoint"`
			} `json:"gold_gate"`
		}
		if b, err := os.ReadFile(file); err != nil || json.Unmarshal(b, &r) != nil {
			continue
		}
		if checkpoint == r.Base || r.Gate.Passed && checkpoint == r.Gate.Checkpoint {
			return nil
		}
	}
	return usage("%s is neither a run's base checkpoint nor one that passed the gold gate (reports in %s)", checkpoint, j.Out)
}

// reload restarts the login item from its file; bootstrap is retried while launchd finishes unloading the old one.
func (j Job) reload(ctx context.Context) error {
	label, err := j.plutil(ctx, "-extract", "Label", "raw", "-o", "-")
	if err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.CommandContext(ctx, "launchctl", "bootout", domain+"/"+label).Run() // not loaded is fine
	var out []byte
	for range reloadTries {
		if out, err = exec.CommandContext(ctx, "launchctl", "bootstrap", domain, j.Service).CombinedOutput(); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("launchctl bootstrap %s: %s", j.Service, strings.TrimSpace(string(out)))
}

// restart reloads the login item and waits for the server to answer its health check.
func (j Job) restart(ctx context.Context, target string) (string, error) {
	if err := j.reload(ctx); err != nil {
		return "", err
	}
	for deadline := time.Now().Add(serveWait); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if model, ok := health(ctx, target); ok {
			return model, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", fmt.Errorf("the server did not answer %s/health within %s", target, serveWait)
}

// repoint backs the login item up and rewrites its --checkpoint value; old is the file as it was.
func (j Job) repoint(ctx context.Context, checkpoint string) (old []byte, backup string, err error) {
	args, at, err := j.checkpointArg(ctx)
	if err != nil {
		return nil, "", err
	}
	args[at] = checkpoint
	list, err := json.Marshal(args)
	if err != nil {
		return nil, "", err
	}
	if old, err = os.ReadFile(j.Service); err != nil {
		return nil, "", err
	}
	if backup, err = paths.Backup(j.Service, old); err != nil {
		return nil, "", err
	}
	// the whole list is replaced: plutil's -replace on one array index inserts instead
	_, err = j.plutil(ctx, "-replace", "ProgramArguments", "-json", string(list))
	return old, backup, err
}

// Serve points the login item at checkpoint and restarts the server; a server that stays down gets the old item back.
func (j Job) Serve(ctx context.Context, checkpoint string, w io.Writer) error {
	switch {
	case runtime.GOOS != "darwin":
		return errors.New("--serve drives a launchd login item, which only macOS has")
	case j.Service == "":
		return usage("no login item remembered: pass --service FILE (kept after first use)")
	}
	checkpoint, err := filepath.Abs(checkpoint)
	if err == nil {
		err = j.servable(checkpoint)
	}
	if err != nil {
		return err
	}
	old, backup, err := j.repoint(ctx, checkpoint)
	if err != nil {
		return err
	}
	_, target, _ := Server(ctx)
	model, err := j.restart(ctx, target)
	if err != nil {
		return j.putBack(ctx, target, old, backup, err)
	}
	fmt.Fprintf(w, "serving %s\nserver: up (%s) %s\nlogin item backup: %s\n", checkpoint, target, model, backup)
	return nil
}

// putBack restores the login item after a failed restart and starts the previous server again; it returns cause.
func (j Job) putBack(ctx context.Context, target string, old []byte, backup string, cause error) error {
	if err := os.WriteFile(j.Service, old, 0o644); err != nil {
		return fmt.Errorf("%w; restoring %s from %s also failed: %v", cause, j.Service, backup, err)
	}
	_, err := j.restart(ctx, target)
	return fmt.Errorf("%w; login item restored (previous server: %v)", cause, cmpErr(err))
}

// cmpErr words a restore's outcome for the error message.
func cmpErr(err error) string {
	if err == nil {
		return "back up"
	}
	return err.Error()
}
