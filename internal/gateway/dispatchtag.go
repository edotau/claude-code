package gateway

// DispatchPrefix opens a router path that tags the request with its dispatch id: /d/<id>/…, composable with /via/.
const DispatchPrefix = "/d/"

// DispatchBase is base plus /d/<id> for dispatch id; an id the proxy would refuse is left off.
func DispatchBase(base, id string) string {
	if !ValidDispatchID(id) {
		return base
	}
	return base + DispatchPrefix + id
}

const maxDispatchIDLen = 128

// ValidDispatchID reports whether id is a dispatch id the proxy may log: 1-128 of digits, letters, '.' and '-'.
func ValidDispatchID(id string) bool {
	if id == "" || len(id) > maxDispatchIDLen {
		return false
	}
	for _, r := range id {
		if !dispatchIDRune(r) {
			return false
		}
	}
	return true
}

func dispatchIDRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '-'
}
