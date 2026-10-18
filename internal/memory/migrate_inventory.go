package memory

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// InventoryOptions: Origins (slug → checkout path, from --origin flags) win over .claude.json; Now stamps Created.
type InventoryOptions struct {
	Origins map[string]string
	Now     time.Time
}

// inventory is one dry run's working state; every method is read-only on disk. m.Origins doubles as the slug map.
type inventory struct {
	m          *Manifest
	cfg, home  string
	explicit   map[string]bool   // slugs pinned by --origin (never reported ambiguous)
	ambiguous  map[string]bool   // slugs two origins claim
	dests      map[string]string // canonical origin → store dir
	aliasOf    map[string]string // canonical alias path → canonical origin (~/orca/claude-code → .claude)
	originItem map[string]string // store dir → pending origin item ID
	destTaken  map[string]string // dest → item ID claiming it
	seen       map[string]bool
}

// Inventory builds the migration manifest from the LEGACY trees only (never StoreDir()); it writes nothing.
func Inventory(o InventoryOptions) (Manifest, error) {
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	home, _ := os.UserHomeDir()
	m := &Manifest{Schema: ManifestSchema, Created: now, ConfigDir: paths.ConfigDir(), Store: StoreDir(), Origins: map[string]string{}}
	inv := &inventory{m: m, cfg: paths.ConfigDir(), home: home, explicit: map[string]bool{}, ambiguous: map[string]bool{},
		dests: map[string]string{}, aliasOf: map[string]string{}, originItem: map[string]string{}, destTaken: map[string]string{}, seen: map[string]bool{}}
	if err := inv.resolveOrigins(o.Origins); err != nil {
		return Manifest{}, err
	}
	inv.legacyTree()
	inv.nativeStores()
	inv.handoffs()
	inv.agentMemory()
	inv.precheck()
	return *m, nil
}

func (inv *inventory) finding(format string, args ...any) {
	inv.m.Findings = append(inv.m.Findings, fmt.Sprintf(format, args...))
}

// resolveOrigins registers the config checkout, the ~/orca/claude-code alias, $HOME, --origin flags, then .claude.json.
func (inv *inventory) resolveOrigins(explicit map[string]string) error {
	var aliases []string
	orca := filepath.Join(inv.home, "orca", "claude-code")
	if canon, err := filepath.EvalSymlinks(orca); err == nil && isDir(canon) {
		aliases = append(aliases, canon)
	}
	cfgOrigin := inv.register(inv.cfg, "", aliases)
	for _, a := range aliases {
		inv.aliasOf[a] = cfgOrigin
		inv.mapSlug(paths.RepoSlug(a), cfgOrigin)
		inv.mapSlug(projectSlug(a), cfgOrigin)
		inv.mapSlug(projectSlug(orca), cfgOrigin)
	}
	if inv.home != "" {
		inv.register(inv.home, "", nil)
	}
	for _, slug := range sortedKeys(explicit) {
		inv.explicit[slug] = true
		if inv.register(explicit[slug], slug, nil) == "" {
			return fmt.Errorf("--origin %s=%s: not an existing directory", slug, explicit[slug])
		}
	}
	for _, key := range claudeJSONProjects() {
		inv.register(key, "", nil)
	}
	return nil
}

// register maps path's canonical origin to a store dir and records its slugs; "" when path is no checkout.
func (inv *inventory) register(path, explicitSlug string, aliases []string) string {
	canon, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil || !isDir(canon) {
		return ""
	}
	if origin, ok := inv.aliasOf[canon]; ok {
		return origin
	}
	id, isHome := Resolve(canon), paths.SamePath(canon, inv.home)
	if id.Global && explicitSlug != "" && !isHome { // a pinned non-git workspace gets its own store
		id = Identity{Origin: canon, Base: filepath.Base(canon)}
	}
	origin := id.Origin
	if isHome {
		origin = canon
	}
	if _, known := inv.dests[origin]; !known {
		dest, err := inv.destFor(id, canon, isHome)
		if err != nil {
			inv.finding("origin %s: %v", canon, err)
		}
		if dest == "" {
			return ""
		}
		inv.dests[origin] = dest
		inv.originItemFor(dest, origin, aliases)
	}
	inv.mapSlug(paths.RepoSlug(canon), origin)
	inv.mapSlug(projectSlug(canon), origin)
	inv.mapSlug(projectSlug(filepath.Clean(path)), origin) // native slugs come from the cwd as typed, symlinks unresolved
	if explicitSlug != "" {
		inv.m.Origins[explicitSlug] = origin
	}
	return origin
}

