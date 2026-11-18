package safety

import (
	"regexp"
	"sync"
)

const (
	exfilSink = `(curl|nc|ncat|wget)([^[:alnum:]_]|$)`
	// secretCmd prints a token; secretSrc is one in command position, tailed to its own pipeline stage (no `&&`/`||`).
	secretCmd = `(?:claude-code\s+vault\s+get|security\s+find-(?:generic|internet)-password\s[^|;&\n]*-[a-zA-Z]*w|` +
		`gh\s+auth\s+token|databricks\s+auth\s+token|oc\s+whoami\s+-t)`
	stageTail = `(?:[^|;&\n]|>&|&>)*`
	secretSrc = cmdStart + `(?:\S*/)?(?:` + secretCmd + stageTail +
		`|(?:env(?:\s+-\S+)*|printenv(?:\s+\w+)*)\s*)`
	secretSink = `\|&?[ \t]*(?:xargs\s+(?:-[^Ii\s]\S*\s+)*|env\s+|command\s+|\\)?(?:\S*/)?` + exfilSink
	secretPipe = secretSrc + `(?:\|&?[ \t]*[^|;&\n \t]` + stageTail + `)*` + secretSink
)

var secretPiped = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(secretPipe) })
