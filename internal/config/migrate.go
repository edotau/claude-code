package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vendorconfig"
)

var (
	legacyKeyringLookup    = keyring.Lookup
	legacyKeyringAvailable = keyring.Available
)

type migrationGroup struct {
	kind   string
	items  []Item
	values map[string]string
	doc    []byte
	meta   legacyMetadata
	names  []string
	err    error
}

// Migrate imports absent vault entries and retains the original files as manifest-owned backups.
func Migrate(w io.Writer, dryRun bool) error {
	m, err := loadManifest()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(LegacyFiles()) == 0 {
		if hasPendingRenames(m) {
			if m.Finalizing {
				return errors.New("finalize already started; migration refused")
			}
			if err := resumeMigration(w, m, dryRun); err != nil {
				return err
			}
		}
		fmt.Fprintln(w, "nothing to migrate")
		return nil
	}
	if dryRun {
		return migrate(w, true)
	}
	return withMigrationLocks(legacyLocks(), func() error { return migrate(w, false) })
}

func hasPendingRenames(m *Manifest) bool {
	if m != nil {
		for _, item := range m.Items {
			if !item.Done {
				return true
			}
		}
	}
	return false
}

func withMigrationLocks(locks []string, fn func() error) error {
	if len(locks) == 0 {
		return fn()
	}
	return paths.WithFileLock(locks[0], func() error {
		return withMigrationLocks(locks[1:], fn)
	})
}

func migrate(w io.Writer, dryRun bool) error {
	m, err := loadManifest()
	if errors.Is(err, fs.ErrNotExist) {
		m = &Manifest{Keyring: []string{}}
	} else if err != nil {
		return err
	}
	if m.Finalizing {
		return errors.New("finalize already started; migration refused")
	}
	if err := resumeMigration(w, m, dryRun); err != nil {
		return err
	}
	sources := legacySources()
	if dryRun {
		var remaining []legacySource
		for _, source := range sources {
			pending := false
			for _, item := range m.Items {
				pending = pending || (!item.Done && item.Source == source.path)
			}
			if !pending {
				remaining = append(remaining, source)
			}
		}
		sources = remaining
	}
	if len(sources) == 0 {
		fmt.Fprintln(w, "nothing to migrate")
		return nil
	}
	groups := parseMigration(sources, m)
	var failures []error
	report := func(err error) {
		fmt.Fprintf(w, "migrate: %v\n", err)
		failures = append(failures, err)
	}
	for _, g := range groups {
		if g.err != nil {
			report(g.err)
		}
	}
	var committed []*migrationGroup
	var keyringNames []string
	for _, g := range groups {
		if g.err != nil {
			continue
		}
		// Every other group needs a file to import; the secrets group also has keyring-only
		// candidates (an `@keyring` ref, provider auth.env, CLAUDE_CODE_OAUTH_TOKEN) with no
		// secrets.env/secrets.meta.json file, so it must still run or those names are silently
		// stranded once their ref is rewritten to @secret with nothing sealed to back it.
		if g.kind != "secrets" && len(g.items) == 0 {
			continue
		}
		var err error
		if g.kind == "secrets" {
			var found []string
			found, err = migrateSecrets(g, groups, m, dryRun)
			if err == nil {
				keyringNames = append(keyringNames, found...)
			}
		} else {
			err = migrateConfigGroup(w, g, dryRun)
		}
		if err != nil {
			report(err)
			continue
		}
		committed = append(committed, g)
		switch {
		case len(g.items) != 0:
			fmt.Fprintf(w, "%-9s %d imported from %s\n", g.kind, len(g.names), legacyLabel(g.items[0].Source))
		case len(g.names) != 0:
			fmt.Fprintf(w, "%-9s %d imported from the keyring\n", g.kind, len(g.names))
		}
	}
	count := 0
	for _, g := range committed {
		count += len(g.items)
	}
	if dryRun {
		fmt.Fprintf(w, "would migrate %d file(s)\n", count)
		return errors.Join(failures...)
	}
	if count == 0 {
		return errors.Join(failures...)
	}
	now := time.Now().UTC()
	if err := vault.Update(func(t *vault.Tx) error {
		return t.Put(vault.Meta, "migrated_at", now.Format(time.RFC3339))
	}); err != nil {
		report(err)
	}
	if err := vault.View(func(t *vault.Tx) error {
		m.VaultID, _ = t.Get(vault.Meta, "vault_id")
		m.Kid, _ = t.Get(vault.Meta, "kid")
		return nil
	}); err != nil {
		return errors.Join(append(failures, err)...)
	}
	m.MigratedAt = now
	m.Keyring = sortedNames(append(m.Keyring, keyringNames...))
	start := len(m.Items)
	for _, g := range committed {
		for _, item := range g.items {
			backup, err := migrationBackup(item.Source, now)
			if err != nil {
				return errors.Join(append(failures, err)...)
			}
			item.Backup, item.Names = backup, g.names
			m.Items = append(m.Items, item)
		}
	}
	if err := m.save(); err != nil {
		return errors.Join(append(failures, err)...)
	}
	for i := start; i < len(m.Items); i++ {
		if err := finishRename(m, i, false, false); err != nil {
			return errors.Join(append(failures, err)...)
		}
	}
	fmt.Fprintf(w, "renamed %d file(s) to .bak; run claude-code config finalize once verified\n", count)
	return errors.Join(failures...)
}