// destFor: $HOME → the store root, the config checkout → HarnessStoreDir, a repo → RepoDir, a non-repo → "".
func (inv *inventory) destFor(id Identity, canon string, isHome bool) (string, error) {
	switch {
	case isHome:
		return StoreDir(), nil
	case id.Harness:
		return HarnessStoreDir(), nil
	case id.Global:
		return "", nil
	}
	entries, err := RepoDirs()
	if err != nil {
		return "", err
	}
	if e := findEntry(entries, id.Origin); e != nil {
		return e.Dir, nil
	}
	disk := takenKeys(entries)
	taken := func(key string) (Origin, bool) { // disk plus the stores this run already planned: same-basename repos diverge
		if o, ok := disk(key); ok {
			return o, true
		}
		for origin, dest := range inv.dests {
			if dest == filepath.Join(StoreDir(), key) {
				return Origin{Origin: origin}, true
			}
		}
		return Origin{}, false
	}
	key, err := slugKey(id.Base, id.Origin, taken)
	if err != nil {
		return "", err
	}
	return filepath.Join(StoreDir(), key), nil
}

// mapSlug binds a legacy or project slug to origin; two different origins make it ambiguous unless --origin pinned it.
func (inv *inventory) mapSlug(slug, origin string) {
	if slug == "" || inv.explicit[slug] || inv.ambiguous[slug] {
		return
	}
	if prev, ok := inv.m.Origins[slug]; ok && prev != origin {
		inv.ambiguous[slug] = true
		delete(inv.m.Origins, slug)
		inv.finding("slug %s is ambiguous (%s, %s): pass --origin %s=<path>", slug, prev, origin, slug)
		return
	}
	inv.m.Origins[slug] = origin
}

func (inv *inventory) destOf(slug string) (string, bool) {
	d, ok := inv.dests[inv.m.Origins[slug]]
	return d, ok
}

// originItemFor plans dest/.origin when it is absent or lacks an alias; the store root never has one.
func (inv *inventory) originItemFor(dest, origin string, aliases []string) {
	if _, done := inv.originItem[dest]; done || paths.SamePath(dest, StoreDir()) {
		return
	}
	if cur, err := ReadOrigin(dest); err == nil {
		complete := true
		for _, a := range aliases {
			complete = complete && slices.Contains(cur.Aliases, a)
		}
		if complete {
			return
		}
	}
	data, _ := json.MarshalIndent(Origin{Version: OriginVersion, Key: filepath.Base(dest), Origin: origin, Aliases: aliases}, "", "  ")
	data = append(data, '\n') // the exact bytes putOrigin writes, so the manifest hash matches the file
	file := filepath.Join(dest, OriginFile)
	it := MigrateItem{ID: itemID("inline:" + configRel(file)), Kind: KindOriginItem, Dest: file, Inline: data,
		Bytes: int64(len(data)), SHA256: sha256Hex(data), Action: ActionCopy, Reason: "origin record for " + origin}
	inv.m.Items = append(inv.m.Items, it)
	inv.originItem[dest] = it.ID
}

// claudeJSONProjects lists the real project paths Claude Code recorded, sorted.
func claudeJSONProjects() []string {
	var doc struct {
		Projects map[string]json.RawMessage `json:"projects"`
	}
	if data, err := os.ReadFile(paths.ClaudeJSON()); err != nil || json.Unmarshal(data, &doc) != nil {
		return nil
	}
	return sortedKeys(doc.Projects)
}

