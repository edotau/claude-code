//go:build !unix

package paths

// dirSync is a no-op where a directory cannot be fsynced (Windows).
func dirSync(string) error { return nil }
