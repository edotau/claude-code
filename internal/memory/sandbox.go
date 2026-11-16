package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/vault"
)

// SandboxOwnerFile is the marker in a sandbox root naming the process that created it.
const SandboxOwnerFile = "owner.json"

// SandboxOwner is the marker's content: the creating process and when it started.
type SandboxOwner struct {
	PID   int   `json:"pid"`
	Start int64 `json:"start"`
}

// SandboxRegistry lists every sandbox dir ever created, whatever TMPDIR it was under.
func SandboxRegistry() string { return filepath.Join(StateDir(), "sandboxes.list") }

// UpdateSandboxRegistry adds dir when non-empty, drops entries whose dir is gone, and returns the live ones.
func UpdateSandboxRegistry(add string) (live []string, err error) {
	file := SandboxRegistry()
	if err = os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return nil, err
	}
	err = paths.WithFileLock(file, func() error {
		data, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		live = liveSandboxDirs(append(strings.Split(string(data), "\n"), add))
		body := strings.Join(live, "\n") + "\n"
		if len(live) == 0 {
			body = ""
		}
		if body == string(data) {
			return nil
		}
		return paths.AtomicWrite(file, []byte(body), 0o600)
	})
	return live, err
}

// liveSandboxDirs keeps the absolute, unique entries whose dir is not known to be gone (any doubt keeps it).
func liveSandboxDirs(entries []string) (live []string) {
	for _, l := range entries {
		l = strings.TrimRight(l, "\r")
		if _, err := os.Lstat(l); l == "" || !filepath.IsAbs(l) || slices.Contains(live, l) || errors.Is(err, fs.ErrNotExist) {
			continue
		}
		live = append(live, l)
	}
	return live
}

// recordSandbox writes the owner marker into dir and registers dir for the clean sweep.
func recordSandbox(dir string) error {
	data, err := json.Marshal(SandboxOwner{PID: os.Getpid(), Start: time.Now().Unix()})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, SandboxOwnerFile), data, 0o600); err != nil {
		return err
	}
	_, err = UpdateSandboxRegistry(dir)
	return err
}

// SandboxOptions selects the stores and whether their contents are copied.
type SandboxOptions struct {
	Repo       string
	All, Empty bool
}

// SandboxStore reports the number of bank files placed in one store.
type SandboxStore struct {
	Key   string `json:"key"`
	Files int    `json:"files"`
}

// SandboxResult describes a disposable config with an origin-only vault and no embedding cache.
type SandboxResult struct {
	Dir    string            `json:"dir"`
	Env    map[string]string `json:"env"`
	Stores []SandboxStore    `json:"stores"`
}

// Sandbox creates a disposable config without writing to the source bank.
func Sandbox(opt SandboxOptions) (res SandboxResult, err error) {
	stores, origins, err := sandboxSources(opt)
	if err != nil {
		return res, err
	}
	res.Dir, err = os.MkdirTemp("", "claude-memory-sandbox-")
	if err != nil {
		return res, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(res.Dir))
		}
	}()
	if res.Dir, err = filepath.Abs(res.Dir); err != nil {
		return res, err
	}
	if err = recordSandbox(res.Dir); err != nil {
		return res, err
	}
	res.Env = map[string]string{"CLAUDE_CONFIG_DIR": res.Dir}
	if storeFlat() {
		res.Stores, err = sandboxFlat(res.Dir, opt)
		return res, err
	}
	var files []string
	for _, key := range slices.Sorted(maps.Keys(stores)) {
		names, copyErr := sandboxStore(res.Dir, key, stores[key], opt.Empty)
		if copyErr != nil {
			return res, copyErr
		}
		files = append(files, names...)
		res.Stores = append(res.Stores, SandboxStore{key, len(names)})
	}
	if err = sandboxOrigins(res.Dir, origins); err != nil {
		return res, err
	}
	err = sandboxSnapshot(filepath.Join(res.Dir, "memory"), files)
	return res, err
}

func sandboxSources(opt SandboxOptions) (map[string]string, []Origin, error) {
	if storeFlat() { // the flat copy needs no origin rows or store dirs
		return nil, nil, nil
	}
	root, err := ResolveRoot(opt.Repo)
	if err != nil {
		return nil, nil, importInput("%v", err)
	}
	entries, err := RepoDirs()
	if err != nil {
		return nil, nil, err
	}
	selected := BankFor(root)
	stores := map[string]string{"home": HomeBank().SubDir(""), selected.Key: selected.SubDir("")}
	if opt.All {
		for _, e := range entries {
			stores[e.Key] = e.Dir
		}
	}
	var origins []Origin
	for _, key := range slices.Sorted(maps.Keys(stores)) {
		if key == "home" {
			continue
		}
		o := Origin{Version: OriginVersion, Key: key, Origin: Resolve(root).Origin}
		for _, e := range entries {
			if e.Key == key {
				o = e.Origin
				break
			}
		}
		origins = append(origins, o)
	}
	return stores, origins, nil
}

