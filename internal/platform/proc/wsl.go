package proc

import (
	"os"
	"strings"
)

// OnWSL reports a WSL guest: WSL's env vars, else a "microsoft" kernel in /proc/version (ssh sessions lack the vars).
func OnWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	b, err := os.ReadFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}
