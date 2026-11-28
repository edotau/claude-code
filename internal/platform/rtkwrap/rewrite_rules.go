// Ported from rtk v1.1.0 src/discover/rules.rs and registry.rs tables (Apache 2.0, github.com/rtk-ai/rtk).
package rtkwrap

import (
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
)

// pipeSafety is where in a pipeline a rule may rewrite its command (rtk #3171).
type pipeSafety uint8

const (
	pipeNone pipeSafety = iota
	pipeProducer
	pipeBoth
)

// rtkRule is one row: name is the rtk command a match becomes, pattern the condition, prefixes the words it replaces.
type rtkRule struct {
	name     string
	pattern  string
	safety   pipeSafety
	prefixes []string
}

// npmRunners is rtk's optional npm/pnpm/npx runner group in front of a JS tool.
const npmRunners = `^((p?np(m|x)|p?npm\s+(exec|run|run-script)|npm\s+(rum|urn|x)|pnpm\s+dlx)\s+)?`

// jsPrefixes is each runner spelling of tool, as rtk lists them for a JS tool rule.
func jsPrefixes(tool string, bare bool) []string {
	var out []string
	for _, r := range []string{"npm exec", "npm", "npm rum", "npm run", "npm run-script", "npm urn", "npm x", "npx",
		"pnpm dlx", "pnpm exec", "pnpm", "pnpm run", "pnpm run-script", "pnpx"} {
		out = append(out, r+" "+tool)
	}
	if bare {
		out = append(out, tool)
	}
	return out
}

