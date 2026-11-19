package paths

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RTKBinaryEnv overrides the rtk binary: tests cannot rely on rtk being installed, and a wrapper path also works.
const RTKBinaryEnv = "HARNESS_RTK_BIN"

// RTKBinary returns the absolute rtk binary to invoke, or "" when it is not available.
func RTKBinary() string {
	candidates := []string{RTKPinned(), "rtk"} // the pinned install first, then PATH
	if name := os.Getenv(RTKBinaryEnv); name != "" {
		candidates = []string{name}
	}
	for _, name := range candidates {
		if path, err := exec.LookPath(name); err == nil {
			if abs, err := filepath.Abs(path); err == nil {
				return abs
			}
			return path
		}
	}
	return ""
}

// RTKPinned is the harness-pinned rtk install, BinDir/rtk; it may not exist.
func RTKPinned() string { return filepath.Join(BinDir(), "rtk") }

// RTKOnPath is PATH's rtk ("" when none) and whether it resolves, symlinks followed, to the same file as bin.
func RTKOnPath(bin string) (found string, same bool) {
	found, err := exec.LookPath("rtk")
	if err != nil {
		return "", false
	}
	got, err := filepath.EvalSymlinks(found)
	if err != nil {
		return found, false
	}
	want, err := filepath.EvalSymlinks(bin)
	return found, err == nil && got == want
}

var rtkRewrites sync.Map // binary+command -> rewritten command

// RTKRewrite returns cmd in its rtk form, or cmd itself when rtk is absent, declines, fails or already prefixes it.
func RTKRewrite(ctx context.Context, cmd string) string {
	bin := RTKBinary()
	key := bin + "\x00" + cmd
	if got, ok := rtkRewrites.Load(key); ok {
		return got.(string)
	}
	out := cmd
	if bin != "" && strings.TrimSpace(cmd) != "" && !strings.HasPrefix(strings.TrimSpace(cmd), "rtk ") {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, bin, "rewrite", cmd)
		c.WaitDelay = 200 * time.Millisecond
		b, err := c.Output()
		var ee *exec.ExitError
		if got := strings.TrimSpace(string(b)); got != "" && (err == nil || errors.As(err, &ee) && ee.ExitCode() == 3) {
			out = got // exit 3 is rtk's "rewritten, ask first", which a gate has no one to ask
		}
	}
	rtkRewrites.Store(key, out)
	return out
}
