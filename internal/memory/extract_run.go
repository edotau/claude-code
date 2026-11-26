package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/textutil"
)

// ExtractRunOptions selects a read-only classification or an explicitly requested purge.
type ExtractRunOptions struct {
	DryRun bool
	Purge  bool
	Format string
	Out    string
}

// ExtractRunReport includes the classified rows and the timestamp-free comparison digest.
type ExtractRunReport struct {
	ExtractResult
	Purged          ExtractTotal `json:"purged"`
	RemainingSHA256 string       `json:"remaining_sha256,omitempty"`
}

// RunExtract resolves the same store as upgrade and never calls a model.
func RunExtract(o ExtractRunOptions, w io.Writer) error {
	if err := validateExtractRun(o); err != nil {
		return err
	}
	if o.Format == "" {
		o.Format = "text"
	}
	root := paths.MemoryStoreDir()
	if err := extractOutputOutsideStore(root, o.Out); err != nil {
		return err
	}
	var rep ExtractRunReport
	var err error
	run := func() error { rep, err = extractRun(root, o); return err }
	if o.Purge && !o.DryRun {
		err = extractRunLocked(run)
	} else {
		err = run()
	}
	if err != nil {
		if rep.Purged.Files > 0 {
			return errors.Join(err, finishExtract(o, rep, w))
		}
		return err
	}
	return finishExtract(o, rep, w)
}

func validateExtractRun(o ExtractRunOptions) error {
	if !o.DryRun && !o.Purge {
		return errors.New("specify --dry-run or --purge")
	}
	if o.Format != "" && o.Format != "text" && o.Format != "json" {
		return errors.New("--format must be text or json")
	}
	return nil
}

func extractRunLocked(run func() error) error {
	ran, err := paths.TryFileLock(lockPathOf()+".lock", lockWait(), func() error { return WithBankLock(BankLockWait, run) })
	if err == nil && !ran {
		return ErrBankBusy
	}
	return err
}

func extractRun(root string, o ExtractRunOptions) (rep ExtractRunReport, err error) {
	result, head, err := classifyExtractRun(root, o)
	rep.ExtractResult = result
	if err != nil {
		return rep, err
	}
	rep.RemainingSHA256, err = extractRemainingHash(root)
	if err != nil {
		return rep, err
	}
	if o.Out != "" {
		if err := writeExtractOutput(o.Out, rep.ExtractResult); err != nil {
			return rep, err
		}
	}
	if o.Purge && !o.DryRun {
		rep.Purged, err = purgeExtract(root, rep.ExtractResult, head)
		if err != nil {
			return rep, err
		}
		rep.RemainingSHA256, err = extractRemainingHash(root)
	}
	return rep, err
}

func classifyExtractRun(root string, o ExtractRunOptions) (ExtractResult, string, error) {
	purge := o.Purge && !o.DryRun
	opts, err := extractRunOptions(purge)
	if err != nil {
		return ExtractResult{}, "", err
	}
	head := ""
	if purge {
		head, err = extractCleanHead(root)
		if err != nil {
			return ExtractResult{}, "", err
		}
	}
	result, err := ClassifyExtract(root, opts)
	return result, head, err
}

func finishExtract(o ExtractRunOptions, rep ExtractRunReport, w io.Writer) error {
	if o.Format == "json" {
		return json.NewEncoder(w).Encode(rep)
	}
	return printExtract(w, rep)
}

func extractRunOptions(strict bool) (ExtractOptions, error) {
	host, _ := os.Hostname()
	opts := ExtractOptions{Now: time.Now(), Facts: ExtractGitFacts{Host: host}}
	for i := 0; i <= accessGenerations; i++ {
		if _, err := os.Stat(accessGeneration(accessLogPath(), i)); err == nil {
			opts.AccessLogs = []string{accessLogPath()}
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return opts, err
		}
	}
	for _, sub := range []string{"plans", "CLAUDE.md", "rules", "skills", "docs", "agents"} {
		sources, err := extractLinkSources(filepath.Join(paths.ConfigDir(), sub), strict && sub == "plans")
		if err != nil {
			return opts, err
		}
		opts.LinkSources = append(opts.LinkSources, sources...)
	}
	return opts, nil
}

func extractLinkSources(root string, required bool) (sources []string, err error) {
	real, err := filepath.EvalSymlinks(root)
	if errors.Is(err, fs.ErrNotExist) && !required {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	err = extractWalkLinkSources(real, map[string]bool{}, &sources)
	slices.Sort(sources)
	return slices.Compact(sources), err
}

func extractWalkLinkSources(root string, seen map[string]bool, sources *[]string) error {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if seen[real] {
		return nil
	}
	seen[real] = true
	return filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return extractWalkLinkSources(p, seen, sources)
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if d.Type().IsRegular() && strings.HasSuffix(p, ".md") {
			*sources = append(*sources, p)
		}
		return nil
	})
}