// rtkRules is rtk's RULES table in its order: the last row whose pattern matches decides.
var rtkRules = []rtkRule{
	{"rtk git", `^(?:git|yadm)\s+(?:-[Cc]\s+\S+\s+)*(status|log|diff|show|add|commit|checkout|push|pull|branch|fetch|stash|worktree)(?:\s|$|[;|&()<>])`, pipeProducer, []string{"git", "yadm"}},
	{"rtk gh", `^gh\s+(pr|issue|run|repo|api|release)(?:\s|$|[;|&()<>])`, pipeNone, []string{"gh"}},
	{"rtk glab", `^glab\s+(mr|issue|ci|pipeline|api|release)(?:\s|$|[;|&()<>])`, pipeNone, []string{"glab"}},
	{"rtk cargo", `^cargo\s+(build|test|clippy|check|fmt|install)(?:\s|$|[;|&()<>])`, pipeProducer, []string{"cargo"}},
	{"rtk pnpm", `^pnpm\s+(exec|i|install|list|ls|outdated|run|run-script)`, pipeNone, []string{"pnpm"}},
	{"rtk npm", `^npm\s+(exec|run|run-script|rum|urn|x)(\s|$)`, pipeNone, []string{"npm"}},
	{"rtk npx", `^npx\s+`, pipeNone, []string{"npx"}},
	{"rtk read", `^(cat|head|tail)\s+`, pipeNone, []string{"cat", "head", "tail"}},
	{"rtk grep", `^grep\s+`, pipeBoth, []string{"grep"}},
	{"rtk rg", `^rg\s+`, pipeBoth, []string{"rg"}},
	{"rtk ast-grep", `^ast-grep\s+`, pipeProducer, []string{"ast-grep"}},
	{"rtk ls", `^ls(\s|$)`, pipeProducer, []string{"ls"}},
	{"rtk find", `^find\s+`, pipeProducer, []string{"find"}},
	{"rtk tsc", npmRunners + `tsc(\s|$)`, pipeNone, sortedPrefixes(jsPrefixes("tsc", true))},
	{"rtk lint", npmRunners + `(biome|eslint|lint)(\s|$)`, pipeProducer, lintPrefixes()},
	{"rtk prettier", npmRunners + `prettier(?:\s|$|[;|&()<>])`, pipeProducer, sortedPrefixes(jsPrefixes("prettier", true))},
	{"rtk ng", `^ng\s+build(?:\s|$|[;|&()<>])`, pipeNone, []string{"ng"}},
	{"rtk next", npmRunners + `next\s+build(?:\s|$|[;|&()<>])`, pipeProducer, sortedPrefixes(jsPrefixes("next build", true))},
	{"rtk jest", npmRunners + `jest(\s+run)?(\s|$)`, pipeNone, runPrefixes("jest")},
	{"rtk vitest", npmRunners + `vitest(\s+run)?(\s|$)`, pipeNone, runPrefixes("vitest")},
	{"rtk ctest", `^ctest(?:\s|$)`, pipeNone, []string{"ctest"}},
	{"rtk playwright", npmRunners + `playwright(?:\s|$|[;|&()<>])`, pipeNone, sortedPrefixes(jsPrefixes("playwright", true))},
	{"rtk prisma", npmRunners + `prisma(?:\s|$|[;|&()<>])`, pipeNone, sortedPrefixes(jsPrefixes("prisma", true))},
	{"rtk docker", `^docker\s+(ps|images|logs|run|exec|build|compose\s+(ps|logs|build))(?:\s|$|[;|&()<>])`, pipeNone, []string{"docker"}},
	{"rtk kubectl", `^kubectl\s+(get|logs|describe|apply)(?:\s|$|[;|&()<>])`, pipeNone, []string{"kubectl"}},
	{"rtk oc", `^oc\s+(get|logs|describe|apply|status|adm)(?:\s|$|[;|&()<>])`, pipeNone, []string{"oc"}},
	{"rtk tree", `^tree(\s|$)`, pipeProducer, []string{"tree"}},
	{"rtk diff", `^diff\s+`, pipeProducer, []string{"diff"}},
	{"rtk curl", `^curl\s+`, pipeNone, []string{"curl"}},
	{"rtk wget", `^wget\s+`, pipeNone, []string{"wget"}},
	{"rtk mypy", `^(python3?\s+-m\s+)?mypy(\s|$)`, pipeProducer, []string{"python3 -m mypy", "python -m mypy", "mypy"}},
	{"rtk ruff", `^ruff\s+(check|format)(?:\s|$|[;|&()<>])`, pipeProducer, []string{"ruff"}},
	{"rtk sqlfluff", `^sqlfluff\s+lint(?:\s|$|[;|&()<>])`, pipeNone, []string{"sqlfluff"}},
	{"rtk pytest", `^(python[0-9.]*\s+-m\s+)?pytest(\s|$)`, pipeProducer, []string{"python3 -m pytest", "python -m pytest", "pytest"}},
	{"rtk pip", `^(pip3?|uv\s+pip)\s+(list|outdated|install|show)(?:\s|$|[;|&()<>])`, pipeProducer, []string{"pip3", "pip", "uv pip"}},
	{"rtk uv", `^uv\s+run(?:\s|$)`, pipeNone, []string{"uv"}},
	{"rtk go", `^go\s+(test|build|vet)(?:\s|$|[;|&()<>])`, pipeProducer, []string{"go"}},
	{"rtk golangci-lint run", `^(?:golangci-lint|golangci)\s+(run)(?:\s|$)`, pipeProducer, []string{"golangci-lint run", "golangci run"}},
	{"rtk sbt", `^sbt\s+["']?(testOnly|testQuick|test|compile|run|clean|assembly|package)(?:[\s"']|$)`, pipeNone, []string{"sbt"}},
	{"rtk bundle", `^bundle\s+(install|update)\b`, pipeProducer, []string{"bundle"}},
	{"rtk rake", `^(?:bundle\s+exec\s+)?(?:bin/)?(?:rake|rails)\s+test(?:[\s:]|$|[;|&()<>])`, pipeProducer, []string{"bundle exec rails", "bundle exec rake", "bin/rails", "rails", "rake"}},
	{"rtk rspec", `^(?:bundle\s+exec\s+)?rspec(?:\s|$)`, pipeProducer, []string{"bundle exec rspec", "bin/rspec", "rspec"}},
	{"rtk rubocop", `^(?:bundle\s+exec\s+)?rubocop(?:\s|$)`, pipeProducer, []string{"bundle exec rubocop", "rubocop"}},
	{"rtk php", `^php\s+artisan(?:\s|$)`, pipeNone, []string{"php"}},
	{"rtk php", `^php\s+-l(?:\s|$)`, pipeNone, []string{"php"}},
	{"rtk phpt", `^php\s+run-tests\.php(?:\s|$)`, pipeNone, []string{"php run-tests.php"}},
	{"rtk phpunit", `^(?:php\s+)?(?:\./)?(?:(?:vendor/)?bin/)?phpunit(?:\s|$)`, pipeProducer, []string{"bin/phpunit", "phpunit"}},
	{"rtk phpstan", `^(?:php\s+)?(?:\./)?(?:(?:vendor/)?bin/)?phpstan\s+analy[sz]e\b`, pipeProducer, []string{"bin/phpstan", "phpstan"}},
	{"rtk pest", `^(?:\./)?(?:vendor/bin/)?pest(?:\s|$)`, pipeProducer, []string{"pest"}},
	{"rtk paratest", `^(?:\./)?(?:vendor/bin/)?paratest(?:\s|$)`, pipeProducer, []string{"paratest"}},
	{"rtk ecs", `^(?:\./)?(?:vendor/bin/)?ecs(?:\s|$)`, pipeProducer, []string{"ecs"}},
	{"rtk pint", `^(?:\./)?(?:vendor/bin/)?pint(?:\s|$)`, pipeProducer, []string{"pint"}},
	{"rtk aws", `^aws\s+`, pipeNone, []string{"aws"}},
	{"rtk psql", `^psql(\s|$)`, pipeNone, []string{"psql"}},
	{"rtk bun", `^bun\s+(install|add|remove|test|build|run|pm\s+ls|pm|x)\b`, pipeNone, []string{"bun"}},
	{"rtk bunx", `^bunx\s+`, pipeNone, []string{"bunx"}},
	{"rtk deno", `^deno\s+(test|lint|check|run|task|compile|install)\b`, pipeNone, []string{"deno"}},
	{"rtk ansible-playbook", `^ansible-playbook\b`, pipeNone, []string{"ansible-playbook"}},
	{"rtk brew", `^brew\s+(install|upgrade)\b`, pipeProducer, []string{"brew"}},
	{"rtk composer", `^composer\s+(install|update|require)\b`, pipeProducer, []string{"composer"}},
	{"rtk df", `^df(\s|$)`, pipeProducer, []string{"df"}},
	{"rtk dotnet", `^dotnet\s+(build|restore)\b`, pipeProducer, []string{"dotnet"}},
	{"rtk du", `^du\b`, pipeProducer, []string{"du"}},
	{"rtk fail2ban-client", `^fail2ban-client\b`, pipeNone, []string{"fail2ban-client"}},
	{"rtk gcloud", `^gcloud\b`, pipeNone, []string{"gcloud"}},
	{"rtk gradlew", `^(?:\./gradlew|gradlew\.bat|gradlew|gradle)(?:\s+(test|build|clean|assemble\w*|install\w*|check|lint\w*|dependencies))?(\s|$)`, pipeNone, []string{"./gradlew", "gradlew.bat", "gradlew", "gradle"}},
	{"rtk hadolint", `^hadolint\b`, pipeProducer, []string{"hadolint"}},
	{"rtk helm", `^helm\b`, pipeProducer, []string{"helm"}},
	{"rtk iptables", `^iptables\b`, pipeProducer, []string{"iptables"}},
	{"rtk make", `^make\b`, pipeNone, []string{"make"}},
	{"rtk markdownlint", `^markdownlint\b`, pipeProducer, []string{"markdownlint"}},
	{"rtk mix", `^mix\s+(compile|format)(\s|$)`, pipeProducer, []string{"mix"}},
	{"rtk mvn", `^(?:\./mvnw|mvnw\.cmd|mvnw|mvn)\b(?:\s+\S+)*?\s+(compile|test|integration-test|package|install|verify|deploy)\b`, pipeProducer, []string{"./mvnw", "mvnw.cmd", "mvnw", "mvn"}},
	{"rtk mvnd", `^(?:mvnd\.cmd|mvnd)\b(?:\s+\S+)*?\s+(compile|test|integration-test|package|install|verify|deploy)\b`, pipeNone, []string{"mvnd.cmd", "mvnd"}},
	{"rtk ping", `^ping\b`, pipeNone, []string{"ping"}},
	{"rtk pio", `^pio\s+run(?:\s|$|[;|&()<>])`, pipeProducer, []string{"pio"}},
	{"rtk poetry", `^poetry\s+(install|lock|update)\b`, pipeProducer, []string{"poetry"}},
	{"rtk pre-commit", `^pre-commit\b`, pipeProducer, []string{"pre-commit"}},
	{"rtk ps", `^ps(\s|$)`, pipeProducer, []string{"ps"}},
	{"rtk pulumi", `^pulumi\s+(preview|up|destroy|refresh|stack)(\s|$)`, pipeNone, []string{"pulumi"}},
	{"rtk quarto", `^quarto\s+render(?:\s|$|[;|&()<>])`, pipeProducer, []string{"quarto"}},
	{"rtk rsync", `^rsync\b`, pipeNone, []string{"rsync"}},
	{"rtk shellcheck", `^shellcheck\b`, pipeProducer, []string{"shellcheck"}},
	{"rtk shopify", `^shopify\s+theme\s+(push|pull)(?:\s|$|[;|&()<>])`, pipeProducer, []string{"shopify"}},
	{"rtk sops", `^sops\b`, pipeNone, []string{"sops"}},
	{"rtk swift", `^swift\s+(build|test)\b`, pipeProducer, []string{"swift"}},
	{"rtk systemctl", `^systemctl\s+status\b`, pipeProducer, []string{"systemctl"}},
	{"rtk terraform", `^terraform\s+plan(?:\s|$|[;|&()<>])`, pipeProducer, []string{"terraform"}},
	{"rtk tofu", `^tofu\s+(fmt|init|plan|validate)(\s|$)`, pipeProducer, []string{"tofu"}},
	{"rtk trunk", `^trunk\s+build(?:\s|$|[;|&()<>])`, pipeProducer, []string{"trunk"}},
	{"rtk uv", `^uv\s+(sync|pip\s+install)\b`, pipeNone, []string{"uv"}},
	{"rtk yamllint", `^yamllint\b`, pipeProducer, []string{"yamllint"}},
	{"rtk wc", `^wc(\s|$)`, pipeProducer, []string{"wc"}},
	{"rtk gt", `^gt\s+`, pipeNone, []string{"gt"}},
	{"rtk liquibase", `^liquibase(?:\s|$)`, pipeProducer, []string{"liquibase"}},
}

