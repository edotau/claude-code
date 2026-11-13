package paths

import "path/filepath"

// WinRootFrom is where r mounts C: (a custom automount root is honored), else /mnt/c.
func WinRootFrom(r *Resolver) string {
	if p, ok := r.Resolve(`C:\`); ok && p != "/" {
		return p
	}
	return "/mnt/c"
}

// WinPowerShell is Windows PowerShell under this machine's C: mount, for WSL when interop leaves it off PATH.
func WinPowerShell() string {
	return filepath.Join(WinRootFrom(Default()), "Windows", "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}
