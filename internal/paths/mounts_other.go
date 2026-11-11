//go:build !linux && !darwin

package paths

func readMounts() []Mount { return nil }
