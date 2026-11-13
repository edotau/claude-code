# Go CLIs

Stdlib `flag` first, cobra when the command tree earns it, viper rarely.

## Shape: a testable `run`

- `main` builds the signal context, calls `run`, `os.Exit`s its code; nothing else calls `os.Exit` (skips defers).
- `run` takes args and streams (env/stdin when used) and returns the exit code.

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

- Always `flag.NewFlagSet(name, flag.ContinueOnError)`; global `flag.CommandLine` uses `ExitOnError` and shares state
  across tests.
- Parsing stops at the first non-flag: `tool file -v` leaves `-v` in `fs.Args()`. Document "flags first" or re-parse.
- Booleans take `-v` or `-v=false`, never `-v false` (`false` becomes a positional).
- `-flag` and `--flag` are identical; no short/long pairs or bundling. Alias: register both names on one variable.
- `fs.Visit` visits only flags that were set: tells "zero value" from "not passed".
- Custom types: `fs.Func` (one-off parser), `fs.TextVar` (`encoding.TextUnmarshaler`: `netip.Addr`, `slog.Level`),
  `flag.Value` (repeatable flags, append in `Set`).

## Subcommands

- One flag set per verb, chosen by `args[0]` from a table so help and dispatcher can't drift.
- Global flags on a root flag set parsed before the verb; avoid flags valid in both positions. Past two nesting
  levels, consider cobra.
- Requested help: stdout, exit 0. Help after a mistake: stderr, exit 2.

## Exit codes

| Code | Meaning | Typical source |
| --- | --- | --- |
| 0 | success, including requested `-h` | normal return |
| 1 | runtime failure | any error from the work |
| 2 | usage error | bad flag, wrong arg count, unknown verb |
| 3+ | tool-specific, documented outcomes | "diff found", "not logged in" (few, stable) |
| 130 / 143 | interrupted by SIGINT / SIGTERM | context cancelled by a signal |

- Map errors to codes in one place (`exitCode(err)`, `errors.As` on a typed error carrying the code); changing a code
  breaks scripts. Error construction: [errors.md](errors.md).

## stdout, stderr, stdin

- stdout is data for the next program; progress, warnings, logs, prompts and errors go to stderr.
- Color and spinners only when the stream is a terminal (`term.IsTerminal(int(f.Fd()))`) and `NO_COLOR` is unset.
- Never prompt when stdin is not a terminal; fail naming the flag that supplies the answer.
- Check the `bufio.Writer` `Flush` error; a closed pipe (`| head`) shows up there.

## Signals and cancellation

- Call `stop()` from `signal.NotifyContext` once the context is done so a second Ctrl-C kills a stuck process.
- Give shutdown its own `context.WithTimeout(context.Background(), …)`; the signal context is already cancelled.
  Goroutine lifetimes and `errgroup`: [concurrency.md](concurrency.md).
- Report an interrupt as 130/143 with one line, not a stack of "context canceled" wrappers.

## Version

- `debug.ReadBuildInfo()` gives the module version (`go install …@v1.2.3`) and `vcs.revision`/`vcs.modified`; try it
  before `-ldflags "-X main.version=…"`.
- `-X` only for values the toolchain can't know (release channel, CI date), on a package-level `var string`; it
  silently does nothing on a `const` or a misspelled path.

## Testing a CLI

- Call `run` with `bytes.Buffer`s; assert stdout empty on error paths, stderr empty on success. Isolate with
  `t.Setenv` and `t.TempDir()`. General shape: [testing.md](testing.md).

## cobra — when the tree earns it

Worth it for many verbs, deep nesting, generated completions, or GNU-style flags (`-n`, `--name`, `-abc`).

- `RunE`, never `Run`.
- `SilenceUsage` and `SilenceErrors` on the root; print the error and map the exit code in `main`.
- `root.ExecuteContext(ctx)` passes the signal context; read it via `cmd.Context()`.
- Write via `cmd.OutOrStdout()` / `cmd.ErrOrStderr()` so `SetOut`/`SetErr` capture it.
- Arg rules in `Args` (`ExactArgs`, `RangeArgs`, `MatchAll(...)`), not `len(args)` checks.
- A child's `PersistentPreRunE` replaces the parent's (no chaining); call the parent's explicitly. Hooks stop at the
  first error; `PostRunE` runs only after a successful `RunE`.
- Constraints: `MarkFlagRequired`, `MarkFlagsMutuallyExclusive`, `MarkFlagsOneRequired`, `MarkFlagsRequiredTogether`;
  `cmd.Flags().Changed(name)` = "did the user pass it".
- `StringSlice` splits on commas; use `StringArray` when values may contain commas.
- Completions: `ValidArgs` (static), `ValidArgsFunction` / `RegisterFlagCompletionFunc` (dynamic);
  `ShellCompDirectiveNoFileComp` when exhaustive. Test via the hidden `__complete` verb.
- Build the tree in `newRootCmd()`, not package-level `var`s in `init()`: flag state lives on the command and leaks
  between tests. One fresh tree per test (`SetOut`/`SetErr`/`SetArgs`).

## viper — rarely

Only when one key must resolve across flag, env and a multi-format config file in a shipped tool; otherwise one
JSON/TOML file plus `os.LookupEnv`.

- Precedence: `Set` > flag (only if `Changed`) > env > config file > remote KV > default.
- Env needs all three: `SetEnvPrefix("TOOL")`, `SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))`,
  `AutomaticEnv()`; without the replacer nested keys look for `TOOL_DB.HOST`.
- `Unmarshal` only sees known keys: `SetDefault` or `BindEnv` every key the struct reads.
- Tag fields `mapstructure:"…"`; durations and slices need decode hooks.
- A missing optional file is the value `viper.ConfigFileNotFoundError`:
  `var nf viper.ConfigFileNotFoundError; errors.As(err, &nf)` (a pointer target never matches). Only the
  name-and-path search returns it; a missing explicit `SetConfigFile` path is an `fs` error.
- `viper.New()` injected; the global leaks state across tests and goroutines.
- `WatchConfig` misses atomic-rename saves and calls back on another goroutine: decode into a fresh struct, validate,
  swap under a lock or `atomic.Pointer`.