func parseMigration(sources []legacySource, m *Manifest) []*migrationGroup {
	groups := []*migrationGroup{
		{kind: "pins"}, {kind: "env"}, {kind: "providers"}, {kind: "vendors"}, {kind: "secrets"},
	}
	for _, source := range sources {
		kind := source.kind
		if kind == "secrets-meta" {
			kind = "secrets"
		}
		for _, g := range groups {
			if g.kind != kind {
				continue
			}
			item := Item{Source: source.path, Kind: source.kind}
			for _, old := range m.Items {
				if old.Source == source.path {
					g.err = errors.Join(g.err, fmt.Errorf(
						"%s reappeared after migration; run config finalize or rollback first", source.path))
				}
			}
			raw, err := os.ReadFile(source.path)
			if err == nil {
				if source.kind != "secrets" {
					sum := sha256.Sum256(raw)
					item.SHA256 = hex.EncodeToString(sum[:])
				}
				switch source.kind {
				case "pins", "env", "secrets":
					g.values, err = parseEnvFile(source.path)
					if err == nil && source.kind == "secrets" {
						for name, value := range g.values {
							if strings.ContainsAny(value, "\r\n") {
								err = fmt.Errorf("%s: %s: a secret must be one line", source.path, name)
								break
							}
						}
					}
				case "providers":
					g.doc = raw
					err = providers.Validate(raw)
					if len(g.items) != 0 {
						err = errors.Join(err, duplicateProvidersError())
					}
				case "vendors":
					g.doc = raw
					_, err = vendorconfig.Parse(nil, raw)
				case "secrets-meta":
					err = json.Unmarshal(raw, &g.meta)
				}
			}
			g.err = errors.Join(g.err, err)
			g.items = append(g.items, item)
		}
	}
	return groups
}

func migrateConfigGroup(w io.Writer, g *migrationGroup, dryRun bool) error {
	run := vault.Update
	if dryRun {
		run = vault.View
	}
	return run(func(t *vault.Tx) error {
		g.names = nil
		if g.kind == "providers" || g.kind == "vendors" {
			if _, ok := t.Get(vault.Overlay, g.kind); ok {
				fmt.Fprintf(w, "migrate: %s overlay already present in the vault; %s was not imported "+
					"(finalize will refuse unless you --discard %s)\n",
					g.kind, legacyLabel(g.items[0].Source), g.kind)
				return nil
			}
			g.names = append(g.names, g.kind)
			if dryRun {
				return nil
			}
			return t.Put(vault.Overlay, g.kind, string(g.doc))
		}
		bucket := vault.Pins
		if g.kind == "env" {
			bucket = vault.Env
		}
		for _, name := range mapNames(g.values) {
			if _, ok := t.Get(bucket, name); ok {
				continue
			}
			value := g.values[name]
			if g.kind == "env" && value == "@keyring" {
				value = providers.SecretRef
			}
			if !dryRun {
				if err := t.Put(bucket, name, value); err != nil {
					return err
				}
			}
			g.names = append(g.names, name)
		}
		return nil
	})
}

