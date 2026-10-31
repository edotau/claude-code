package proc

import (
	"os"
	"strings"
)

// OnWSL reports whether we're running under WSL: WSL_DISTRO_NAME is set or /proc/version names "microsoft".
func OnWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	b, err := os.ReadFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}
