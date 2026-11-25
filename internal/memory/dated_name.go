package memory

import (
	"regexp"
	"sync"
	"time"
)

const dateLayout = "2006-01-02"

var (
	repoKeyRE  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*--`) })
	dashDateRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\d{4}-\d{2}-\d{2}`) })
	compactRE  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})-`) })
)

func validDate(s string) bool {
	_, err := time.Parse(dateLayout, s)
	return err == nil
}

// StripRepoPrefix drops every leading `<key>--` and returns the first key; a name with none comes back whole.
func StripRepoPrefix(name string) (rest, key string) {
	rest = name
	for {
		k := repoKeyRE().FindString(rest)
		if k == "" || len(k) == len(rest) {
			return rest, key
		}
		if key == "" {
			key = k[:len(k)-2]
		}
		rest = rest[len(k):]
	}
}

// DatedName strips the repo prefix and leads the name with a date: its own, one inside it, else now.
func DatedName(name string, now time.Time) string {
	rest, _ := StripRepoPrefix(name)
	if len(rest) > 10 && rest[10] == '-' && validDate(rest[:10]) {
		return rest
	}
	if m := compactRE().FindStringSubmatch(rest); m != nil && validDate(m[1]+"-"+m[2]+"-"+m[3]) {
		return m[1] + "-" + m[2] + "-" + m[3] + rest[8:]
	}
	for _, d := range dashDateRE().FindAllString(rest, -1) {
		if validDate(d) {
			return d + "-" + rest
		}
	}
	return now.Format(dateLayout) + "-" + rest
}
