// rotate.go — rotate or replace a credential that is already present: generate a fresh one via the
// vendor's own CLI where one exists (`runLogin`/`runMint`), otherwise paste, always probing a keyed
// replacement live before it overwrites the stored value, and writing back to the same plane it came from.
package configure

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
)

// planeOf reports which store currently answers name, and the forceFile a re-Store must pass to land
// back in the same plane; source "env" is a raw process var — nothing the harness manages to overwrite.
func planeOf(name string) (forceFile bool, source string) {
	_, source, _ = secrets.Get(name)
	return source == "file", source
}

// probeValue live-checks a candidate value for p without storing it first, via the env-var seam
// providers.Credential already checks ahead of any stored plane; the previous env is restored after.
func probeValue(p *providers.Provider, envVar, candidate string) (status, detail string) {
	prev, had := os.LookupEnv(envVar)
	os.Setenv(envVar, candidate)
	defer func() {
		if had {
			os.Setenv(envVar, prev)
		} else {
			os.Unsetenv(envVar)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return providers.ProbeCredential(ctx, p)
}

// replaceKey pastes a new value for kt's keyed credential, probes it live before it overwrites the
// stored one (a rejected/unreachable paste asks whether to keep the old one instead), and writes it
// back to the plane it came from.
func replaceKey(d *dialog, kt keyTarget) bool {
	if kt.envVar == "" {
		return false
	}
	forceFile, plane := planeOf(kt.envVar)
	if plane == "env" {
		d.printf("%s: set via the process environment, not a stored plane — edit it where it is exported\n", kt.envVar)
		return false
	}
	if u := consoleURLs[kt.envVar]; u != "" && confirm(d, "Open "+u+" in your browser to create the new key?") {
		openBrowser(u)
	}
	val, err := secretLine(d, "Paste the new value for "+kt.envVar+" (input hidden, Enter when done): ")
	if err != nil || strings.TrimSpace(val) == "" {
		d.printf("%s: no value entered\n", kt.envVar)
		return false
	}
	_, v := ParseAssignment(val)
	if kt.provider != nil {
		status, detail := probeValue(kt.provider, kt.envVar, v)
		if status == "rejected" || status == "unreachable" {
			d.printf("%s: new value %s (%s)\n", kt.envVar, status, detail)
			if confirm(d, "Keep the old key and discard this paste?") {
				return false
			}
		}
	}
	return storeValue(d, kt.envVar, v, forceFile)
}

// regenerate re-runs a vendor's own login or mint CLI to rotate a credential that is already present.
// A login just reports the outcome (the session lives in the vendor's own file, not a plane we store);
// a mint writes the printed token back to the plane the old one came from.
func regenerate(d *dialog, kt keyTarget, argv []string, mint bool) bool {
	if !mint {
		if err := runLogin(argv); err != nil {
			d.printf("%s: %v\n", kt.label, err)
			return false
		}
		d.printf("%-10s logged in again\n", kt.label)
		return true
	}
	tok := runMint(argv)
	if tok == "" {
		d.printf("%s: no token found in the output; try again or paste one\n", kt.envVar)
		return false
	}
	forceFile, plane := planeOf(kt.envVar)
	if plane == "env" {
		d.printf("%s: minted a new token, but %s is set via the process environment — update it there\n", kt.label, kt.envVar)
		return false
	}
	return storeValue(d, kt.envVar, tok, forceFile)
}

// rotateOptions lists how an already-present kt can be rotated: its own CLI first (login, then mint —
// the anthropic subscription row carries both, named explicitly), else a plain paste when it is a
// keyed credential with no generator.
func rotateOptions(kt keyTarget) []authOption {
	var opts []authOption
	if len(kt.loginArgv) > 0 {
		opts = append(opts, authOption{action: "Login", desc: strings.Join(kt.loginArgv, " ") + " · browser sign-in, refreshes the session", argv: kt.loginArgv})
	}
	if len(kt.mintArgv) > 0 {
		opts = append(opts, authOption{action: "OAuth token", desc: strings.Join(kt.mintArgv, " ") + " · mints a new token, rotates " + kt.envVar,
			mint: kt.mintArgv, env: kt.envVar})
	}
	if len(opts) == 0 && kt.envVar != "" && kt.provider != nil {
		opts = append(opts, authOption{action: "API key", desc: "paste a new " + kt.envVar + " · replaces or rotates the stored key", env: kt.envVar})
	}
	return opts
}

// runRotate offers kt's rotate options (generate first, else paste) and runs the chosen one; false
// means skipped, cancelled, or nothing to rotate.
func runRotate(d *dialog, kt keyTarget) bool {
	opts := rotateOptions(kt)
	if len(opts) == 0 {
		return false
	}
	chosen := 0
	if len(opts) > 1 {
		if isStdinTTY() && d.o.In == os.Stdin {
			rows := make([][2]string, len(opts))
			for i, o := range opts {
				rows[i] = [2]string{o.action, o.desc}
			}
			idx, err := tui.PickBack(kt.label+" — rotate which credential?", rows, 0)
			if err != nil || idx < 0 {
				return false
			}
			chosen = idx
		} else if !confirm(d, opts[0].label()+"?") {
			chosen = 1
		}
	}
	o := opts[chosen]
	switch {
	case len(o.argv) > 0:
		return regenerate(d, kt, o.argv, false)
	case len(o.mint) > 0:
		return regenerate(d, kt, o.mint, true)
	default:
		return replaceKey(d, kt)
	}
}

// ensureKey makes sure kt's keyed credential is usable before a sub-picker pins it: absent asks to add
// one, present defaults to keeping it and only replaces on an explicit no.
func ensureKey(d *dialog, kt keyTarget, present bool) bool {
	if !present {
		return resolveKey(d, kt)
	}
	if confirm(d, kt.label+" key present — keep it?") {
		return true
	}
	return replaceKey(d, kt)
}
