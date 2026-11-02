package menu

import (
	"github.com/edotau/claude-code/internal/configure"
	"regexp"

	"github.com/edotau/claude-code/internal/ui/tui"
)

// pickKey is the terminal picker seam; tests replace it.
var pickKey = tui.PickBack

// keysMenu lists every credential with its status; picking one opens its Login / OAuth token / API key actions
// (or the set-up flow when absent), then returns to the list. Back leaves. Nothing changes unless a row is picked.
func keysMenu(d *dialog) (allOK, changed bool) {
	start := 0
	for {
		targets, err := configure.KeyTargets(d.reg, d.o.Agent)
		if err != nil {
			d.println(err)
			return false, changed
		}
		rows, ok := keyRows(targets)
		idx, err := pickKey("keys — pick a credential to change, or Back to leave", rows, start)
		if err != nil || idx < 0 {
			return ok, changed
		}
		start = idx
		if changeKey(d, targets[idx]) {
			changed = true
		}
	}
}

// keyRows renders one picker row per credential (name, then mode and check aligned) and reports whether every
// credential is present and accepted.
func keyRows(targets []configure.KeyTarget) ([][2]string, bool) {
	names := make([]string, len(targets))
	cols := make([][2]string, len(targets))
	allOK := true
	for i, kt := range targets {
		names[i] = kt.Label
		cols[i] = keyCols(kt)
		if cols[i][1] != "verified" {
			allOK = false
		}
	}
	return WithChecks(names, cols), allOK
}

// keyCols is one credential's mode (how it signs in · how it bills) and check: verified, unverified, rejected or
// missing. A login counts as verified when its own CLI's status check confirmed it.
func keyCols(kt configure.KeyTarget) [2]string {
	if !kt.Present {
		return [2]string{"not set up", "not verified"}
	}
	if kt.Provider != nil && kt.EnvVar != "" {
		status, _ := configure.Probe(kt)
		if status == "verified" {
			return [2]string{modeAPIKey, "verified"}
		}
		if status == "login" {
			return [2]string{loginLine(kt), "verified"}
		}
		return [2]string{modeAPIKey, "not verified"}
	}
	return [2]string{loginLine(kt), "verified"}
}

var tierRE = regexp.MustCompile(`\((\w+)\)`)

// loginLine is a login credential's mode: "subscription", with the tier when the CLI reports one ("subscription (max)").
func loginLine(kt configure.KeyTarget) string {
	if kt.Label == "claude" {
		if m := tierRE.FindStringSubmatch(kt.Source); m != nil {
			return "subscription (" + m[1] + ")"
		}
	}
	return "subscription"
}

// changeKey runs the picked credential's actions: rotate when present, set up when absent.
func changeKey(d *dialog, kt configure.KeyTarget) bool {
	if !kt.Present {
		if !actionConfirmed("Set up " + kt.Label) {
			return false
		}
		return resolveKey(d, kt)
	}
	if len(configure.RotateOptions(kt)) == 0 {
		d.printf("%-10s nothing to rotate here: %s\n", kt.Label, kt.Source)
		return false
	}
	return runRotate(d, kt)
}
