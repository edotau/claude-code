# Go CLIs

Stdlib `flag` first, cobra when the command tree earns it, viper rarely.

## Shape: a testable `run`

- `main` builds the signal context, calls `run`, `os.Exit`s its code. Nothing else calls `os.Exit` — it skips every
  deferred close, flush and temp-file cleanup.
- `run` takes args, streams, and (when used) env and stdin, and returns an `int`: the exit code is part of the
  contract, and only this layer knows it.

```go
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

## `flag` gotchas

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

## Subcommands

- One flag set per verb, chosen by `args[0]`, from a table so the help listing and the dispatcher can't drift.
- Global flags go on a root flag set parsed before the verb. Avoid flags valid in both positions — users can't tell
  which one won. Past two levels of nesting, consider cobra.
- Requested help goes to stdout with exit 0; help printed because of a mistake goes to stderr with exit 2.

## Exit codes

| Code | Meaning | Typical source |
| --- | --- | --- |
| 0 | success, including requested `-h` | normal return |
| 1 | runtime failure | any error from the work |
| 2 | usage error | bad flag, wrong arg count, unknown verb |
| 3+ | tool-specific, documented outcomes | "diff found", "not logged in" (pick few, keep stable) |
| 130 / 143 | interrupted by SIGINT / SIGTERM | context cancelled by a signal |

- Map errors to codes in one place (`exitCode(err)`), with `errors.As` on a typed error carrying the code. Scripts
  branch on the code, so changing one is a breaking change. Error construction: [errors.md](errors.md).

## stdout, stderr, stdin

- stdout is data for the next program in a pipe; progress, warnings, logs, prompts and errors go to stderr. One stray
  log line on stdout corrupts `tool list --json | jq`.
- Color and spinners only when the destination is a terminal (`term.IsTerminal(int(f.Fd()))` or a `ModeCharDevice`
  check) and `NO_COLOR` is unset — decide per stream, not per platform.
- Never prompt when stdin is not a terminal — fail with a message naming the flag that supplies the answer.
- Check the `bufio.Writer` `Flush` error; a closed pipe (`| head`) shows up there.

## Signals and cancellation

- Call `stop()` from `signal.NotifyContext` once the context is done (or right after `run` returns) so a second
  Ctrl-C falls back to the default handler and kills a stuck process — users expect "press again to force".
- Give shutdown its own bounded context (`context.WithTimeout(context.Background(), …)`): the signal context is
  already cancelled. Goroutine lifetimes and `errgroup`: [concurrency.md](concurrency.md).
- Report an interrupt as 130/143, not 1, and print one line, not a stack of "context canceled" wrappers.

## Version

- `debug.ReadBuildInfo()` already gives the module version (`go install …@v1.2.3`) and `vcs.revision`/`vcs.modified`
  for local builds; read it before reaching for `-ldflags "-X main.version=…"`.
- `-X` only for values the toolchain can't know (release channel, CI build date), on a package-level `var string` —
  `-X` silently does nothing on a `const` or a misspelled path.

## Testing a CLI

- Call `run` with `bytes.Buffer`s; assert stdout is empty on error paths and stderr empty on success — that pins the
  stream discipline. Isolate with `t.Setenv` and `t.TempDir()`. General test shape: [testing.md](testing.md).

## cobra — when the tree earns it

Worth the dependency for a public tool with many verbs, deep nesting, generated shell completions, or GNU-style
flags (`-n`, `--name`, `-abc`). A flat verb list stays `flag`.

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
  state on the command, so a shared tree leaks values between tests. One fresh tree per test case, driven with
  `SetOut`/`SetErr`/`SetArgs`.

## viper — rarely

Worth it only when one key must resolve across flag, env, and a config file in several formats, for a tool you
ship to others. A single JSON or TOML file plus `os.LookupEnv` overrides is a struct and twenty lines of stdlib.

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
