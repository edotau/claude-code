package memory

import "regexp"

const (
	archiveSrc   = `(activeContext|progress|decisionLog|conventions|sessionHistory)`
	archiveMonth = `\d{4}-(?:0[1-9]|1[0-2])`
	archiveDate  = archiveMonth + `-(?:0[1-9]|[12]\d|3[01])`
	archiveSlug  = `[A-Za-z0-9][A-Za-z0-9_-]*`
)

// archiveNameRE matches src-first names (group 1) and date-first overflow names (group 2).
var archiveNameRE = regexp.MustCompile(`^(?:` + archiveSrc + `-` + archiveMonth + `(?:-\d{2}-` + archiveSlug + `)?|` +
	archiveDate + `-` + archiveSrc + `-overflow(?:-` + archiveSlug + `)?)\.md$`)

// archiveSource maps an archive file name to its bank source; ok is false for any other name.
func archiveSource(name string) (src string, ok bool) {
	m := archiveNameRE.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	if m[1] != "" {
		return m[1], true
	}
	return m[2], true
}
