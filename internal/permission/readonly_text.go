package permission

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/shell"
)

// literalReaders take only the listed separate flags and a bounded count of literal operands; no unquoted glob.
var literalReaders = map[string]func(args []string) bool{
	"jq": jqArgsOK,
	"uniq": func(a []string) bool { // a second operand is an output file
		ops, ok := literalArgs(a, "-c", "-d", "-u")
		return ok && len(ops) <= 1
	},
	"comm": func(a []string) bool { ops, ok := literalArgs(a, "-1", "-2", "-3"); return ok && len(ops) == 2 },
	"diff": func(a []string) bool { // no --output or -l/--paginate (runs pr); `-` spools stdin to a temp file
		ops, ok := literalArgs(a, "-u", "-q", "-s", "-w", "-b", "-B", "-i")
		return ok && len(ops) == 2 && !slices.Contains(ops, "-")
	},
	"stat": func(a []string) bool { // stat - names a file the operand check drops
		ops, ok := literalArgs(a)
		return ok && len(ops) > 0 && !slices.Contains(ops, "-")
	},
	"file": func(a []string) bool { ops, ok := literalArgs(a, "-b"); return ok && len(ops) > 0 }, // -C writes, -z runs
	"date": func(a []string) bool { // any other operand or -s may set the clock
		ops, ok := literalArgs(a, "-u")
		return ok && (len(ops) == 0 || len(ops) == 1 && strings.HasPrefix(ops[0], "+"))
	},
	"basename": func(a []string) bool { ops, ok := literalArgs(a); return ok && (len(ops) == 1 || len(ops) == 2) },
	"dirname":  func(a []string) bool { ops, ok := literalArgs(a); return ok && len(ops) == 1 },
	"sort": func(a []string) bool { // only the check: ordinary sorting spills temp files, -o writes
		ops, ok := literalArgs(a[min(1, len(a)):])
		return len(a) > 0 && a[0] == "-c" && ok && len(ops) <= 1
	},
}

// literalOrCLI judges a literal reader by its argument rule, anything else by the cluster and cloud CLI table.
func literalOrCLI(toks []string) bool {
	if argsOK := literalReaders[toks[0]]; argsOK != nil {
		return argsOK(toks[1:])
	}
	return readOnlyCLI(toks)
}

// expandsWords: seg expands a word, or a literal reader's seg holds an unquoted glob (its operands are counted).
func expandsWords(seg, name string) bool {
	return shell.Expands(seg) || braceExpands(seg) || literalReaders[name] != nil && unquotedAny(seg, "*?[")
}

// literalArgs are args' operands (`-` is one) when every word before the first is one of flags and none after it
// starts with `-`: BSD tools take a later dash word as an operand (`uniq f -c` writes the file -c).
func literalArgs(args []string, flags ...string) ([]string, bool) {
	i := 0
	for ; i < len(args) && args[i] != "-" && strings.HasPrefix(args[i], "-"); i++ {
		if !slices.Contains(flags, args[i]) {
			return nil, false
		}
	}
	ops := args[i:]
	return ops, !slices.ContainsFunc(ops, func(a string) bool { return a != "-" && strings.HasPrefix(a, "-") })
}

// jqArgsOK is jq with -c, -r, -s, -e, one filter and input files: no file, module or argument options.
func jqArgsOK(args []string) bool {
	ops, ok := literalArgs(args, "-c", "-r", "-s", "-e")
	return ok && len(ops) > 0 && jqFilterOK(ops[0]) && !jqStartupFile()
}

// jqStartupFile: jq sources ~/.jq into every filter when it is a file, and no jq option turns that off.
func jqStartupFile() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	fi, err := os.Stat(filepath.Join(home, ".jq"))
	return err == nil && !fi.IsDir() || err != nil && !errors.Is(err, fs.ErrNotExist)
}

var jqWord = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`) })

// jqFilterOK refuses a filter naming the environment (`env`, `$ENV`, `$ ENV`) or a module directive, even in a string.
func jqFilterOK(filter string) bool {
	return !slices.ContainsFunc(jqWord().FindAllString(filter, -1), func(w string) bool {
		return slices.Contains([]string{"env", "ENV", "import", "include", "modulemeta", "get_search_list"}, w)
	})
}

// literalReaderOperands are a literal reader's file operands; date, basename and dirname open none.
func literalReaderOperands(name string, files []string) []string {
	switch name {
	case "date", "basename", "dirname":
		return nil
	case "jq":
		return files[min(1, len(files)):] // files[0] is the filter
	}
	return files
}

// diffNotRegular: a shell-list diff has an operand that is not a regular file where it runs; diff walks a directory
// (past Read denies, through symlinks) and spools a device or fifo to a temp file.
func (p Permissions) diffNotRegular(seg string, at opDir) bool {
	toks, _ := shell.Unwrap(shell.Tokens(seg), "")
	if toks = rtkPlain(toks); len(toks) == 0 || toks[0] != "diff" || !slices.ContainsFunc(p.Allow, func(r Rule) bool {
		return r == ReadOnlyAllow || r == Rule{Tool: "Bash", Arg: VerifyShellArg}
	}) {
		return false
	}
	ops, _ := literalArgs(toks[1:], "-u", "-q", "-s", "-w", "-b", "-B", "-i")
	return slices.ContainsFunc(ops, func(o string) bool {
		if !filepath.IsAbs(o) {
			if at.lost {
				return true
			}
			o = filepath.Join(at.dir, o) // "" is the process cwd
		}
		fi, err := os.Stat(o)
		return err != nil || !fi.Mode().IsRegular()
	})
}
