package claude

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
)

// hashedOverlayPath names the file by content so a later launch never rewrites one a live session loaded.
func hashedOverlayPath(path string, data []byte) string {
	base := strings.TrimSuffix(path, ".json")
	if i := strings.LastIndex(base, "-"); i >= 0 && len(base)-i-1 == 12 { // drop an earlier hash
		if _, err := strconv.ParseUint(base[i+1:], 16, 64); err == nil {
			base = base[:i]
		}
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s-%x.json", base, sum[:6])
}
