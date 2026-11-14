package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/paths"
)

// toolDoctorOK is false only for a bin/<agent> entry that exists but cannot run (broken link, retired shim).
func toolDoctorOK(s tools.Status) bool {
	bin, ok := paths.AgentName(s.Name)
	if !ok {
		return true
	}
	if _, err := os.Lstat(filepath.Join(paths.BinDir(), bin)); os.IsNotExist(err) {
		return true
	}
	_, err := paths.AgentBin(bin)
	return err == nil
}

// agentVersionDetail renders one agent's version row: ok <ver>, warn <ver> <problem>, or not installed.
func agentVersionDetail(spec agent.Schema) (pass bool, detail string) {
	v, err := tools.Version(spec.Name)
	switch {
	case errors.Is(err, tools.ErrAgentNotInstalled):
		return true, "not installed"
	case err != nil:
		return true, "version unknown (" + err.Error() + ")"
	}
	if p := tools.VersionProblem(spec.CLI, v); p != "" {
		return false, "warn " + v + " " + p
	}
	return true, "ok " + v
}

// toolDoctorDetail renders one tool's doctor line: an agent's bin/ entry, else path + version, then extra PATH copies.
func toolDoctorDetail(ctx context.Context, s tools.Status) string {
	if bin, ok := paths.AgentName(s.Name); ok {
		return agentDoctorDetail(ctx, s, bin)
	}
	if s.Path == "" {
		if s.Hint != "" {
			return "not found (" + s.Hint + ")"
		}
		return "not found (claude-code install --tools=" + s.Name + ")"
	}
	var detail strings.Builder
	detail.WriteString(s.Path)
	if s.Version != "" {
		detail.WriteString(" ")
		detail.WriteString(s.Version)
	}
	switch {
	case s.NPMGlobal:
		detail.WriteString(" (npm-global; claude-code install --tools retires this)")
	case s.LocalBin:
		detail.WriteString(" (~/.local/bin)")
	}
	detail.WriteString(extraPATHCopies(ctx, s.Name, s.Path))
	return detail.String()
}

// agentDoctorDetail renders BinDir/<bin>: `<dst> → <Target> <version>`, a direct install, or why it is unusable.
func agentDoctorDetail(ctx context.Context, s tools.Status, bin string) string {
	dst := filepath.Join(paths.BinDir(), bin)
	if _, err := paths.AgentBin(bin); err != nil {
		if s.Path != "" {
			return err.Error() + " · on PATH: " + s.Path
		}
		return err.Error()
	}
	var detail strings.Builder
	detail.WriteString(dst)
	if s.Target != "" {
		detail.WriteString(" → ")
		detail.WriteString(s.Target)
	}
	version := s.Version
	if s.Target == "" || s.Target != s.Path {
		version = toolPATHVersion(ctx, dst)
	}
	if version != "" {
		detail.WriteString(" ")
		detail.WriteString(version)
	}
	if s.Target == "" {
		detail.WriteString(" (direct install in bin/)")
	}
	if s.NPMGlobal && s.Target == s.Path {
		detail.WriteString(" (npm-global)")
	}
	detail.WriteString(extraPATHCopies(ctx, s.Name, dst))
	return detail.String()
}

// extraPATHCopies lists PATH copies of name that resolve somewhere other than primary, as " · also on PATH: …".
func extraPATHCopies(ctx context.Context, name, primary string) string {
	first, _ := filepath.Abs(paths.Canon(primary)) // a broken symlink compares by its own path
	var out strings.Builder
	for _, path := range toolPATHCopies(name) {
		if real, _ := filepath.Abs(paths.Canon(path)); real == first {
			continue
		}
		extra := path
		if version := toolPATHVersion(ctx, path); version != "" {
			extra += " " + version
		}
		out.WriteString(" · also on PATH: ")
		out.WriteString(extra)
	}
	return out.String()
}

// toolPATHCopies lists distinct executables named name on PATH, skipping BinDir and the harness binary.
func toolPATHCopies(name string) []string {
	binDir, _ := filepath.Abs(paths.BinDir())
	self, _ := paths.SelfBinary()
	seen := map[string]bool{}
	var copies []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if abs, _ := filepath.Abs(dir); abs == binDir {
			continue
		}
		candidate := filepath.Join(dir, name)
		real := paths.ExecutableRealPath(candidate)
		if real == "" || real == self || filepath.Base(real) == "claude-code" || seen[real] {
			continue
		}
		seen[real] = true
		copies = append(copies, candidate)
	}
	return copies
}

// pathCopyVersionTimeout bounds `--version` on a PATH copy, shorter than tools' probe so doctor stays quick.
const pathCopyVersionTimeout = 2 * time.Second

// toolPATHVersion is the first --version line of a PATH copy, cached by path+size+mtime like the tools version cache.
func toolPATHVersion(ctx context.Context, path string) string {
	return tools.ProbedVersionLine(path, func(p string) string {
		return tools.FirstVersionLine(ctx, p, pathCopyVersionTimeout)
	})
}