// add records one item: hashes Source, derives the ID, attaches the origin dependency and guards dest collisions.
func (inv *inventory) add(it MigrateItem) string {
	if it.Source != "" {
		it.ID = itemID(configRel(it.Source))
		if it.Kind != KindEmptyDir {
			h, n, err := sha256File(it.Source)
			if err != nil {
				it.Action, it.Reason = ActionSkip, "unreadable: "+err.Error()
				inv.finding("skip %s: %v", it.Source, err)
			}
			it.SHA256, it.Bytes = h, n
		}
	}
	if inv.seen[it.ID] {
		return it.ID
	}
	for dir, id := range inv.originItem {
		if it.Dest != "" && strings.HasPrefix(it.Dest, dir+string(filepath.Separator)) {
			it.Deps = append(it.Deps, id)
		}
	}
	if it.Action == ActionCopy && it.Dest != "" {
		if prev, taken := inv.destTaken[it.Dest]; taken {
			it.Action, it.Dest, it.Reason = ActionSkip, "", "conflict: another source ("+prev+") targets "+it.Dest
			inv.finding("%s: %s", it.Source, it.Reason)
		} else {
			inv.destTaken[it.Dest] = it.ID
		}
	}
	if it.Action == ActionQuarantine {
		inv.finding("quarantine %s: %s", it.Source, it.Reason)
	}
	inv.seen[it.ID] = true
	inv.m.Items = append(inv.m.Items, it)
	return it.ID
}

// tree plans every regular file under root: copied to the same rel under destRoot, or (destRoot "") given action.
func (inv *inventory) tree(root, destRoot, kind, action, reason string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil || d.IsDir():
		case !d.Type().IsRegular():
			inv.finding("left in place (not a regular file): %s", p)
		case destRoot == "":
			inv.add(MigrateItem{Kind: kind, Source: p, Action: action, Reason: reason})
		default:
			rel, _ := filepath.Rel(root, p)
			inv.add(MigrateItem{Kind: kind, Source: p, Dest: filepath.Join(destRoot, rel), Action: ActionCopy, Reason: reason})
		}
		return nil
	})
}

func (inv *inventory) quarantineTree(root, reason string) {
	inv.tree(root, "", KindUnknownItem, ActionQuarantine, reason)
}

// legacyTree inventories GlobalDir(): the global six, archive/, <slug>/ banks and anything unknown.
func (inv *inventory) legacyTree() {
	g := LegacyGlobalDir()
	ents, _ := os.ReadDir(g)
	for _, e := range ents {
		name, p := e.Name(), filepath.Join(g, e.Name())
		switch {
		case e.Type()&fs.ModeSymlink != 0:
			inv.finding("left in place (symlink): %s", p)
		case e.IsDir() && name == "archive":
			inv.legacyArchive(p)
		case e.IsDir() && hasTypedFile(p):
			inv.legacyBank(p, name)
		case e.IsDir():
			inv.quarantineTree(p, "unknown directory in the legacy bank tree")
		default:
			inv.bankFile(g, name, StoreDir(), "global", KindGlobalBank)
		}
	}
}

// bankFile classifies one top-level regular file of a bank dir (global or <slug>) and plans it.
func (inv *inventory) bankFile(dir, name, dest, key, typedKind string) {
	p := filepath.Join(dir, name)
	switch {
	case slices.Contains(bankFiles, name):
		inv.add(MigrateItem{Kind: typedKind, Source: p, Dest: filepath.Join(dest, name), Action: ActionCopy, Reason: "typed bank file"})
	case name == CatalogFile:
		inv.add(MigrateItem{Kind: KindLegacyIndex, Source: p, Action: ActionQuarantine, Reason: "legacy index; regenerated after cutover"})
	case name == "access.log":
		inv.add(MigrateItem{Kind: KindAccessLog, Source: p, Dest: filepath.Join(AccessStateDir(), key+".log"), Action: ActionCopy, Reason: "access log → state"})
	case isLockFile(name):
		inv.add(MigrateItem{Kind: KindLockFile, Source: p, Action: ActionDelete, Reason: "stale lock file"})
	case strings.HasSuffix(name, ".md") && classifyMemoryFile(dir, name) == KindTopic:
		inv.add(MigrateItem{Kind: KindNativeTopic, Source: p, Dest: filepath.Join(dest, name), Action: ActionCopy, Reason: "topic file"})
	default:
		inv.add(MigrateItem{Kind: KindUnknownItem, Source: p, Action: ActionQuarantine, Reason: "unknown file in bank " + key})
	}
}

