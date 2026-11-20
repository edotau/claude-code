package worktree

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/parallel"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// PeerScan is the process scan; a var seam so tests never see the host's live sessions.
var PeerScan = PeerAgents

// CheckoutPeersInfo returns the checkout root, branch and nested worktrees; an empty root means no repo.
func CheckoutPeersInfo(ctx context.Context, cwd string) (top, branch string, nested []string) {
	top, _ = paths.CheckoutOf(cwd)
	if top == "" {
		return "", "", nil
	}
	top = paths.Canon(top)
	gitDir := filepath.Join(top, ".git")
	if st, err := os.Stat(gitDir); err == nil && st.IsDir() && ctx.Err() == nil {
		if _, err := os.Lstat(filepath.Join(gitDir, "commondir")); !os.IsNotExist(err) {
			return checkoutGit(ctx, cwd, top)
		}
		if branch, ok := checkoutBranch(gitDir); ok {
			if nested, ok := checkoutNested(top, gitDir); ok {
				return top, branch, nested
			}
		}
	}
	return checkoutGit(ctx, cwd, top)
}

func checkoutBranch(gitDir string) (string, bool) {
	if st, err := os.Lstat(filepath.Join(gitDir, "HEAD")); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", false
	}
	ref, symbolic := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: ")
	if !symbolic {
		return "HEAD", checkoutOID(ref)
	}
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || !filepath.IsLocal(ref) {
		return "", false
	}
	return branch, checkoutRef(gitDir, ref, branch)
}

func checkoutRef(gitDir, ref, branch string) bool {
	packed, err := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if err != nil && !os.IsNotExist(err) {
		return false
	}
	// Ambiguous short names and symbolic branch refs still need Git's ref resolution.
	for _, candidate := range []string{branch, "refs/" + branch, "refs/tags/" + branch,
		"refs/remotes/" + branch, "refs/remotes/" + branch + "/HEAD"} {
		if _, err := os.Lstat(filepath.Join(gitDir, candidate)); !os.IsNotExist(err) ||
			strings.Contains(string(packed), " "+candidate+"\n") {
			return false
		}
	}
	value, err := os.ReadFile(filepath.Join(gitDir, ref))
	if os.IsNotExist(err) {
		return strings.Contains(string(packed), " "+ref+"\n")
	}
	if err != nil {
		return false
	}
	return checkoutOID(strings.TrimSpace(string(value)))
}

func checkoutOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	_, err := hex.DecodeString(oid)
	return err == nil
}

func checkoutNested(top, gitDir string) ([]string, bool) {
	root := filepath.Join(gitDir, "worktrees")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	var others []string
	for _, entry := range entries {
		dir := filepath.Join(root, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "gitdir"))
		if err != nil {
			return nil, false
		}
		path := strings.TrimRight(string(data), "\r\n")
		if path == "" || strings.Contains(path, "\n") {
			return nil, false
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		others = append(others, filepath.Dir(path))
	}
	sort.Strings(others)
	var nested []string
	for _, path := range others {
		if real, err := filepath.EvalSymlinks(path); err == nil {
			nested = append(nested, real)
		}
	}
	return nestedIn(top, nested), true
}

// nestedIn keeps the worktree paths inside top: the peer scan skips those, never the tree that holds top.
func nestedIn(top string, worktrees []string) []string {
	var out []string
	for _, w := range worktrees {
		if strings.HasPrefix(w, top+string(filepath.Separator)) {
			out = append(out, w)
		}
	}
	return out
}

func checkoutGit(ctx context.Context, cwd, top string) (string, string, []string) {
	git := func(args ...string) string {
		out, err := proc.RunGit(ctx, cwd, args...)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(out)
	}
	var branch, list string
	parallel.Rethrow(parallel.All(2, func(i int) error { // the two git calls are independent
		if i == 0 {
			branch = git("rev-parse", "--abbrev-ref", "HEAD")
		} else {
			list = git("worktree", "list", "--porcelain")
		}
		return nil
	})...) // a contained panic stays the dispatcher's to recover
	var others []string
	for line := range strings.SplitSeq(list, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		if real, err := filepath.EvalSymlinks(path); err == nil && real != top {
			others = append(others, real)
		}
	}
	return top, branch, nestedIn(top, others)
}

// PeersOf runs the PeerAgents scan for top under ctx's deadline; a scan past it is abandoned as an error.
func PeersOf(ctx context.Context, top string, nested []string) ([]Proc, error) {
	type scan struct {
		procs []Proc
		err   error
	}
	done := make(chan scan, 1)
	scanFn := PeerScan // captured here so an abandoned scan never re-reads the seam
	go func() {
		procs, err := scanFn(top, nested)
		done <- scan{procs, err}
	}()
	select {
	case s := <-done:
		return s.procs, s.err
	case <-ctx.Done():
		return nil, fmt.Errorf("process scan: %w", ctx.Err())
	}
}

// SessionLabel names a peer from Claude Code's session registry (<config>/sessions/<pid>.json; never the .key file).
func SessionLabel(p Proc) string {
	fallback := fmt.Sprintf("%s pid %d", p.Comm, p.PID)
	b, err := os.ReadFile(filepath.Join(paths.ConfigDir(), "sessions", strconv.Itoa(p.PID)+".json"))
	if err != nil {
		return fallback
	}
	var s struct {
		Name      string `json:"name"`
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(b, &s) != nil || len(s.SessionID) < 8 {
		return fallback
	}
	name := s.Name
	if name == "" {
		name = s.SessionID[:8]
	}
	return fmt.Sprintf("session %s (%s, pid %d)", name, s.SessionID[:8], p.PID)
}
