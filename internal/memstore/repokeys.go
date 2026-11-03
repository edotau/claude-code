package memstore

// RepoKeys lists the catalog's repo keys (dirs under memory/ with a valid origin record).
func RepoKeys() []string {
	ents, err := RepoDirs()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Key)
	}
	return out
}