// legacyArchive plans GlobalDir()/archive: *.md to the store archive, native/<pslug>/** to the owning repo.
func (inv *inventory) legacyArchive(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		name, p := e.Name(), filepath.Join(dir, e.Name())
		switch {
		case e.IsDir() && name == "native":
			subs, _ := os.ReadDir(p)
			for _, s := range subs {
				inv.nativeArchive(filepath.Join(p, s.Name()), s.Name())
			}
		case e.IsDir():
			inv.quarantineTree(p, "unknown archive directory")
		case e.Type().IsRegular() && strings.HasSuffix(name, ".md"):
			inv.add(MigrateItem{Kind: KindRepoArchive, Source: p, Dest: filepath.Join(ArchiveDir(StoreDir()), name), Action: ActionCopy, Reason: "global archive"})
		default:
			inv.add(MigrateItem{Kind: KindUnknownItem, Source: p, Action: ActionQuarantine, Reason: "unknown archive entry"})
		}
	}
}

// nativeArchive sends an archived native tree for pslug to <repo dir>/archive/native, or quarantines it unresolved.
func (inv *inventory) nativeArchive(root, pslug string) {
	if dest, ok := inv.destOf(pslug); ok {
		inv.tree(root, filepath.Join(dest, "archive", "native"), KindArchiveNative, ActionCopy, "native archive")
		return
	}
	inv.finding("no origin for native archive %s: pass --origin %s=<path>", pslug, pslug)
	inv.tree(root, "", KindArchiveNative, ActionQuarantine, "no origin for native archive "+pslug)
}

// legacyBank plans one GlobalDir()/<slug>/ bank: pristine → delete; resolved → import; unresolved → skip.
func (inv *inventory) legacyBank(dir, slug string) {
	ents, _ := os.ReadDir(dir)
	if isPristine(dir) {
		var deps []string
		for _, e := range ents {
			kind, why := KindPristine, "byte-equal to the legacy template (C.5)"
			if isLockFile(e.Name()) {
				kind, why = KindLockFile, "stale lock file"
			}
			deps = append(deps, inv.add(MigrateItem{Kind: kind, Source: filepath.Join(dir, e.Name()), Action: ActionDelete, Reason: why}))
		}
		inv.add(MigrateItem{Kind: KindEmptyDir, Source: dir, Action: ActionDelete, Reason: "pristine bank " + slug, Deps: deps})
		return
	}
	dest, ok := inv.destOf(slug)
	if !ok {
		reason := fmt.Sprintf("no origin for legacy bank %s: pass --origin %s=<path>", slug, slug)
		inv.finding("%s", reason)
		inv.tree(dir, "", KindUnknownItem, ActionSkip, reason)
		return
	}
	for _, e := range ents {
		name, p := e.Name(), filepath.Join(dir, e.Name())
		switch {
		case e.IsDir() && name == "archive":
			inv.tree(p, ArchiveDir(dest), KindRepoArchive, ActionCopy, "bank archive")
		case e.IsDir():
			inv.quarantineTree(p, "unknown directory in legacy bank "+slug)
		case !e.Type().IsRegular():
			inv.finding("left in place (not a regular file): %s", p)
		default:
			inv.bankFile(dir, name, dest, filepath.Base(dest), KindRepoBank)
		}
	}
}

// nativeStores plans projects/*/memory: a link is done, a real dir is imported file by file.
func (inv *inventory) nativeStores() {
	projects := filepath.Join(inv.cfg, "projects")
	ents, _ := os.ReadDir(projects)
	for _, e := range ents {
		pslug := e.Name()
		mem := filepath.Join(projects, pslug, "memory")
		fi, err := os.Lstat(mem)
		if !e.IsDir() || err != nil {
			continue
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			inv.finding("already linked: %s", mem)
			continue
		}
		if !fi.IsDir() {
			inv.finding("left in place (not a directory): %s", mem)
			continue
		}
		items, _ := os.ReadDir(mem)
		if len(items) == 0 {
			inv.add(MigrateItem{Kind: KindEmptyDir, Source: mem, Action: ActionDelete, Reason: "empty native store"})
			continue
		}
		dest, ok := inv.destOf(pslug)
		if !ok {
			reason := fmt.Sprintf("no origin for native store %s: pass --origin %s=<path>", pslug, pslug)
			inv.finding("%s", reason)
			inv.tree(mem, "", KindUnknownItem, ActionSkip, reason)
			continue
		}
		for _, it := range items {
			name, p := it.Name(), filepath.Join(mem, it.Name())
			switch {
			case it.IsDir() && name == "archive":
				inv.tree(p, filepath.Join(dest, "archive", "native"), KindNativeArchive, ActionCopy, "native archive")
			case it.IsDir():
				inv.quarantineTree(p, "unknown directory in native store "+pslug)
			default:
				inv.nativeFile(mem, name, dest)
			}
		}
	}
}

