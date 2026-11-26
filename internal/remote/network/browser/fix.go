package browser

import (
	"errors"
	"fmt"
)

// fixFor is the exact next step for a browser whose extension/host is not ready, "" when it is (or nothing helps).
func fixFor(b browser) string {
	if !b.Installed { // a browser you never installed needs no step; --setup names the install when asked for it
		return ""
	}
	if b.Family != familyChromium {
		return b.DisplayName + " has no Claude extension (WebDriver-based control is not built); use a Chromium browser."
	}
	switch {
	case b.Host == stateUnsupportedWSL:
		if b.Paths.CDP == stateReady {
			return ""
		}
		return cdpFix(b)
	case b.Paths.Extension == stateNoHost:
		return hostFix(b)
	case b.Paths.Extension == stateDisabled:
		return enableFix(b)
	case b.Paths.Extension == stateNoExtension:
		return installFix(b)
	}
	return ""
}

func hostFix(b browser) string {
	return fmt.Sprintf("Claude Code owns the native-messaging host for %s; "+
		"run `claude --chrome` (or reinstall Claude Code) to register it.", b.DisplayName)
}

func enableFix(b browser) string {
	return fmt.Sprintf("The extension is installed but disabled in %s — enable it from its extensions page.", b.DisplayName)
}

func installFix(b browser) string {
	return fmt.Sprintf("run `claude-code network browser --setup --browser %s` to open the extension install page in %s.",
		b.Name, b.DisplayName)
}

func cdpFix(b browser) string {
	return fmt.Sprintf("run `claude-code network browser --setup --browser %s` to start %s with CDP on localhost:%d "+
		"and register the chrome-devtools MCP (Claude in Chrome is unsupported from WSL).", b.Name, b.DisplayName, cdpPort)
}

// openCandidates: Linux commands opening url in s, best first: gtk-launch, the browser binary, xdg-open.
func openCandidates(env Env, s spec, url string) [][]string {
	var c [][]string
	if s.linuxDesktopFile != "" && env.has("gtk-launch") {
		c = append(c, []string{"gtk-launch", s.linuxDesktopFile, url})
	}
	if s.linuxBinary != "" && env.has(s.linuxBinary) {
		c = append(c, []string{s.linuxBinary, url})
	}
	return append(c, []string{"xdg-open", url})
}

// has reports whether name is on PATH, asked through the Run seam like isInstalled.
func (e Env) has(name string) bool {
	out, err := e.run("sh", "-c", "command -v "+name)
	return err == nil && out != ""
}

// runCandidates runs cands until one starts; gtk-launch and open are waited for, the rest start detached.
func runCandidates(env Env, cands [][]string) (string, error) {
	last := errors.New("no opener available")
	for _, c := range cands {
		var out string
		var err error
		if c[0] == "gtk-launch" || c[0] == "open" || env.Launch == nil {
			out, err = env.run(c[0], c[1:]...)
		} else {
			err = env.Launch(c[0], c[1:]...)
		}
		if err == nil {
			return out, nil
		}
		last = fmt.Errorf("%s: %w", c[0], err)
	}
	return "", last
}
