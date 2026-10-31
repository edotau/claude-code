package launch

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/edotau/claude-code/internal/agent"
)

var codexManagedPaths = func() []string {
	if runtime.GOOS == "windows" {
		root := os.Getenv("ProgramData")
		if root == "" {
			root = `C:\ProgramData`
		}
		return []string{filepath.Join(root, "OpenAI", "Codex", "requirements.toml")}
	}
	return []string{"/etc/codex/managed_config.toml", "/etc/codex/requirements.toml"}
}

// codexConfigChecks reports configuration sources without reading or printing their contents.
func codexConfigChecks() []Check {
	var checks []Check
	for _, path := range codexManagedPaths() {
		checks = append(checks, codexConfigCheck("codex machine config", path, true))
	}
	cfg, err := agent.CodexConfigPath("")
	if err != nil {
		return append(checks, Check{Name: "codex user config", Detail: "home unavailable; effective route unknown"})
	}
	return append(checks, codexConfigCheck("codex user config", cfg, false))
}

func codexConfigCheck(name, path string, managed bool) Check {
	row := Check{Name: name, OK: true, Detail: path + ": absent"}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return row
	}
	if err != nil || !info.Mode().IsRegular() {
		row.OK, row.Detail = false, path+": unavailable; effective route unknown"
		return row
	}
	if managed {
		row.Detail = path + ": present; machine policy may override CLI settings; effective route unverified"
	} else {
		row.Detail = path + ": present; CLI and machine settings may override it"
	}
	return row
}