func extractOutputOutsideStore(root, out string) error {
	if out != "" && paths.InsideDir(out, root) {
		return errors.New("--out must be outside the memory store")
	}
	return nil
}

func writeExtractOutput(out string, result ExtractResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(out, append(data, '\n'), 0o600)
}

func printExtract(w io.Writer, rep ExtractRunReport) error {
	for _, f := range rep.Folders {
		if err := printExtractFolder(w, f); err != nil {
			return err
		}
	}
	for _, r := range rep.Rows {
		if r.Action == ExtractPurge {
			if _, err := fmt.Fprintf(w, "purge %q %dB %s\n", r.Path, r.Size, r.Reason); err != nil {
				return err
			}
		}
	}
	if rep.RemainingSHA256 != "" || rep.Purged.Files > 0 {
		_, err := fmt.Fprintf(w, "purged=%d/%dB\nremaining_sha256=%s\n", rep.Purged.Files, rep.Purged.Bytes, rep.RemainingSHA256)
		return err
	}
	return nil
}

func printExtractFolder(w io.Writer, f ExtractFolderTotal) error {
	if _, err := fmt.Fprintf(w, "%s", f.Folder); err != nil {
		return err
	}
	for _, a := range []ExtractAction{ExtractKeep, ExtractRewrite, ExtractPurge} {
		n := f.Actions[a]
		if _, err := fmt.Fprintf(w, " %s=%d/%dB", a, n.Files, n.Bytes); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// ExtractGitFacts accepts explicit host-qualified repository/branch or repository/result_commit metadata.
type ExtractGitFacts struct{ Host string }

func extractFactRepository(fm map[string]any) string {
	repo, _ := fm["repository"].(string)
	if !filepath.IsAbs(repo) {
		return ""
	}
	top, err := gitOut(repo, "rev-parse", "--show-toplevel")
	if err != nil || paths.CanonDeep(top) != paths.CanonDeep(repo) {
		return ""
	}
	return repo
}

func (f ExtractGitFacts) WorktreeBranchExists(_ string, data []byte) (bool, bool, error) {
	fm, _ := textutil.ParseFrontmatter(string(data))
	repo := extractFactRepository(fm)
	branch, _ := fm["branch"].(string)
	host, _ := fm["host"].(string)
	if repo == "" || branch == "" || f.Host == "" || host != f.Host {
		return false, false, nil
	}
	if _, err := gitOut(repo, "check-ref-format", "refs/heads/"+branch); err != nil {
		return false, false, nil
	}
	_, err := gitOut(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, true, nil
	}
	var e *exec.ExitError
	if errors.As(err, &e) && e.ExitCode() == 1 {
		return false, true, nil
	}
	return false, false, nil
}

func (ExtractGitFacts) ReportResultInGit(_ string, data []byte) (bool, bool, error) {
	fm, _ := textutil.ParseFrontmatter(string(data))
	repo := extractFactRepository(fm)
	commit, _ := fm["result_commit"].(string)
	if repo == "" || !isHexID(commit) {
		return false, false, nil
	}
	trunk := ""
	for _, ref := range []string{"refs/heads/main", "refs/heads/master", "refs/remotes/origin/HEAD"} {
		if _, err := gitOut(repo, "rev-parse", "--verify", ref+"^{commit}"); err == nil {
			trunk = ref
			break
		}
	}
	if trunk == "" {
		return false, false, nil
	}
	_, err := gitOut(repo, "merge-base", "--is-ancestor", commit, trunk)
	if err != nil {
		return false, false, nil
	}
	return true, true, nil
}

func extractGitInput(dir string, env []string, input string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Env = append(proc.GitEnv(), "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("extract git %s: %w", args[0], err)
	}
	return string(out), nil
}

func extractCleanHead(root string) (string, error) {
	repo := storeRepo{Dir: root}
	if !repo.Exists() {
		return "", errors.New("extract: store must be its own git repository")
	}
	top, err := gitOut(root, "rev-parse", "--show-toplevel")
	if err != nil || paths.CanonDeep(top) != paths.CanonDeep(root) {
		return "", errors.New("extract: invalid store repository root")
	}
	status, err := repo.Status()
	if err != nil {
		return "", err
	}
	if len(status) > 0 {
		return "", errors.New("extract: store git tree is dirty; refusing purge (missing originals remain readable with git show HEAD:<path>)")
	}
	return gitOut(root, "rev-parse", "--verify", "HEAD")
}

type extractRemoval struct {
	row  ExtractRow
	data []byte
	mode fs.FileMode
	mod  time.Time
}

func extractRemovals(root string, result ExtractResult, head string) ([]extractRemoval, error) {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	var removals []extractRemoval
	for _, row := range result.Rows {
		if err := (storeRepo{Dir: root}).validateRel(row.Path); err != nil {
			return nil, err
		}
		data, info, err := extractReadRow(dir, row)
		if err != nil {
			return nil, err
		}
		if row.Action != ExtractPurge {
			continue
		}
		original, err := extractGitInput(root, nil, "", "show", head+":"+row.Path)
		if err != nil || original != string(data) {
			return nil, fmt.Errorf("extract: file not backed up in HEAD: %s", row.Path)
		}
		removals = append(removals, extractRemoval{row, data, info.Mode().Perm(), info.ModTime()})
	}
	return removals, nil
}

func extractReadRow(dir *os.Root, row ExtractRow) ([]byte, fs.FileInfo, error) {
	info, err := dir.Lstat(row.Path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("extract: not a regular file: %s", row.Path)
	}
	data, err := dir.ReadFile(row.Path)
	if err != nil {
		return nil, nil, err
	}
	if textutil.SHA256Hex(data) != row.SHA256 {
		return nil, nil, fmt.Errorf("extract: hash changed: %s", row.Path)
	}
	return data, info, nil
}

func purgeExtract(root string, result ExtractResult, classifiedHead string) (ExtractTotal, error) {
	release, err := extractClaimIndex(root)
	if err != nil {
		return ExtractTotal{}, err
	}
	defer release()
	head, err := extractCleanHead(root)
	if err != nil {
		return ExtractTotal{}, err
	}
	if head != classifiedHead {
		return ExtractTotal{}, errors.New("extract: HEAD changed since classification")
	}
	removals, err := extractRemovals(root, result, head)
	if err != nil || len(removals) == 0 {
		return ExtractTotal{}, err
	}
	name, err := extractIndexFile(root)
	if err != nil {
		return ExtractTotal{}, err
	}
	defer func() { _ = os.Remove(name); _ = os.Remove(name + ".lock") }()
	env := []string{"GIT_INDEX_FILE=" + name}
	total, commit, err := extractPurgeCommit(root, env, head, removals)
	if err != nil {
		return ExtractTotal{}, err
	}
	if err := extractInstallPurge(root, name, removals, head, commit); err != nil {
		return ExtractTotal{}, err
	}
	return total, nil
}

func extractClaimIndex(root string) (func(), error) {
	lock := filepath.Join(root, ".git", "index.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	release := func() { _ = os.Remove(lock) }
	if err := f.Close(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func extractIndexFile(root string) (string, error) {
	temp, err := os.CreateTemp(filepath.Join(root, ".git"), "extract-index-")
	if err != nil {
		return "", err
	}
	name := temp.Name()
	return name, errors.Join(temp.Close(), os.Remove(name))
}

func extractInstallPurge(root, index string, removals []extractRemoval, head, commit string) error {
	dest := filepath.Join(root, ".git", "index")
	old, err := os.ReadFile(dest)
	if err != nil {
		return err
	}
	if err := os.Rename(index, dest); err != nil {
		return err
	}
	if err := extractApplyRemovals(root, removals, head, commit); err != nil {
		return errors.Join(err, paths.AtomicWrite(dest, old, 0o600))
	}
	return nil
}

func extractPurgeCommit(root string, env []string, head string, removals []extractRemoval) (ExtractTotal, string, error) {
	total := ExtractTotal{}
	if _, err := extractGitInput(root, env, "", "read-tree", head); err != nil {
		return total, "", err
	}
	var names strings.Builder
	for _, r := range removals {
		names.WriteString(r.row.Path)
		names.WriteByte(0)
		total.Files++
		total.Bytes += r.row.Size
	}
	if _, err := extractGitInput(root, env, names.String(), "update-index", "--force-remove", "-z", "--stdin"); err != nil {
		return total, "", err
	}
	tree, err := extractGitInput(root, env, "", "write-tree")
	if err != nil {
		return total, "", err
	}
	message := fmt.Sprintf("memory extract: purge %d files, %d bytes", total.Files, total.Bytes)
	commit, err := extractGitInput(root, env, "", "commit-tree", strings.TrimSpace(tree), "-p", head, "-m", message)
	return total, strings.TrimSpace(commit), err
}

func extractApplyRemovals(root string, removals []extractRemoval, head, commit string) (err error) {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	var removed []extractRemoval
	defer func() {
		if err != nil {
			err = errors.Join(err, extractRollback(dir, removed))
		}
	}()
	for _, r := range removals {
		data, e := dir.ReadFile(r.row.Path)
		if e != nil {
			return e
		}
		if textutil.SHA256Hex(data) != r.row.SHA256 {
			return fmt.Errorf("extract: hash changed: %s", r.row.Path)
		}
		if e := dir.Remove(r.row.Path); e != nil {
			return e
		}
		removed = append(removed, r)
	}
	_, err = gitOut(root, "update-ref", "HEAD", commit, head)
	return err
}

func extractRollback(dir *os.Root, removed []extractRemoval) (err error) {
	for _, r := range removed {
		f, e := dir.OpenFile(r.row.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, r.mode)
		if e == nil {
			_, e = f.Write(r.data)
			e = errors.Join(e, f.Close())
		}
		if e == nil {
			e = dir.Chtimes(r.row.Path, r.mod, r.mod)
		}
		if e != nil {
			err = errors.Join(err, fmt.Errorf("extract rollback %s: %w", r.row.Path, e))
		}
	}
	return err
}

func extractRemainingHash(root string) (string, error) {
	hashes := map[string]string{}
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == filepath.Join(root, ".git") {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		hash, err := extractHashFile(p, d)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		names = append(names, rel)
		hashes[rel] = hash
		return nil
	})
	if err != nil {
		return "", err
	}
	slices.Sort(names)
	hash := sha256.New()
	for _, rel := range names {
		fmt.Fprintf(hash, "%d:%s:%s\n", len(rel), rel, hashes[rel])
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func extractHashFile(p string, d fs.DirEntry) (string, error) {
	if d.Type()&os.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		return "symlink:" + textutil.SHA256Hex([]byte(target)), err
	}
	if !d.Type().IsRegular() {
		return "", fmt.Errorf("extract: unsupported file %s", p)
	}
	data, err := os.ReadFile(p)
	return "file:" + textutil.SHA256Hex(data), err
}

// dreamExtract consolidates one over-budget rewrite group (no model under budget) and prints the before and after line.
func dreamExtract(o dreamOptions, agentErr error) {
	if agentErr != nil {
		return
	}
	root := paths.MemoryStoreDir()
	opts, err := extractRunOptions(false)
	if err != nil {
		fmt.Fprintln(o.Out, "memory dream: extract:", err)
		return
	}
	result, err := ClassifyExtract(root, opts)
	if err != nil {
		fmt.Fprintln(o.Out, "memory dream: extract:", err)
		return
	}
	for _, folder := range result.Folders {
		for _, group := range folder.Groups {
			if folder.OverBudget == 0 || group.Action != ExtractRewrite {
				continue
			}
			report, err := dreamExtractGroup(root, group, opts)
			if err != nil {
				fmt.Fprintln(o.Out, "memory dream: extract refused:", err)
				return
			}
			fmt.Fprintf(o.Out, "memory dream: extract %s consolidated %d files; %s\n", folder.Folder, len(group.Paths), report)
			return
		}
	}
}

func dreamExtractGroup(root string, group ExtractGroup, opts ExtractOptions) (report ExtractRecallReport, err error) {
	if len(opts.AccessLogs) == 0 {
		return report, ErrExtractNoAccessLog
	}
	if report.BytesBefore, report.ScanBefore, err = ExtractRecallMeasure(root); err != nil {
		return report, err
	}
	sources, err := dreamReadSources(root, group.Paths)
	if err != nil {
		return report, err
	}
	ans, err := RewriteExtractGroup(context.Background(), root, group, "")
	if err != nil {
		return report, err
	}
	err = extractRunLocked(func() error {
		checked, applyErr := dreamApplyChecked(root, group, ans, sources, opts)
		report.Queried, report.Lost = checked.Queried, checked.Lost
		return applyErr
	})
	if err != nil {
		return report, err
	}
	report.BytesAfter, report.ScanAfter, err = ExtractRecallMeasure(root)
	return report, err
}

func dreamReadSources(root string, rels []string) (map[string][]byte, error) {
	sources := make(map[string][]byte, len(rels))
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		sources[rel] = data
	}
	return sources, nil
}

// dreamApplyChecked runs under the store lock: sources unchanged, recall check, ledger debit first, then apply.
func dreamApplyChecked(root string, group ExtractGroup, ans ExtractRewriteAnswer, seen map[string][]byte, opts ExtractOptions) (ExtractRecallReport, error) {
	now, err := dreamReadSources(root, group.Paths)
	if err != nil {
		return ExtractRecallReport{}, err
	}
	if !maps.EqualFunc(seen, now, bytes.Equal) {
		return ExtractRecallReport{}, errors.New("extract sources changed while the model ran")
	}
	ledger := filepath.Join(paths.MemoryStateDir(), "extract-loss.log")
	// An access logged after this read is not counted: the limit is statistical and a purged source stays in git.
	report, err := CheckExtractRecall(opts.AccessLogs, ledger, now, ans, time.Now())
	if err != nil {
		return report, err
	}
	if err := RecordExtractLoss(ledger, time.Now(), report.Lost); err != nil {
		return report, err
	}
	_, err = ApplyExtractRewrite(storeRepo{Dir: root}, group, ans, time.Now())
	return report, err
}
