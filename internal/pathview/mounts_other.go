//go:build !linux && !darwin

package pathview

func readMounts() []Mount { return nil }
