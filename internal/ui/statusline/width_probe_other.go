//go:build !linux && !darwin

package statusline

func ancestorTTYWidth() int { return 0 }
