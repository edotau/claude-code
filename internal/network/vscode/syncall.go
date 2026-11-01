package vscode

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

// SyncOptions are `theme vscode`'s paths; an empty field resolves to the platform default.
type SyncOptions struct {
	Template, Dest, ExtensionsDir, KeybindingsTemplate, KeybindingsDest string
	Check                                                               bool // never write (--check and --dry-run)
}

// Outcome is one sync run's results: settings template, keybindings and extension link.
type Outcome struct {
	DestPath   string
	Result     Result
	KbDestPath string
	Skip       bool // keybindings were not synced; Note says why
	Note       string
	KbResult   Result
	ExtResult  ExtensionResult
}

// SyncAll merges the settings and keybindings templates and links the Agents extension.
func SyncAll(o SyncOptions) (Outcome, error) {
	out := Outcome{DestPath: o.Dest}
	if o.Dest == "" {
		p, err := paths.VSCodeSettings()
		if err != nil {
			return out, err
		}
		out.DestPath = p
	}
	var err error
	if out.Result, err = Sync(o.Template, out.DestPath, paths.ConfigDir(), paths.BinDir(), o.Check); err != nil {
		return out, err
	}
	if out.KbDestPath, out.Skip, out.Note, err = resolveKeybindingsDest(o.KeybindingsDest, o.Dest, out.DestPath); err != nil {
		return out, err
	}
	if !out.Skip {
		if out.KbResult, err = SyncKeybindings(o.KeybindingsTemplate, out.KbDestPath, o.Check); err != nil {
			return out, err
		}
	}
	extDir := o.ExtensionsDir
	if extDir == "" {
		if extDir, err = DefaultExtensionsDir(); err != nil {
			return out, err
		}
	}
	out.ExtResult, err = LinkExtension(paths.ConfigDir(), extDir, o.Check)
	return out, err
}

// resolveKeybindingsDest: explicit kbDest wins outright; an explicit dest with no kbDest uses the same
// directory as the settings file, never the platform's vscode-server Machine guess for an unrelated dest.
func resolveKeybindingsDest(kbDest, dest, destPath string) (path string, skip bool, note string, err error) {
	switch {
	case kbDest != "":
		return kbDest, false, "", nil
	case dest != "":
		return filepath.Join(filepath.Dir(destPath), "keybindings.json"), false, "", nil
	default:
		return DefaultKeybindingsDest()
	}
}