// sandboxFlat copies the selected banks' files (by prefix) into config's flat store, stamped, with no origin rows.
func sandboxFlat(config string, opt SandboxOptions) ([]SandboxStore, error) {
	_, sel, err := ResolveBank(opt.Repo)
	if err != nil {
		return nil, importInput("%v", err)
	}
	banks := []Bank{HomeBank(), sel}
	if opt.All {
		banks = Banks()
	}
	dst := filepath.Join(config, "memory")
	var files []string
	var out []SandboxStore
	for _, b := range slices.CompactFunc(banks, func(x, y Bank) bool { return x.Key == y.Key }) {
		names, err := sandboxFlatBank(dst, b, opt.Empty)
		if err != nil {
			return nil, err
		}
		files = append(files, names...)
		out = append(out, SandboxStore{stateKey(b), len(names)})
	}
	if err := os.WriteFile(filepath.Join(dst, layoutFile), []byte(layoutFlat+"\n"), 0o600); err != nil {
		return nil, err
	}
	return out, sandboxSnapshot(dst, append(files, layoutFile))
}

// sandboxFlatBank places b's files in the flat store at dst: its templates when empty, else every file it owns.
func sandboxFlatBank(dst string, b Bank, empty bool) ([]string, error) {
	var rels []string
	if empty {
		for name := range templates() {
			rels = append(rels, filepath.Base(b.Path(name)))
		}
	} else {
		rels = ownedStoreRels(b)
	}
	for _, rel := range rels {
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, err
		}
		data := []byte(templates()[strings.TrimPrefix(rel, b.Prefix())])
		if !empty {
			var err error
			if data, err = os.ReadFile(filepath.Join(StoreDir(), filepath.FromSlash(rel))); err != nil {
				return nil, err
			}
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return nil, err
		}
	}
	return rels, nil
}

// ownedStoreRels lists the store-relative regular files a flat bank owns: top-level and in the shared subdirs.
func ownedStoreRels(b Bank) []string {
	var rels []string
	for _, dir := range append([]string{""}, flatSubs...) {
		ents, _ := os.ReadDir(filepath.Join(StoreDir(), dir))
		for _, e := range ents {
			rel := path.Join(dir, e.Name())
			if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") && !storeLevelFile(rel) && BankOfFile(rel).Key == b.Key {
				rels = append(rels, rel)
			}
		}
	}
	return rels
}

// storeLevelFile is a top-level file shared by every bank: the index, the catalog.
func storeLevelFile(rel string) bool { return rel == nativeIndexFile || rel == catalogFile }

func sandboxStore(config, key, src string, empty bool) ([]string, error) {
	dst := filepath.Join(config, "memory", key)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return nil, err
	}
	p := &importPlan{topics: true}
	if !empty {
		if err := p.planStore(importPair{key: key, src: src, tgt: dst}); err != nil {
			return nil, err
		}
	}
	if empty {
		for name, body := range templates() {
			p.changes = append(p.changes, Change{Rel: storeRel(dst, name), Data: []byte(body)})
		}
	}
	changes := slices.DeleteFunc(p.changes, func(c Change) bool { return filepath.Base(c.Rel) == ".git" })
	var files []string
	for _, c := range changes {
		target := filepath.Join(StoreDir(), c.Rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, c.Data, 0o600); err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(filepath.Join(config, "memory"), target)
		if err != nil {
			return nil, err
		}
		files = append(files, filepath.ToSlash(rel))
	}
	return files, nil
}

// sandboxOrigins writes the origins into the sandbox vault by path; no source vault bucket is copied.
func sandboxOrigins(config string, origins []Origin) error {
	return vault.UpdateAt(filepath.Join(config, "state", "vault.db"), func(tx *vault.Tx) error {
		for _, o := range origins {
			data, err := json.Marshal(o)
			if err != nil {
				return err
			}
			if err := tx.Put(vault.MemoryOrigins, o.Key, string(data)); err != nil {
				return err
			}
		}
		return nil
	})
}

func sandboxSnapshot(dir string, files []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	if _, err := proc.RunGit(ctx, dir, "init", "--quiet", "--template="); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "update-index", "--add", "-z", "--stdin")
	cmd.Env = proc.GitEnv()
	if len(files) > 0 {
		cmd.Stdin = strings.NewReader(strings.Join(files, "\x00") + "\x00")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("snapshot index: %w: %s", err, out)
	}
	tree, err := proc.RunGit(ctx, dir, "write-tree")
	if err != nil {
		return err
	}
	args := proc.Values{"user.name": "Memory sandbox", "user.email": "sandbox@localhost"}.Args("-c")
	commit, err := proc.RunGit(ctx, dir, append(args, "commit-tree", tree, "-m", "chore(memory): seed sandbox")...)
	if err != nil {
		return err
	}
	_, err = proc.RunGit(ctx, dir, "update-ref", "HEAD", commit)
	return err
}
