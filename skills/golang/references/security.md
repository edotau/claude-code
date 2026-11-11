# Go Security and Safety

The Go-specific how for crash-free, attacker-resistant code (module targets Go 1.27; features dated by release).

Authority: [rules/standards/security.md](../../../rules/standards/security.md) — its rules hold; this is the Go how.

## Triage before flagging

- Trace each sink argument back to where it enters (request, file, env, another service's row). Only data an
  attacker controls makes a sink exploitable; constants and parsed ints rarely do.
- Upstream validation lowers severity, it never closes the finding — every layer guards itself. Record a deliberate
  downgrade inline (`// security: id is strconv.Atoi output`) so the next audit doesn't re-flag it.
- Safety bugs (nil, aliasing, overflow) become security bugs the moment an attacker picks the input: a remotely
  triggerable panic is a DoS, a wrapped length is an out-of-bounds read.

## Nil and zero-value traps

| Value | Read | Write / call | Notes |
| --- | --- | --- | --- |
| nil map | zero value, `len` 0 | **panic** on assign | `delete` and `range` are fine |
| nil slice | index **panics** | `append` allocates | behaves as empty everywhere else |
| nil chan | blocks forever | blocks forever | `close` panics; useful only to disable a `select` case |
| nil func | — | **panic** on call | default to a no-op in the constructor |
| nil `*T` | method runs until it dereferences | field access **panics** | guard only where nil is a documented state |

- An interface is nil only when type *and* value are nil. Returning a nil `*MyErr` as `error` yields `err != nil`;
  return the literal `nil` on the success path, never a typed variable.
- Bare assertion `x.(T)` panics on mismatch — use comma-ok on any value that crossed a boundary. For reflection use
  `reflect.TypeAssert[T](v)` (1.25) instead of `v.Interface().(T)`.
- Design types whose zero value works (`sync.Mutex`, `bytes.Buffer` do); otherwise lazy-init under the lock or force
  a constructor. `sync.OnceValue`/`OnceValues` (1.21) beat a hand-rolled `once` + field, and keep the error.
- `errors.As` needs a pointer to the target type; passing a value panics.

## Integers: overflow and conversion

- Go has no checked arithmetic: `+`, `*` and `<<` wrap silently, and a conversion (`int32(x)`, `uint(neg)`)
  truncates or reinterprets without error. gosec `G115` flags narrowing conversions.
- `int` is 32 bits on 386/arm/wasm — a size that fits on amd64 can wrap there. Use `int64` for wire values.
- Bound at parse time: `strconv.ParseInt(s, 10, 32)` rejects out-of-range input, unlike `Atoi` then `int32(...)`.
- Size math on attacker input (`count * elemSize`, `off + n`) gets an overflow check *and* a ceiling before `make`;
  otherwise one header allocates gigabytes or a wrapped offset passes a bounds check.
- Integer `/` or `%` by zero panics; float division yields `±Inf`/`NaN`, and `NaN != NaN` breaks map keys and sorts.

```go
func allocSize(count, elem uint64) (int, error) {
    hi, lo := bits.Mul64(count, elem) // hi != 0 means the product overflowed 64 bits
    if hi != 0 || lo > maxAlloc {
        return 0, fmt.Errorf("alloc %d x %d exceeds %d bytes", count, elem, maxAlloc)
    }
    return int(lo), nil // safe: maxAlloc < math.MaxInt32
}
```

- `math/big.Rat`/`Int` grow without bound; cap `BitLen()` when an attacker drives the operations.

## Slices, maps, and aliasing

- `append` writes into spare capacity in place, so two slices from one array clobber each other. Cap a slice before
  handing it out or appending to a borrowed one: `s[:len(s):len(s)]` forces the next `append` to copy.
- A small subslice pins its whole backing array — `slices.Clone(buf[:64])` before retaining a header out of a
  1 MB read. Same for `strings` cut from a large input when the input is otherwise dead: `strings.Clone`.
- Return `slices.Clone`/`maps.Clone` of internal state from exported getters, and copy slices a caller passed in
  before storing them — otherwise either side mutates the other's invariants (shallow copies; clone deeper if needed).
- Remove with `slices.DeleteFunc`/`slices.Delete` (they zero the vacated tail since 1.22, freeing pointers); a
  forward loop that re-slices inside `range` skips elements.
- Never depend on map iteration order; `slices.Sorted(maps.Keys(m))` when output must be stable.
- Concurrent map write is a `fatal error`, not a panic — `recover` can't stop it. Guard with a mutex or use
  `sync.Map` for write-once keys (patterns: [concurrency.md](concurrency.md)); a data race on a check-then-act
  path is also an authz bypass, so run `go test -race`.
- `defer` in a loop runs at function exit; open files or held locks pile up. Move the body into a function.

## Injection

| Sink | Unsafe | Safe |
| --- | --- | --- |
| SQL value | `fmt.Sprintf("... id = %s", id)` | `db.QueryContext(ctx, "... id = $1", id)` (`?` on MySQL/SQLite) |
| SQL identifier / `ORDER BY` | user string spliced in | lookup in a fixed allowlist (placeholders bind values only) |
| SQL `IN (...)` | `strings.Join(ids, ",")` | generated `$1..$n` + `args...`; `sqlx.In` + `Rebind`; pgx `= ANY($1)` |
| Process | `exec.Command("sh", "-c", "tar " + f)` | `exec.CommandContext(ctx, "tar", "-xf", "--", f)` |
| HTML | `fmt.Fprintf(w, "<p>%s</p>", s)`, `text/template` | `html/template` (context-aware escaping) |
| Redirect / outbound URL | raw query param | `url.Parse`, require scheme `https`, host in an allowlist |

- `exec.Command` never invokes a shell, but the target program still parses flags: an argument starting with `-`
  is option injection. End options with `--`, or reject a leading `-`. Never pass user text as the program name.
- `exec.LookPath` refuses a binary found via a relative `PATH` entry (`exec.ErrDot`, 1.19); don't re-enable it.
- Set `cmd.Env` explicitly for children that run untrusted code; the default inherits every secret in your env.
- `html/template` escaping is defeated by converting input to `template.HTML`, `template.JS` or `template.URL` —
  those types assert "already safe". Never `Parse` a template body from user input (it can call any exported method).
- SSRF: resolving the host and checking the IP once is a TOCTOU (DNS rebinding). Check the resolved address in a
  `net.Dialer.Control` hook, rejecting `ip.IsLoopback()`, `IsPrivate()`, `IsLinkLocalUnicast()` (cloud metadata at
  169.254.169.254) and `IsUnspecified()`; disable redirects or re-check each hop via `CheckRedirect`.
- Decoding: `encoding/gob` isn't hardened against hostile input; prefer `encoding/json` into a concrete struct. Wrap
  bodies in `http.MaxBytesReader`, and use `dec.DisallowUnknownFields()` where strictness matters. `encoding/xml`
  doesn't resolve external entities, so XXE isn't a stdlib risk; size and depth still need limits.

## Filesystem and path traversal

- `os.Root` (1.24) is the answer for untrusted relative names: `root, err := os.OpenRoot(dir)` then `root.Open`,
  `root.OpenFile`, `root.Create`, `root.Stat`, and since 1.25 `MkdirAll`, `ReadFile`, `WriteFile`, `Rename`,
  `RemoveAll`. It rejects `..` escapes and symlinks that resolve outside, at the syscall level, so it survives a
  racing symlink swap. One-shot form: `os.OpenInRoot(dir, name)`.
- `os.Root` is not a sandbox: it doesn't stop device files, FIFOs or a bind mount placed inside the root. Archive
  extraction still rejects non-regular entries (`hdr.Typeflag`, `f.Mode().IsRegular()`).
- `filepath.Clean` + `strings.HasPrefix(p, base)` is broken: `/srv/app-evil` has prefix `/srv/app`, and symlinks
  bypass any lexical check. Lexical gate without `os.Root`: `filepath.IsLocal(name)` (1.20) — rejects absolute,
  `..`-escaping, empty and Windows reserved names; `filepath.Localize` (1.23) converts a slash path safely.
- Zip/tar: entry names are attacker input (ZipSlip). Open each through `os.Root`; `archive/zip` and `archive/tar`
  return `ErrInsecurePath` only under GODEBUG `zipinsecurepath=0` / `tarinsecurepath=0`, so don't rely on it.
- Decompression bombs: copy through `io.LimitReader(r, max+1)` and fail if more than `max` bytes arrived — a bare
  `io.LimitReader` truncates silently and looks like success. gosec `G110`.
- Temp files: `os.CreateTemp`/`os.MkdirTemp` (random name, `0600`/`0700`); never a fixed path in `/tmp`.
- Modes: secrets `0600`, private dirs `0700`/`0750`; the umask only removes bits, so `0666` stays group-readable
  under a `002` umask. gosec `G301`–`G306`.
- Static serving: `http.FileServerFS(fsys)` over an `os.DirFS` or `embed.FS`; `http.ServeFile` with a path joined
  from a query param is a traversal sink (it only sanitises `r.URL.Path`).

## Crypto primitives

- Randomness for tokens, keys, nonces, salts: `crypto/rand`. `rand.Text()` (1.24) gives a 26-char base32 token of
  128+ bits; `rand.Read` never returns an error since 1.24 (it crashes on entropy failure). `math/rand` and
  `math/rand/v2` are predictable — gosec `G404`.
- Compare secrets with `subtle.ConstantTimeCompare` (MACs via `hmac.Equal`). It returns early on a length mismatch,
  so it leaks length: for variable-length secrets compare fixed-size digests (`sha256.Sum256` of both sides).
- AEAD only: `cipher.NewGCMWithRandomNonce` (1.24) generates and prepends the nonce, removing nonce reuse — the one
  GCM failure that leaks the key stream. Otherwise `chacha20poly1305.NewX` (random 24-byte nonces are safe).
  Raw `block.Encrypt` is ECB; CBC/CTR without a MAC are malleable.
- Passwords: `golang.org/x/crypto/argon2.IDKey` (time 3, 64 MiB, per-user 16-byte salt) or `bcrypt` (72-byte
  input limit). Stdlib `crypto/pbkdf2` (1.24, 600k+ iterations of SHA-256) where FIPS demands it. Never a bare hash.
- KDFs and newer primitives are stdlib since 1.24: `crypto/hkdf`, `crypto/sha3`, `crypto/mlkem`. FIPS 140-3 mode:
  `GODEBUG=fips140=on`.
- MD5/SHA-1/DES/RC4/RSA < 2048 for anything security-bearing is a finding (gosec `G401`, `G501`–`G505`); MD5 as a
  non-adversarial cache key is fine — say so in a `#nosec` reason.
- JWT: pin the algorithm in the key func (`token.Method.(*jwt.SigningMethodEd25519)`), require `exp`, `iss`, `aud`
  (`jwt.WithExpirationRequired`, `WithIssuer`, `WithAudience`); otherwise `alg` confusion forges tokens.

## TLS and HTTP

- Leave `tls.Config` defaults alone unless you have a reason: the minimum is TLS 1.2 for client and server, TLS 1.3
  suites aren't configurable, and the default curves include the post-quantum hybrid `X25519MLKEM768` (1.24).
  Setting `CurvePreferences` or `CipherSuites` explicitly can silently drop it.
- `InsecureSkipVerify: true` is a finding (gosec `G402`). For pinning or a private CA, set `RootCAs` or add a
  `VerifyConnection` callback — never skip verification and re-implement it.
- mTLS server: `ClientAuth: tls.RequireAndVerifyClientCert` with `ClientCAs`; `RequireAnyClientCert` verifies nothing.
- SSH: `ssh.InsecureIgnoreHostKey()` is MITM by design; use `knownhosts.New` or `ssh.FixedHostKey`.
- `http.ListenAndServe` has no timeouts. Build an `http.Server` with `ReadHeaderTimeout` (Slowloris, gosec
  `G112`), `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, and cap bodies with `http.MaxBytesReader`.
- The package-level `http.Get`/`DefaultClient` never time out; give outbound clients a `Timeout` or a ctx deadline.
- CSRF: `http.CrossOriginProtection` (1.25) checks `Sec-Fetch-Site`/`Origin` on non-safe methods — wrap the mux.
- Cookies carrying a session: `HttpOnly`, `Secure`, `SameSite: http.SameSiteLaxMode` (or `Strict`), a `__Host-`
  name prefix to pin host and path.
- `import _ "net/http/pprof"` registers handlers on `DefaultServeMux`; serve pprof from a separate mux on loopback.
- Proxy headers (`X-Forwarded-For`, `X-Real-IP`) are client-forgeable unless your own load balancer sets them.
- Errors to clients are generic (`http.Error(w, "internal error", 500)`); the wrapped detail goes to the log only.

## Secrets in process

Where secrets live is the authority file's call; in Go, keep them from leaking once loaded.

- Give secret-bearing types a redacting `String()`, `GoString()` and `slog.LogValuer` — `%+v` and `slog.Any`
  otherwise print every field. A struct-level wrapper type (`type Secret string`) makes this one place.
- Tag such fields `json:"-"` so an accidental `json.Marshal` of a config or error response omits them.
- Prefer `[]byte` for key material you can `clear()` after use; a `string` is immutable and lingers until GC.
  `clear` is best-effort (copies made by the runtime or GC moves aren't wiped), not a guarantee.
- Never embed a secret via `-ldflags "-X"` or a constant: `strings` on the binary recovers it. gosec `G101`.
- Strip `Authorization`/`Cookie` from requests sent to error trackers and traces (`BeforeSend` hooks, span filters).
- Sanitise attacker strings before text logs (CR/LF forge log lines); `slog.JSONHandler` quotes values for you.

## Tooling

```bash
go vet ./...
go test -race ./...                                  # races are correctness and authz bugs
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go run github.com/securego/gosec/v2/cmd/gosec@latest ./...
go test -fuzz=FuzzParse -fuzztime=60s ./internal/parse   # every parser of untrusted bytes
```

- `govulncheck` reports only vulnerabilities whose affected symbols your code reaches — a hit is real, fix or bump
  it. `-show verbose` adds imported-but-unreached ones; `-mode=binary ./app` scans a built binary. A `tool`
  directive in `go.mod` (1.24) pins either tool: `go get -tool <pkg>`, then `go tool govulncheck ./...`.
- `gosec` is also a golangci-lint linter. Suppress per line with a rule and reason — `// #nosec G304 -- path from
  os.Root` — never a blanket `#nosec`; an unexplained suppression is itself a review finding.
- High-value gosec rules: `G101` creds, `G107`/`G704` tainted URL, `G110` bomb, `G115` int conversion, `G201`/`G202`
  SQL, `G203` template, `G204` exec, `G304` file path, `G402` TLS, `G404` weak rand.
- `unsafe` outside a reviewed low-level package is a finding; `unsafe.String`/`unsafe.Slice` (1.20) replace the
  deprecated `reflect.StringHeader` tricks where zero-copy is genuinely needed.

Adapted from samber/cc-skills-golang (MIT): golang-security, golang-safety.