func migrateSecrets(g *migrationGroup, groups []*migrationGroup, m *Manifest, dryRun bool) ([]string, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	names := mapNames(g.values)
	for name := range g.meta.Credentials {
		names = append(names, name)
	}
	// Earlier groups may already be backed up when a failed secrets group is retried.
	for _, item := range m.Items {
		if item.Kind != "env" {
			continue
		}
		path := item.Backup
		if !item.Done {
			if _, err := os.Lstat(item.Source); err == nil {
				path = item.Source
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
		values, err := parseEnvFile(path)
		if err != nil {
			return nil, err
		}
		for name, value := range values {
			if value == "@keyring" {
				names = append(names, name)
			}
		}
	}
	for _, other := range groups {
		if other.err != nil {
			continue
		}
		if other.kind == "env" {
			for name, value := range other.values {
				if value == "@keyring" {
					names = append(names, name)
				}
			}
		}
		if dryRun && other.kind == "providers" && len(other.names) != 0 {
			var overlay providers.Registry
			if err := json.Unmarshal(other.doc, &overlay); err != nil {
				return nil, err
			}
			for name, p := range overlay.Providers {
				if p == nil {
					delete(reg.Providers, name)
				} else {
					reg.Providers[name] = p
				}
			}
		}
	}
	for _, p := range reg.Providers {
		if p.Auth.Env != "" {
			names = append(names, p.Auth.Env)
		}
	}
	names = sortedNames(append(names, "CLAUDE_CODE_OAUTH_TOKEN"))
	existing := make(map[string]bool)
	if err := vault.View(func(t *vault.Tx) error {
		for _, name := range t.Names(vault.Secrets) {
			existing[name] = true
		}
		return nil
	}); err != nil {
		return nil, err
	}
	values := make(map[string]string)
	var keyringNames []string
	for _, name := range names {
		if existing[name] {
			continue
		}
		value, found, err := legacyKeyringLookup(name)
		if errors.Is(err, keyring.ErrNoAnswer) ||
			(errors.Is(err, keyring.ErrUnavailable) && legacyKeyringAvailable()) {
			return nil, errors.New("keyring did not answer; re-run claude-code config migrate")
		}
		if err != nil && !errors.Is(err, keyring.ErrNoTool) &&
			!(errors.Is(err, keyring.ErrUnavailable) && !legacyKeyringAvailable()) {
			return nil, fmt.Errorf("keyring lookup %s failed", name)
		}
		if found && err == nil {
			keyringNames = append(keyringNames, name)
		} else {
			value, found = g.values[name]
		}
		if !found {
			continue
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("%s: a secret must be one line", name)
		}
		values[name] = value
	}
	g.names = mapNames(values)
	if !dryRun && len(values) != 0 {
		if err := vault.UpdateSealed(func(t *vault.Tx, k *vault.Key) error {
			g.names = nil
			for _, name := range mapNames(values) {
				if _, found, err := t.Record(name); err != nil {
					return err
				} else if found {
					continue
				}
				value := values[name]
				meta, ok := g.meta.Credentials[name]
				if !ok {
					meta.SetAt = time.Now()
				}
				if err := t.PutRecord(name, vault.Record{
					Sealed: k.Seal(name, value), Kid: k.Kid,
					Prefix: secrets.MetaPrefix(value), Len: len(value),
					SetAt: meta.SetAt, VerifiedAt: meta.VerifiedAt, VerifyStatus: meta.VerifyStatus,
				}); err != nil {
					return err
				}
				g.names = append(g.names, name)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return keyringNames, nil
}

func mapNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedNames(names []string) []string {
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		if len(out) == 0 || out[len(out)-1] != name {
			out = append(out, name)
		}
	}
	return out
}

func migrationBackup(source string, at time.Time) (string, error) {
	path := source + ".bak"
	for {
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			return path, nil
		} else if err != nil {
			return "", err
		}
		path = source + ".bak-" + at.Format("20060102-150405")
		at = at.Add(time.Second)
	}
}

func resumeMigration(w io.Writer, m *Manifest, dryRun bool) error {
	count := 0
	for i := range m.Items {
		if m.Items[i].Done {
			continue
		}
		if err := finishRename(m, i, dryRun, true); err != nil {
			return err
		}
		count++
	}
	if count > 0 {
		if dryRun {
			fmt.Fprintf(w, "would resume %d rename(s)\n", count)
		} else {
			fmt.Fprintf(w, "resumed %d rename(s)\n", count)
		}
	}
	return nil
}

// verifyResumeSource guards a pending migrate-side rename: a crash can land between the manifest
// save and finishRename's os.Rename, and an old binary still on disk may update the source in that
// window. Resuming must not rename (and thus silently "finish migrating") content that was never
// actually imported.
func verifyResumeSource(item Item) error {
	changed := fmt.Errorf("%s: source changed since migration; re-run after rollback", item.Source)
	if item.Kind != "secrets" {
		raw, err := os.ReadFile(item.Source)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != item.SHA256 {
			return changed
		}
		return nil
	}
	values, err := parseEnvFile(item.Source)
	if err != nil {
		return changed
	}
	for name, value := range values {
		// A keyring value took precedence over the file at seal time (migrateSecrets); match that.
		expect := value
		if kv, found, err := legacyKeyringLookup(name); err == nil && found {
			expect = kv
		}
		stored, found, err := vault.ReadSecret(name)
		if err != nil {
			return err
		}
		if !found || stored != expect {
			return changed
		}
	}
	return nil
}

// finishRename completes a pending source->backup rename; verify is true only when resuming a
// prior incomplete migration, where the source may have been touched again since it was recorded.
func finishRename(m *Manifest, i int, dryRun, verify bool) error {
	item := &m.Items[i]
	_, sourceErr := os.Lstat(item.Source)
	_, backupErr := os.Lstat(item.Backup)
	if sourceErr != nil && !errors.Is(sourceErr, fs.ErrNotExist) {
		return sourceErr
	}
	if backupErr != nil && !errors.Is(backupErr, fs.ErrNotExist) {
		return backupErr
	}
	switch {
	case sourceErr == nil && errors.Is(backupErr, fs.ErrNotExist):
		if verify {
			if err := verifyResumeSource(*item); err != nil {
				return err
			}
		}
		if !dryRun {
			if err := os.Rename(item.Source, item.Backup); err != nil {
				return err
			}
		}
	case errors.Is(sourceErr, fs.ErrNotExist) && backupErr == nil:
	default:
		return fmt.Errorf("cannot resume %s: need exactly one of source or backup", item.Source)
	}
	if dryRun {
		return nil
	}
	item.Done = true
	return m.save()
}
