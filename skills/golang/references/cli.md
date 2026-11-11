# Go CLIs

Building command-line tools in Go: stdlib `flag` first, cobra when the command tree earns it, viper rarely.

## House rules (this harness)

- Layout and ownership: the `cmd/claude-code/`, `internal/` row in [CLAUDE.md](../../../CLAUDE.md) — stdlib-first;
  `internal/cli/<verb>.go` holds flags and output only, the verb's logic lives in its area's package.
- Help text, row width and the `helptext.Page` contract: [cli-text.md](../../../rules/software/go/cli-text.md).
- Test file placement and seams: [testing.md](../../../rules/software/go/testing.md). This file never overrides them.

## Shape: a testable `run`

- `main` is three lines: build the context, call `run`, `os.Exit` its code. Nothing else calls `os.Exit` — it skips
  every deferred close, flush and temp-file cleanup.
- `run` takes everything the process would otherwise grab globally: args, streams, and (when used) env and stdin.
  A test then drives the real entry point with buffers instead of forking the binary.
- Return an `int`, not an `error`, at the top: the exit code is part of the contract, and only this layer knows it.

```go
func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
    stop()
    os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
    fs := flag.NewFlagSet("tool", flag.ContinueOnError)
    fs.SetOutput(stderr)
    jsonOut := fs.Bool("json", false, "print JSON")
    if err := fs.Parse(args); err != nil {
        if errors.Is(err, flag.ErrHelp) {
            return 0
        }
        return 2 // flag already printed the message and usage to stderr
    }
    if err := work(ctx, fs.Args(), *jsonOut, stdout); err != nil {
        fmt.Fprintf(stderr, "tool: %v\n", err)
        return exitCode(err)
    }
    return 0
}
```

## `flag` essentials

- Always `flag.NewFlagSet(name, flag.ContinueOnError)`; the global `flag.CommandLine` uses `ExitOnError` and
  shares state across tests.
- Parsing stops at the first non-flag argument: `tool file -v` leaves `-v` in `fs.Args()`. Document "flags first" or
  re-parse the remainder; don't silently accept a misplaced flag as a positional.
- Booleans take `-v` or `-v=false`, never `-v false` (that makes `false` a positional).
- `-flag` and `--flag` are identical; there are no short/long pairs or bundling (`-abc`). Register both names on one
  variable if you want an alias, and print it once in help.
- Distinguish "set to the zero value" from "not passed" with `fs.Visit` (visits only flags that were set).
- Custom types: `fs.Func` for one-off parsers, `fs.TextVar` for anything implementing `encoding.TextUnmarshaler`
  (`netip.Addr`, `slog.Level`), a `flag.Value` for repeatable flags (`-tag a -tag b` appends in `Set`).
- Validate positionals and cross-flag constraints right after `Parse`, and return 2 for a usage error.

## Subcommand dispatch

One flag set per verb, chosen by `args[0]`; a table keeps the help listing and the dispatcher from drifting.

```go
type verb struct {
    name, summary string
    run           func(ctx context.Context, args []string, stdout, stderr io.Writer) int
}

func dispatch(ctx context.Context, verbs []verb, args []string, stdout, stderr io.Writer) int {
    if len(args) == 0 || args[0] == "-h" || args[0] == "help" {
        return usage(verbs, stdout) // asked-for help: stdout, exit 0
    }
    for _, v := range verbs {
        if v.name == args[0] {
            return v.run(ctx, args[1:], stdout, stderr)
        }
    }
    fmt.Fprintf(stderr, "tool: unknown command %q (see `tool help`)\n", args[0])
    return 2
}
```

- Global flags go on a root flag set parsed before the verb; per-verb flags on the verb's own set. Avoid flags that
  are valid in both positions — users can't tell which one won.
- Nested verbs (`tool vault set`) are the same function one level down; past two levels, consider cobra.
- Requested help goes to stdout with exit 0; help printed because of a mistake goes to stderr with exit 2.

## Exit codes

| Code | Meaning | Typical source |
| --- | --- | --- |
| 0 | success, including requested `-h` | normal return |
| 1 | runtime failure | any error from the work |
| 2 | usage error | bad flag, wrong arg count, unknown verb |
| 3+ | tool-specific, documented outcomes | "diff found", "not logged in" (pick few, keep stable) |
| 130 / 143 | interrupted by SIGINT / SIGTERM | context cancelled by a signal |

- Map errors to codes in one place (`exitCode(err)`), with `errors.As` on a typed error carrying the code; never
  scatter literal codes through the work. Error construction: [errors.md](errors.md).
- Scripts branch on the code, so a code is an API: changing one is a breaking change.

## stdout, stderr, stdin

- stdout is the product: data the next program in a pipe reads. Everything else — progress, warnings, logs,
  prompts, errors — goes to stderr. One stray log line on stdout corrupts `tool list --json | jq`.
- Offer a machine format (`--json`) for anything a script will read, and keep it stable; the human format may change.
- Decide on color and spinners from the destination, not the platform: only when stderr/stdout is a terminal
  (`term.IsTerminal(int(f.Fd()))` or a `ModeCharDevice` check) and `NO_COLOR` is unset.
- Read `-` as stdin for file arguments; never prompt when stdin is not a terminal — fail with a message naming the
  flag that supplies the answer.
- Buffer large outputs with `bufio.Writer` and check the `Flush` error; a closed pipe (`| head`) shows up there.

