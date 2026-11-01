package bank

import "strings"

const truncatedNotice = "\n<!-- (surface truncated) -->"

// CapChars cuts s to at most max runes (marker included) at the last whole line.
func CapChars(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	room := max - len([]rune(truncatedNotice))
	if room <= 0 {
		return ""
	}
	cut := string(r[:room])
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + truncatedNotice
}
