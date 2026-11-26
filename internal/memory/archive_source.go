package memory

import "regexp"

const (
	archiveSrc   = `(activeContext|progress|decisionLog|conventions|sessionHistory)`
	archiveMonth = `\d{4}-(?:0[1-9]|1[0-2])`
	archiveDate  = archiveMonth + `-(?:0[1-9]|[12]\d|3[01])`
	archiveSlug  = `[A-Za-z0-9][A-Za-z0-9_-]*`
	archiveWord  = `[A-Za-z][A-Za-z0-9_-]*`
)

// archiveNameRE matches src-first, src-archive and date-first overflow names; one source group is set.
var archiveNameRE = regexp.MustCompile(`^(?:` + archiveSrc + `-` + archiveMonth + `(?:-\d{2}-` + archiveSlug + `|-` + archiveWord + `)?|` +
	archiveSrc + `-archive|` +
	archiveDate + `-` + archiveSrc + `-overflow(?:-` + archiveSlug + `)?)\.md$`)

// ArchiveSource maps an archive file name to its bank source; ok is false for any other name.
func ArchiveSource(name string) (src string, ok bool) {
	m := archiveNameRE.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	for _, g := range m[1:] {
		if g != "" {
			return g, true
		}
	}
	return "", false
}