## Signals and cancellation

- `signal.NotifyContext` in `main`, the context passed down to every blocking call; work that ignores `ctx` cannot
  be interrupted cleanly.
- Call `stop()` once the context is done (or right after `run` returns) so a second Ctrl-C falls back to the default
  handler and kills a stuck process — users expect "press again to force".
- Give shutdown its own bounded context (`context.WithTimeout(context.Background(), …)`): the signal context is
  already cancelled. Goroutine lifetimes and `errgroup`: [concurrency.md](concurrency.md).
- Report an interrupt as 130/143, not 1, and don't print a stack of "context canceled" wrappers — one line.

## Version

- `debug.ReadBuildInfo()` already gives the module version (`go install …@v1.2.3`) and `vcs.revision`/`vcs.modified`
  for local builds; read it before reaching for `-ldflags "-X main.version=…"`.
- Use `-X` only for values the toolchain can't know (a release channel, a build date from CI), on a package-level
  `var string` — `-X` silently does nothing on a `const` or a misspelled path.

## Testing a CLI

- Call `run` with `bytes.Buffer`s and assert on the exit code and both streams; assert stdout is empty on the error
  paths and stderr empty on the success path — that pins the stream discipline.
- Isolate the environment with `t.Setenv` and `t.TempDir()`; never let a test read the developer's real config.
- Golden files for long help or table output, with an `-update` flag to rewrite them deliberately.
- General Go test shape: [testing.md](testing.md).

## cobra — when the tree earns it

Worth the dependency for a public tool with many verbs, deep nesting, generated shell completions, or GNU-style
flags (`-n`, `--name`, `-abc`). Not worth it for an internal tool with a flat verb list — that stays `flag`.

- `RunE`, never `Run`: `Run` can't return an error, so failures end in `os.Exit` or a panic.
- Set `SilenceUsage` and `SilenceErrors` on the root; print the error and map the exit code yourself in `main`.
  Without them every runtime error dumps the usage wall.
- Pass the signal context with `root.ExecuteContext(ctx)` and read it via `cmd.Context()`.
- Write through `cmd.OutOrStdout()` / `cmd.ErrOrStderr()` so `SetOut`/`SetErr` can capture it.
- Declare arg rules in `Args` (`ExactArgs`, `RangeArgs`, `MatchAll(...)`), not as `len(args)` checks in `RunE`.
- A child's `PersistentPreRunE` replaces the parent's — it does not chain. Call the parent's explicitly if both
  must run. Hooks stop at the first error; `PostRunE` runs only after a successful `RunE`.
- Flag constraints are declarative: `MarkFlagRequired`, `MarkFlagsMutuallyExclusive`, `MarkFlagsOneRequired`,
  `MarkFlagsRequiredTogether`. `cmd.Flags().Changed(name)` answers "did the user pass it".
- `StringSlice` splits on commas (`--tag a,b` → two values); use `StringArray` when values may contain commas.
- Completions: `ValidArgs` for static lists, `ValidArgsFunction` / `RegisterFlagCompletionFunc` for dynamic ones;
  return `ShellCompDirectiveNoFileComp` when the list is exhaustive. Test them via the hidden `__complete` verb.
- Build the tree in a `newRootCmd()` constructor, not package-level `var`s wired in `init()`: cobra keeps parsed flag
  state on the command, so a shared tree leaks values between tests. One fresh tree per test case.

```go
func TestDeleteNeedsName(t *testing.T) {
    var out, errOut bytes.Buffer
    root := newRootCmd()
    root.SetOut(&out)
    root.SetErr(&errOut)
    root.SetArgs([]string{"delete"})
    if err := root.Execute(); err == nil {
        t.Fatal("want arg-count error")
    }
    if out.Len() != 0 {
        t.Fatalf("stdout = %q, want empty on error", out.String())
    }
}
```

## viper — rarely

Worth it only when one key must resolve across flag, env, and a config file in several formats, for a tool you
ship to others. A single JSON or TOML file plus `os.LookupEnv` overrides is a struct and twenty lines of stdlib —
prefer that, and keep config parsing in the area's package, not in the verb file.

- Precedence is fixed: `Set` > flag (only if `Changed`) > env > config file > remote KV > default.
- Env needs all three together: `SetEnvPrefix("TOOL")`, `SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))`,
  `AutomaticEnv()`. Without the replacer, nested keys look for `TOOL_DB.HOST` and never match.
- `Unmarshal` only sees keys viper already knows: an env-only value with no default, flag binding or file entry is
  invisible to it. `SetDefault` or `BindEnv` every key the struct reads.
- Tag every field `mapstructure:"…"`; durations and slices need decode hooks.
- A missing optional file is `viper.ConfigFileNotFoundError`, returned as a value: match it with
  `var nf viper.ConfigFileNotFoundError; errors.As(err, &nf)` — a `*ConfigFileNotFoundError` target never matches.
  Only the name-and-path search returns it; a missing explicit `SetConfigFile` path surfaces as an `fs` error.
- Use `viper.New()` and inject the instance; the package-level global leaks state across tests and goroutines.
- `WatchConfig` misses editors that save by atomic rename, and its callback runs on another goroutine: decode into a
  fresh struct, validate, then swap under a lock or `atomic.Pointer`.

Adapted from samber/cc-skills-golang (MIT): golang-cli, golang-spf13-cobra, golang-spf13-viper.
