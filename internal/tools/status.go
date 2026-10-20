package tools

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// Status is one tool's resolved state: path and version are "" when it isn't found on PATH.
type Status struct {
	Name      string
	Path      string
	Version   string // first line of `<path> --version`, best-effort
	LocalBin  bool   // Path lives under ~/.local/bin
	NPMGlobal bool   // Path resolves through an npm-global lib/node_modules install
	Target    string // os.Readlink of BinDir/<bin> for a vendor tool; "" when not a link
	Hint      string
}

// StatusAll reports every tool in roster order.
func StatusAll() []Status {
	var out []Status
	for _, t := range Tools {
		if t.Binary != "" { // a setup-only row (browser) has no binary to report; doctor has its own browser row
			out = append(out, statusOne(t))
		}
	}
	return out
}

func statusOne(t Tool) Status {
	s := Status{Name: t.Name, Hint: t.Hint}
	if bin, ok := paths.VendorName(t.Name); ok {
		s.Target = readlink(filepath.Join(paths.BinDir(), bin))
	}
	path, err := paths.LookPathReal(t.Binary)
	if err != nil {
		return s
	}
	s.Path = path
	s.LocalBin = strings.HasPrefix(path, localBinDir()+string(filepath.Separator))
	s.NPMGlobal = npmPrefixFromBinary(path) != ""
	s.Version = firstVersionLine(path)
	return s
}

// firstVersionLine runs `path --version` with a short timeout; errors are silently "" (best-effort).
func firstVersionLine(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}