// sortedPrefixes orders prefixes as rules.rs lists them: alphabetical, which puts longer runner forms first.
func sortedPrefixes(p []string) []string {
	out := append([]string(nil), p...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// runPrefixes lists tool and `tool run` under every runner, each `run` form first, as the jest and vitest rows do.
func runPrefixes(tool string) []string {
	var out []string
	for _, p := range sortedPrefixes(jsPrefixes(tool, true)) {
		out = append(out, p+" run", p)
	}
	return out
}

// lintPrefixes is the lint row's prefix list: biome, eslint and lint under each runner rules.rs spells.
func lintPrefixes() []string {
	out := []string{"biome", "eslint", "lint"}
	for _, r := range []string{"npm", "npm exec", "npm rum", "npm run", "npm run-script", "npm urn", "npm x", "npx",
		"pnpm", "pnpm dlx", "pnpm exec", "pnpm run", "pnpm run-script", "pnpx"} {
		for _, t := range []string{"biome", "eslint", "lint"} {
			if lintSpelled(r, t) {
				out = append(out, r+" "+t)
			}
		}
	}
	return sortedPrefixes(out)
}

// lintSpelled drops the runner/tool pairs rules.rs leaves out of the lint row.
func lintSpelled(runner, tool string) bool {
	return tool != "lint" || !slices.Contains([]string{"npm exec", "npm x", "pnpm dlx", "pnpm exec"}, runner)
}

// tomlFilters are the built-in filters' match_command patterns rtk 1.1.0 embeds (src/filters/*.toml).
var tomlFilters = []string{
	`^(mvn\s+spring-boot:run|java\s+-jar\s+(?:\S*[/\\])?[^/\\\s]*(?i:spring)[^/\\\s]*\.jar|gradle\s+.*bootRun)`,
	`^(pnpm\s+)?nx\b`, `^ansible-playbook\b`, `^basedpyright\b`, `^biome\b`, `^brew\s+(install|upgrade)\b`,
	`^bundle\s+(install|update)\b`, `^composer\s+(install|update|require)\b`, `^df(\s|$)`, `^dotnet\s+build\b`,
	`^du\b`, `^fail2ban-client\b`, `^g(cc|\+\+)\b`, `^gcloud\b`, `^hadolint\b`, `^helm\b`, `^iptables\b`, `^jira\b`,
	`^jj\b`, `^jq\b`, `^just\b`, `^liquibase(?:\s|$)`, `^make\b`, `^markdownlint\b`,
	`^mise\s+(run|exec|install|upgrade)\b`, `^mix\s+compile(\s|$)`, `^mix\s+format(\s|$)`, `^ollama\s+run\b`,
	`^oxlint\b`, `^ping\b`, `^pio\s+run(\s|$|[;|&()<>])`, `^poetry\s+(install|lock|update)\b`, `^pre-commit\b`,
	`^ps(\s|$)`, `^pulumi\s+destroy(\s|$)`, `^pulumi\s+preview(\s|$)`, `^pulumi\s+refresh(\s|$)`,
	`^pulumi\s+stack(\s+(ls|output|history|select|init|rm|rename|tag|unselect|change-secrets-provider)\b|\s*$)`,
	`^pulumi\s+up(\s|$)`, `^quarto\s+render(\s|$|[;|&()<>])`, `^rsync\b`, `^shellcheck\b`,
	`^shopify\s+theme\s+(push|pull)(\s|$|[;|&()<>])`, `^skopeo\b`, `^sops\b`, `^ssh(?:\s|$)`, `^stat\b`,
	`^swift\s+build\b`, `^systemctl\s+status\b`, `^task\b`, `^terraform\s+plan(\s|$|[;|&()<>])`,
	`^tofu\s+fmt(\s|$)`, `^tofu\s+init(\s|$)`, `^tofu\s+plan(\s|$)`, `^tofu\s+validate(\s|$)`,
	`^trunk\s+build(\s|$|[;|&()<>])`, `^turbo\b`, `^ty\b`, `^uv\s+(sync|pip\s+install)\b`, `^xcodebuild\b`,
	`^yadm\b`, `^yamllint\b`,
}

// filterReserved are bases rtk refuses to route to a TOML filter (probed: `pnpm nx build` stays as typed).
var filterReserved = map[string]bool{"pnpm": true}

// rustSpace is Rust regex's Unicode \s, which Go's ASCII \s is narrower than.
const rustSpace = `\t\n\v\f\r\x{85}\p{Z}`

// rx compiles a Rust pattern for Go, giving \s, \S and \d their Unicode meaning; \b and \w stay ASCII (see fold).
func rx(p string) *regexp.Regexp {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' && i+1 < len(p):
			i++
			b.WriteString(rustEscape(p[i], inClass))
		case c == '[' && !inClass:
			inClass = true
			b.WriteByte(c)
		case c == ']' && inClass && p[i-1] != '[' && p[i-1] != '^':
			inClass = false
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return regexp.MustCompile(b.String())
}

// rustEscape is Go's spelling of the Rust escape \e, inside or outside a bracket class.
func rustEscape(e byte, inClass bool) string {
	switch {
	case e == 's' && inClass:
		return rustSpace
	case e == 's':
		return `[` + rustSpace + `]`
	case e == 'S' && !inClass:
		return `[^` + rustSpace + `]`
	case e == 'd':
		return `\p{Nd}`
	}
	return `\` + string(e)
}

func compileAll(ps []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(ps))
	for i, p := range ps {
		out[i] = rx(p)
	}
	return out
}

var ruleRx = sync.OnceValue(func() []*regexp.Regexp {
	ps := make([]string, len(rtkRules))
	for i, r := range rtkRules {
		ps[i] = r.pattern
	}
	return compileAll(ps)
})

var filterRx = sync.OnceValue(func() []*regexp.Regexp { return compileAll(tomlFilters) })

// envWord is one leading `env ` or VAR=value assignment with the blanks after it (rtk's ENV_PREFIX unit).
const envWord = `env\s+|[A-Z_][A-Z0-9_]*=(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s]*)\s+`

// prefixRx are rtk's anchored strip and line-range patterns, compiled once.
var prefixRx = sync.OnceValue(func() map[string]*regexp.Regexp {
	return map[string]*regexp.Regexp{
		"env":       rx(`^(?:` + envWord + `)+`),
		"git":       rx(`^(?:(?:-C\s+\S+|-c\s+\S+|--git-dir(?:=\S+|\s+\S+)|--work-tree(?:=\S+|\s+\S+)|--no-pager|--no-optional-locks|--bare|--literal-pathspecs)\s+)+`),
		"pnpm":      rx(`^(?:(?:-r|--recursive|-w|--workspace-root|--filter(?:=\S+|\s+\S+)|-F(?:=\S+|\s+\S+))\s+)+`),
		"lineCont":  regexp.MustCompile(`(?m)[ \t\x0B\x0C]*\\\r?\n[ \t\x0B\x0C]*`),
		"bashJoin":  regexp.MustCompile(`\\\r?\n`),
		"headBare":  rx(`^head\s+(\S+)$`),
		"headRange": rx(`^head\s+(?:-(\d+)|--lines=(\d+)|-n\s+(\d+)|--lines\s+(\d+))\s+(\S+)$`),
		"tailRange": rx(`^tail\s+(?:-(\d+)|-n\s+(\d+)|--lines=(\d+)|--lines\s+(\d+))\s+(\S+)$`),
	}
})

// ignoredPrefixes and ignoredExact are commands rtk classifies as ignored before any rule.
var ignoredPrefixes = []string{"cd ", "cd\t", "echo ", "printf ", "export ", "source ", "mkdir ", "rm ", "mv ", "cp ",
	"chmod ", "chown ", "touch ", "which ", "type ", "test ", "true", "false", "sleep ", "wait", "kill ", "set ",
	"unset ", "sort ", "uniq ", "tr ", "cut ", "awk ", "sed ", "python3 -c", "python -c", "node -e", "ruby -e", "rtk ",
	"pwd", "bash ", "sh ", "then\n", "then ", "else\n", "else ", "do\n", "do ", "for ", "while ", "if ", "case "}

var ignoredExact = []string{"cd", "echo", "true", "false", "wait", "pwd", "bash", "sh", "fi", "done"}

// phpTools are the Composer tools whose path normalization reads composer config; the port leaves them undecided.
var phpTools = []string{"phpunit", "phpstan", "ecs", "pest", "paratest", "pint"}

// transparentPrefix is a built-in wrapper rtk looks through; routable ones may fall through to their own rule.
type transparentPrefix struct {
	word     string
	routable bool
}

var transparentPrefixes = []transparentPrefix{
	{"uv run", true}, {"noglob", false}, {"command", false}, {"builtin", false}, {"exec", false}, {"nocorrect", false},
}

// processWrapper is a wrapper command's option grammar, read to find the command it runs.
type processWrapper struct {
	name                          string
	valueOpts, flagOpts, attached []string
	positionals                   int
	numeric                       bool
}

var processWrappers = []processWrapper{
	{"timeout", []string{"-s", "-k", "--signal", "--kill-after"}, []string{"--preserve-status", "--foreground", "-v", "--verbose"}, []string{"-s", "-k"}, 1, false},
	{"time", []string{"-f", "-o", "--format", "--output"}, []string{"-p", "-a", "-v", "--append", "--verbose", "--portability", "--quiet"}, []string{"-f", "-o"}, 0, false},
	{"nice", []string{"-n", "--adjustment"}, nil, []string{"-n"}, 0, true},
	{"nohup", nil, nil, nil, 0, false},
}

// safeConsumer is a display-only pipe stage; an unsafe flag (tail -f) makes it unsafe.
type safeConsumer struct {
	name       string
	unsafeLong []string
	unsafeChar string
}

var safeConsumers = []safeConsumer{{"cat", nil, ""}, {"head", nil, ""}, {"tail", []string{"--follow"}, "fF"}}

type class uint8

const (
	clsIgnored class = iota
	clsUnsupported
	clsSupported
)

// classify is rtk's classify_command: ignored, unsupported, or supported by rule idx (the last row that matches).
func (r *rewriter) classify(cmd string) (class, int) {
	t := strings.TrimSpace(cmd)
	if t == "" || slices.Contains(ignoredExact, t) || hasAny(t, ignoredPrefixes, strings.HasPrefix) {
		return clsIgnored, -1
	}
	_, clean := stripEnvPrefix(t)
	if clean == "" {
		return clsIgnored, -1
	}
	n := stripGitGlobal(stripAbsolutePath(clean))
	if isPHPTool(n) {
		r.unsure = true // Composer bin-dir normalization reads project config the port does not
		return clsIgnored, -1
	}
	n = r.adoptPnpm(stripGolangciGlobal(n))
	idx := r.lastRule(n)
	if catRedirect(n) || idx < 0 {
		return clsUnsupported, -1
	}
	r.unsure = r.unsure || slices.Contains(phpTools, strings.TrimPrefix(rtkRules[idx].name, "rtk "))
	return clsSupported, idx
}

// adoptPnpm drops pnpm's global flags only when the bare form still routes to the pnpm rule (rtk #3275).
func (r *rewriter) adoptPnpm(n string) string {
	s := stripPnpmGlobal(n)
	if i := r.lastRule(s); s != n && i >= 0 && rtkRules[i].name == "rtk pnpm" {
		return s
	}
	return n
}

func (r *rewriter) lastRule(n string) int {
	folded := r.fold(n)
	for i, re := range slices.Backward(ruleRx()) {
		if re.MatchString(folded) {
			return i
		}
	}
	return -1
}

// fold maps non-ASCII runes to ASCII of the same Rust class so Go's \b and \w agree; ſ and K (Kelvin) bail.
func (r *rewriter) fold(s string) string {
	if !hasNonASCII(s) {
		return s
	}
	return strings.Map(func(c rune) rune {
		switch {
		case c < 0x80:
			return c
		case c == 'ſ' || c == 'K':
			r.unsure = true
		case unicode.IsSpace(c):
			return ' '
		case rustWordRune(c):
			return 'a'
		}
		return '%'
	}, s)
}

func hasNonASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// rustWordRune is Rust regex's Unicode \w: Alphabetic, marks, decimal digits, connector punctuation, join controls.
func rustWordRune(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsMark(c) || unicode.In(c, unicode.Nl, unicode.Other_Alphabetic, unicode.Nd,
		unicode.Pc, unicode.Join_Control)
}

func stripGitGlobal(cmd string) string {
	after, ok := strings.CutPrefix(cmd, "git ")
	if !ok {
		return cmd
	}
	return "git " + strings.TrimSpace(cutMatch(prefixRx()["git"], after))
}

func stripPnpmGlobal(cmd string) string {
	after, ok := strings.CutPrefix(cmd, "pnpm ")
	if !ok {
		return cmd
	}
	return "pnpm " + strings.TrimSpace(cutMatch(prefixRx()["pnpm"], strings.TrimLeftFunc(after, unicode.IsSpace)))
}

// cutMatch is s without the leading match of the anchored re.
func cutMatch(re *regexp.Regexp, s string) string {
	if loc := re.FindStringIndex(s); loc != nil {
		return s[loc[1]:]
	}
	return s
}

func stripGolangciGlobal(cmd string) string {
	if _, run, ok := golangciParts(cmd); ok {
		return "golangci-lint " + run
	}
	return cmd
}

// golangciParts splits `golangci-lint [global flags] run …` into its global flags and the run segment.
func golangciParts(cmd string) (global, run string, ok bool) {
	words := coalesceWords(cmd, tokenize(cmd))
	if len(words) == 0 || words[0].raw != "golangci-lint" && words[0].raw != "golangci" {
		return "", "", false
	}
	for i := 1; i < len(words); i++ {
		w := words[i].raw
		if w == "run" {
			return strings.TrimSpace(cmd[words[min(1, i)].off:words[i].off]), strings.TrimSpace(cmd[words[i].off:]), true
		}
		if w == "--" || !strings.HasPrefix(w, "-") {
			return "", "", false
		}
		if golangciTakesValue(w) {
			i++
		}
	}
	return "", "", false
}

var golangciValueOpts = []string{"-c", "--color", "--config", "--cpu-profile-path", "--mem-profile-path", "--trace-path"}

func golangciTakesValue(arg string) bool {
	flag := arg
	if strings.HasPrefix(arg, "--") {
		flag, _, _ = strings.Cut(arg, "=")
	}
	return slices.Contains(golangciValueOpts, flag) && (!strings.HasPrefix(arg, "--") || !strings.Contains(arg, "="))
}

// catRedirect: cat/head/tail with a redirect operator word writes rather than reads (rtk #315).
func catRedirect(n string) bool {
	if !hasAny(n, []string{"cat ", "head ", "tail "}, strings.HasPrefix) {
		return false
	}
	return slices.ContainsFunc(strings.Fields(n)[1:], func(t string) bool { return strings.HasPrefix(t, ">") || t == "<" })
}

// isPHPTool reports a first word naming a Composer PHP tool by any path or extension.
func isPHPTool(n string) bool {
	first := strings.ReplaceAll(strings.FieldsFunc(n, unicode.IsSpace)[0], `\`, "/")
	first = first[strings.LastIndexByte(first, '/')+1:]
	if dot := strings.LastIndexByte(first, '.'); dot >= 0 && slices.Contains([]string{"bat", "cmd", "exe", "ps1"}, strings.ToLower(first[dot+1:])) {
		first = first[:dot]
	}
	return slices.Contains(phpTools, first)
}

// blockKeywords open or close a multi-line construct, so a block holding one passes through whole.
var blockKeywords = []string{"for", "while", "until", "if", "then", "else", "elif", "fi", "do", "done", "case", "esac",
	"select", "function", "coproc", "{", "}", "(", ")"}
