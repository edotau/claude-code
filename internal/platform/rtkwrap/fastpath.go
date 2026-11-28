package rtkwrap

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/shell"
)

// FastPath is `rtk rewrite`'s answer for cmd sans exec when certain: "" runs cmd as typed, else the rewrite.
func FastPath(cmd string) (rewritten string, decided bool) {
	if !MayRewrite(cmd) {
		return "", true
	}
	if os.Getenv(paths.RTKBinaryEnv) != "" { // a stand-in rtk (a wrapper, a test fake) answers for itself
		return "", false
	}
	if rtkGitCommits(cmd) { // the harness never wraps a commit, whatever rtk says
		return "", true
	}
	if !quietEnv() || !utf8.ValidString(cmd) || customFilters() {
		return "", false
	}
	excludes, ok := configExcludes()
	if !ok {
		return "", false
	}
	pats := make([]string, len(excludes))
	for i, ex := range excludes {
		pats[i] = strings.Join(ex, " ")
	}
	out, certain := rewriteCommand(cmd, pats)
	if out = strings.TrimSpace(out); out == cmd {
		out = ""
	}
	return out, certain
}

// customFilters reports a user or trusted project TOML filter file rtk may load, which the port does not read.
func customFilters() bool {
	home, _ := os.UserHomeDir()
	files := []string{filepath.Join(home, ".config", "rtk", "filters.toml"),
		filepath.Join(home, ".local", "share", "rtk", "trusted_filters.json")}
	if cfg := paths.RTKConfig(); cfg != "" {
		files = append(files, filepath.Join(filepath.Dir(cfg), "filters.toml"),
			filepath.Join(filepath.Dir(cfg), "trusted_filters.json"))
	}
	return home == "" || slices.ContainsFunc(files, func(f string) bool {
		_, err := os.Stat(f)
		return !errors.Is(err, fs.ErrNotExist)
	})
}

// rewriteWords are the words rtk rewrite may act on, loaded once from guards.json.
var rewriteWords = sync.OnceValue(func() map[string]bool { return shell.GuardSet("rtk_rewrite_words") })

// wordRun is one word of a command, and versioned a versioned spelling of one (gcc-13, python3.12).
var wordRun = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[\w.+-]+`) })
var versioned = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[-.]?[0-9][0-9.]*$`) })

// MayRewrite is false only when no word of cmd is one rtk rewrites, so skipping the exec loses nothing.
func MayRewrite(cmd string) bool {
	return slices.ContainsFunc(wordRun().FindAllString(cmd, -1), isRewriteWord)
}

func isRewriteWord(w string) bool {
	return rewriteWords()[w] || rewriteWords()[versioned().ReplaceAllString(w, "")]
}

// rtkReadEnv are the RTK_* names the rtk 1.1.0 binary reads (its strings, no dynamic names), bar the harness pair.
var rtkReadEnv = []string{"RTK_AUDIT_DIR", "RTK_CONFIG_DIR", "RTK_DB_PATH", "RTK_DISABLED", "RTK_HOOK_AUDIT",
	"RTK_MINOR", "RTK_NO_TOML", "RTK_ONRAMP_DISABLED", "RTK_PLUGIN_SOURCE", "RTK_POSTHOOK", "RTK_PROXY_BIND",
	"RTK_PROXY_CCR_STRUCT_MAX_LINES", "RTK_PROXY_DB", "RTK_PROXY_ICM_DB", "RTK_RECALL", "RTK_RECALL_DB",
	"RTK_REWRITE_HOST", "RTK_TEE", "RTK_TEE_DIR", "RTK_TOML_DEBUG", "RTK_TRUST_PROJECT_FILTERS"}

// quietEnv reports none of rtkReadEnv set: any of them may change what rtk rewrites.
func quietEnv() bool {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "RTK_") && slices.Contains(rtkReadEnv, name) {
			return false
		}
	}
	return true
}

// configExcludes is rtk's exclude_commands as word lists; ok false when the config is absent or holds anything else.
func configExcludes() (excludes [][]string, ok bool) {
	if runtime.GOOS == "linux" && os.Getenv("XDG_CONFIG_HOME") != "" {
		return nil, false // rtk may read another config than paths.RTKConfig
	}
	file := paths.RTKConfig()
	if file == "" {
		return nil, false
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	return parseExcludes(b)
}

// parseExcludes reads a config holding only [hooks] exclude_commands of plain word lists and [telemetry] enabled.
func parseExcludes(b []byte) (excludes [][]string, ok bool) {
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "[hooks]" || line == "[telemetry]" {
			section = line
			continue
		}
		list, good := configLine(section, line)
		if !good {
			return nil, false
		}
		excludes = append(excludes, list...)
	}
	return excludes, sc.Err() == nil
}

// configLine is the word lists one key line of section holds; ok false for any key the fast path does not know.
func configLine(section, line string) (lists [][]string, ok bool) {
	key, value, _ := strings.Cut(line, "=")
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	switch {
	case section == "[telemetry]" && key == "enabled" && (value == "true" || value == "false"):
		return nil, true
	case section == "[hooks]" && key == "exclude_commands":
		return parseWordLists(value)
	}
	return nil, false
}

// plainPattern is an exclude pattern rtk reads as literal words; a regex character or one word is not decided here.
var plainPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z0-9_-]+( [A-Za-z0-9_=-]+)+$`)
})

// parseWordLists parses a one-line TOML array of plain-word strings into word lists.
func parseWordLists(value string) ([][]string, bool) {
	inner, ok := strings.CutPrefix(value, "[")
	if !ok {
		return nil, false
	}
	if inner, ok = strings.CutSuffix(inner, "]"); !ok {
		return nil, false
	}
	var out [][]string
	for item := range strings.SplitSeq(inner, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		word, opened := strings.CutPrefix(item, `"`)
		word, closed := strings.CutSuffix(word, `"`)
		if !opened || !closed || !plainPattern().MatchString(word) {
			return nil, false
		}
		out = append(out, strings.Fields(word))
	}
	return out, true
}
