# Go Security and Safety

Crash-free, attacker-resistant Go (features dated by Go release).

## Triage before flagging

- Trace each sink argument back to where it enters (request, file, env, another service's row). Only
  attacker-controlled data makes a sink exploitable; constants and parsed ints rarely do.

## Nil and zero-value traps

| Value | Read | Write / call | Notes |
| --- | --- | --- | --- |
| nil map | zero value, `len` 0 | **panic** on assign | `delete` and `range` are fine |
| nil slice | index **panics** | `append` allocates | empty everywhere else |
| nil chan | blocks forever | blocks forever | `close` panics; useful only to disable a `select` case |
| nil func | — | **panic** on call | default to a no-op in the constructor |
| nil `*T` | method runs until it dereferences | field access **panics** | guard only where nil is a documented state |

- An interface is nil only when type *and* value are nil. Returning a nil `*MyErr` as `error` yields `err != nil`;
  return the literal `nil` on success.
- Bare assertion `x.(T)` panics on mismatch: comma-ok on any value that crossed a boundary.
- `errors.As` needs a pointer to the target type; a value panics.

## Integers: overflow and conversion

- No checked arithmetic: `+`, `*`, `<<` wrap silently; conversions (`int32(x)`, `uint(neg)`) truncate without
  error. gosec `G115` flags narrowing.
- Bound at parse time: `strconv.ParseInt(s, 10, 32)` rejects out-of-range input, unlike `Atoi` then `int32(...)`.
- Size math on attacker input (`count * elemSize`, `off + n`) needs an overflow check *and* a ceiling before `make`.

```go
func allocSize(count, elem uint64) (int, error) {
    hi, lo := bits.Mul64(count, elem) // hi != 0 means the product overflowed 64 bits
    if hi != 0 || lo > maxAlloc {
        return 0, fmt.Errorf("alloc %d x %d exceeds %d bytes", count, elem, maxAlloc)
    }
    return int(lo), nil // safe: maxAlloc < math.MaxInt32
}
```

## Slices, maps, and aliasing

- `append` writes into spare capacity in place, so two slices from one array clobber each other. Cap before handing
  out: `s[:len(s):len(s)]` forces the next `append` to copy.
- A small subslice pins its whole backing array: `slices.Clone(buf[:64])` before retaining it from a 1 MB read;
  `strings.Clone` for strings cut from a large dead input.
- Concurrent map write is a `fatal error`, not a panic: `recover` can't stop it. Mutex, or `sync.Map` for write-once
  keys ([concurrency.md](concurrency.md)); a race on a check-then-act path is also an authz bypass: `go test -race`.

- `slices.DeleteFunc`/`Delete` zero the vacated tail (1.22); a forward loop re-slicing in `range` skips elements.

## Injection

| Sink | Unsafe | Safe |
| --- | --- | --- |
| SQL value | `fmt.Sprintf("... id = %s", id)` | `db.QueryContext(ctx, "... id = $1", id)` (`?` on MySQL/SQLite) |
| SQL identifier / `ORDER BY` | user string spliced in | lookup in a fixed allowlist (placeholders bind values only) |
| SQL `IN (...)` | `strings.Join(ids, ",")` | generated `$1..$n` + `args...`; `sqlx.In` + `Rebind`; pgx `= ANY($1)` |
| Process | `exec.Command("sh", "-c", "tar " + f)` | `exec.CommandContext(ctx, "tar", "-xf", "--", f)` |
| HTML | `fmt.Fprintf(w, "<p>%s</p>", s)`, `text/template` | `html/template` (context-aware escaping) |
| Redirect / outbound URL | raw query param | `url.Parse`, require scheme `https`, host in an allowlist |

- `exec.LookPath` refuses a binary found via a relative `PATH` entry (`exec.ErrDot`, 1.19); don't re-enable it.
- `exec.Command` never invokes a shell, but the target still parses flags: a leading `-` is option injection. End
  options with `--` or reject it. Never pass user text as the program name.
- `html/template` escaping is defeated by converting input to `template.HTML`/`JS`/`URL`. Never `Parse` a template
  body from user input (it can call any exported method).
- SSRF: resolve-then-check is a TOCTOU (DNS rebinding). Check the resolved address in a `net.Dialer.Control` hook,
  rejecting `ip.IsLoopback()`, `IsPrivate()`, `IsLinkLocalUnicast()` (metadata at 169.254.169.254) and
  `IsUnspecified()`; disable redirects or re-check each hop via `CheckRedirect`.
- Decoding: `encoding/gob` isn't hardened against hostile input; prefer `encoding/json` into a concrete struct with
  `http.MaxBytesReader` and `dec.DisallowUnknownFields()` where strict. `encoding/xml` doesn't resolve external
  entities (no XXE); size and depth still need limits.

## Filesystem and path traversal

- `os.Root` (1.24) is the answer for untrusted relative names: `root, err := os.OpenRoot(dir)`, then `root.Open`,
  `OpenFile`, `Create`, `Stat`, and since 1.25 `MkdirAll`, `ReadFile`, `WriteFile`, `Rename`, `RemoveAll`. It rejects
  `..` and symlink escapes at the syscall level, surviving a racing symlink swap. One-shot: `os.OpenInRoot(dir, name)`.
- `os.Root` is not a sandbox: it doesn't stop device files, FIFOs or a bind mount inside the root. Archive
  extraction still rejects non-regular entries (`hdr.Typeflag`, `f.Mode().IsRegular()`).
- `filepath.Clean` + `strings.HasPrefix(p, base)` is broken (`/srv/app-evil` has prefix `/srv/app`; symlinks bypass
  lexical checks). Without `os.Root`: `filepath.IsLocal(name)` (1.20) rejects absolute, `..`-escaping, empty and
  Windows reserved names; `filepath.Localize` (1.23) converts a slash path safely.
- Zip/tar entry names are attacker input (ZipSlip): open each through `os.Root`. `ErrInsecurePath` fires only under
  GODEBUG `zipinsecurepath=0` / `tarinsecurepath=0`; don't rely on it.
- Decompression bombs: copy through `io.LimitReader(r, max+1)` and fail if more than `max` arrived (a bare
  `LimitReader` truncates silently). gosec `G110`.
- Static serving: `http.FileServerFS(fsys)` over `os.DirFS`/`embed.FS`; `http.ServeFile` with a path built from a
  query param is a traversal sink (it only sanitises `r.URL.Path`).

## Crypto primitives

- Tokens, keys, nonces, salts: `crypto/rand`. `rand.Text()` (1.24) gives a 26-char base32 token of 128+ bits;
  `rand.Read` never errors since 1.24 (crashes on entropy failure). `math/rand(/v2)` is predictable: gosec `G404`.
- Compare secrets with `subtle.ConstantTimeCompare` (MACs: `hmac.Equal`). It returns early on length mismatch, so
  for variable-length secrets compare fixed-size digests (`sha256.Sum256` of both).
- AEAD only: `cipher.NewGCMWithRandomNonce` (1.24) generates and prepends the nonce (nonce reuse leaks the key
  stream); else `chacha20poly1305.NewX` (random 24-byte nonces are safe). Raw `block.Encrypt` is ECB; CBC/CTR
  without a MAC are malleable.
- Passwords: `golang.org/x/crypto/argon2.IDKey` (time 3, 64 MiB, per-user 16-byte salt) or `bcrypt` (72-byte input
  limit); stdlib `crypto/pbkdf2` (1.24, 600k+ SHA-256 iterations) where FIPS demands. Never a bare hash.
- MD5/SHA-1/DES/RC4/RSA < 2048 for anything security-bearing is a finding (gosec `G401`, `G501`–`G505`); MD5 as a
  non-adversarial cache key is fine: say so in the `#nosec` reason.
- JWT: pin the algorithm in the key func (`token.Method.(*jwt.SigningMethodEd25519)`) and require `exp`, `iss`,
  `aud` (`jwt.WithExpirationRequired`, `WithIssuer`, `WithAudience`), or `alg` confusion forges tokens.

## TLS and HTTP

- Leave `tls.Config` defaults alone: minimum TLS 1.2, TLS 1.3 suites not configurable, default curves include
  hybrid `X25519MLKEM768` (1.24). Setting `CurvePreferences`/`CipherSuites` can silently drop it.
- `InsecureSkipVerify: true` is a finding (gosec `G402`). For pinning or a private CA set `RootCAs` or a
  `VerifyConnection` callback.
- mTLS: `ClientAuth: tls.RequireAndVerifyClientCert` + `ClientCAs`; `RequireAnyClientCert` verifies nothing.
- Never embed a secret via `-ldflags "-X"` or a constant (`strings` recovers it; gosec `G101`).
- Server and client timeouts, body caps: [services.md](services.md) → net/http servers, http.Client.
- CSRF: `http.CrossOriginProtection` (1.25) checks `Sec-Fetch-Site`/`Origin` on non-safe methods: wrap the mux.

- `X-Forwarded-For`/`X-Real-IP` are client-forgeable unless your own load balancer sets them.

## Secrets in process

- Give secret-bearing types a redacting `String()`, `GoString()` and `slog.LogValuer` (`%+v` and `slog.Any` print
  every field); a `type Secret string` wrapper makes this one place.

## Tooling

```bash
go test -race ./...                                  # races are correctness and authz bugs
go run github.com/securego/gosec/v2/cmd/gosec@latest ./...
go test -fuzz=FuzzParse -fuzztime=60s ./internal/parse   # every parser of untrusted bytes
```

- `govulncheck` (reachability, binary mode, pinning as a `tool`): [tooling.md](tooling.md) → govulncheck.
- High-value gosec: `G101` `G107` `G110` `G115` `G201` `G202` `G203` `G204` `G304` `G402` `G404` `G704`.
- Strip `Authorization`/`Cookie` from requests sent to error trackers and traces.
- `unsafe.String`/`unsafe.Slice` (1.20) replace `reflect.StringHeader` tricks; other `unsafe` is a finding.
- `gosec` is also a golangci-lint linter. Suppress per line with rule and reason (`// #nosec G304 -- path from
  os.Root`); a blanket or unexplained `#nosec` is a review finding.