// nativeFile plans one top-level file of a real native store by its directory-aware kind.
func (inv *inventory) nativeFile(mem, name, dest string) {
	p, to := filepath.Join(mem, name), filepath.Join(dest, name)
	switch classifyMemoryFile(mem, name) {
	case KindTopic:
		inv.add(MigrateItem{Kind: KindNativeTopic, Source: p, Dest: to, Action: ActionCopy, Reason: "native topic"})
	case KindNativeIndex, KindSearchable:
		inv.add(MigrateItem{Kind: KindNativeIndexIt, Source: p, Dest: to, Action: ActionCopy, Reason: "native index"})
	case KindBank:
		if _, taken := inv.destTaken[to]; taken || exists(to) { // "conflict:" makes apply block, not skip (copyPhase)
			inv.finding("typed-name collision: %s would land on typed file %s", p, to)
			inv.add(MigrateItem{Kind: KindNativeTopic, Source: p, Action: ActionSkip, Reason: "conflict: typed-name collision with " + to})
			return
		}
		inv.add(MigrateItem{Kind: KindNativeTopic, Source: p, Dest: to, Action: ActionCopy, Reason: "native file with a typed name; no typed file exists"})
	default:
		if isLockFile(name) {
			inv.add(MigrateItem{Kind: KindLockFile, Source: p, Action: ActionDelete, Reason: "stale lock file"})
			return
		}
		inv.add(MigrateItem{Kind: KindUnknownItem, Source: p, Action: ActionQuarantine, Reason: "unknown file in native store"})
	}
}

// handoffs plans docs/handoffs: long handoffs → the harness store; the beacon and run artefacts → delete (C.5).
// HANDOFF.md is always a regenerated beacon (fixed name+path, never a user document) and every *.plan.json/
// *.critic.json is a disposable run artefact — neither needs a content check to be safe to delete.
func (inv *inventory) handoffs() {
	dir := filepath.Join(inv.cfg, "docs", "handoffs")
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		name, p := e.Name(), filepath.Join(dir, e.Name())
		switch {
		case !e.Type().IsRegular():
			inv.finding("left in place (not a regular file): %s", p)
		case name == "HANDOFF.md":
			inv.add(MigrateItem{Kind: KindArtifact, Source: p, Action: ActionDelete, Reason: "stale beacon (C.5)"})
		case strings.HasSuffix(name, ".plan.json") || strings.HasSuffix(name, ".critic.json"):
			inv.add(MigrateItem{Kind: KindArtifact, Source: p, Action: ActionDelete, Reason: "run artefact (C.5)"})
		case strings.HasSuffix(name, ".md"):
			inv.add(MigrateItem{Kind: KindHandoff, Source: p, Dest: filepath.Join(HandoffsDir(HarnessStoreDir()), name), Action: ActionCopy, Reason: "handoff → harness store"})
		default:
			inv.finding("left in place (unexpected handoff entry): %s", p)
		}
	}
}

// agentMemory plans ConfigDir()/agent-memory: an empty tree is removed (deepest first at retire), else quarantined.
func (inv *inventory) agentMemory() {
	root := filepath.Join(inv.cfg, "agent-memory")
	if !isDir(root) {
		return
	}
	var dirs []string
	files := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		} else if err == nil {
			files++
		}
		return nil
	})
	if files > 0 {
		inv.quarantineTree(root, "unknown content under agent-memory")
		return
	}
	for _, d := range dirs {
		inv.add(MigrateItem{Kind: KindEmptyDir, Source: d, Action: ActionDelete, Reason: "empty agent-memory directory"})
	}
}

// precheck: a dest with different bytes blocks; equal bytes stay a copy so apply re-verifies, commits and retires it.
func (inv *inventory) precheck() {
	for i := range inv.m.Items {
		it := &inv.m.Items[i]
		if it.Action != ActionCopy || it.Dest == "" || it.Inline != nil || !exists(it.Dest) {
			continue
		}
		if h, _, err := sha256File(it.Dest); err == nil && h == it.SHA256 {
			it.Reason += "; dest already holds equal bytes"
			continue
		}
		it.Action, it.Reason = ActionSkip, "conflict: dest exists with different content"
		inv.finding("%s: conflict: %s exists with different content", it.Source, it.Dest)
	}
}
