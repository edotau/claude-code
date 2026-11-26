package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// cmdMemorySync runs one sync pass; the hidden --serve form is the peer side of it, run over ssh.
func cmdMemorySync(args []string) int {
	if len(args) > 0 && (args[0] == "--serve" || strings.HasPrefix(args[0], "--serve=")) {
		return cmdMemoryServe(args)
	}
	fs := memoryFlagSet("sync")
	host := fs.String("host", "", "sync with this peer only (default: CLAUDE_REMOTE_HOST)")
	if code, done := memoryParseNoArgs(fs, "sync", args); done {
		return code
	}
	if memory.InSandbox() {
		fmt.Fprintln(os.Stderr, "memory sync: refused: this config dir is a sandbox")
		return 0
	}
	if !memory.SyncOn() {
		return helptext.FailUsage(memory.SyncOffMsg)
	}
	peers := []string{strings.TrimSpace(*host)}
	if *host == "" {
		peers = providers.RemoteHosts()
	}
	if len(peers) == 0 {
		return helptext.FailUsage("memory sync: CLAUDE_REMOTE_HOST names no peer; give --host")
	}
	code, err := memory.RunSync(peers, os.Stdout)
	if err != nil {
		return helptext.Fail("memory sync: %v", err)
	}
	return code
}

// cmdMemoryServe is the peer side: export prints envelopes past --since; import reads a stream on stdin.
func cmdMemoryServe(args []string) int {
	fs := memoryFlagSet("sync")
	mode := fs.String("serve", "", "")
	since := fs.String("since", "", "")
	if code, done := memoryParseNoArgs(fs, "sync", args); done {
		return code
	}
	marks, err := memory.ParseSince(*since)
	if err != nil || *mode != "export" && *mode != "import" || *mode == "import" && *since != "" {
		return helptext.FailUsage("memory sync: --serve takes export [--since ORIGIN=SEQ,...] or import")
	}
	st, err := memory.OpenEnvelopeStore()
	if err == nil {
		defer st.Close()
		err = serveRun(st, *mode, marks)
	}
	if errors.Is(err, memory.ErrBankBusy) {
		helptext.Fail("memory sync: %v", err)
		return memory.ImportBusyExit
	}
	if err != nil {
		return helptext.Fail("memory sync: %v", err)
	}
	return 0
}

func serveRun(st memory.EnvelopeStore, mode string, since map[string]int64) error {
	if mode == "import" {
		if err := memory.WithBankLock(memory.BankLockWait, func() error { return nil }); err != nil {
			return err
		}
		timer := time.AfterFunc(memory.SyncCallLimit, func() { _ = os.Stdin.Close() })
		defer timer.Stop()
		return memory.ServeImport(st, os.Stdin, os.Stdout)
	}
	var buf bytes.Buffer // the lock covers the read, not the ssh pipe's pace
	err := memory.WithBankLock(memory.BankLockWait, func() error { return memory.ServeExport(st, since, &buf) })
	if err == nil {
		_, err = os.Stdout.Write(buf.Bytes())
	}
	return err
}
