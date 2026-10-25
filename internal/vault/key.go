package vault

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/paths"
)

var keyringLookup = keyring.Lookup
var keyringSet = keyring.Set
var keyringDelete = keyring.Delete

// UseKeyringSeams swaps the lookup/set/delete functions this package shells out to for the OS
// keyring, returning a restore func; exported solely for vaulttest's FakeKeyring.
func UseKeyringSeams(lookup func(string) (string, bool, error), set func(string, string) error, del func(string) error) (restore func()) {
	savedLookup, savedSet, savedDelete := keyringLookup, keyringSet, keyringDelete
	keyringLookup, keyringSet, keyringDelete = lookup, set, del
	return func() { keyringLookup, keyringSet, keyringDelete = savedLookup, savedSet, savedDelete }
}

// testHookAfterKey runs right after UpdateSealed selects a key and before it opens the sealing
// tx; a spec test uses it to pause a writer mid-rotation (TestStaleWriterRetries).
var testHookAfterKey = func() {}

// errStaleKey means the tx's meta/kid no longer matches the key UpdateSealed selected before
// opening it — a rotation landed between selection and commit; the caller refreshes and retries.
var errStaleKey = errors.New("vault: master key is stale")

// account is the keyring account for a vault's current master key; account+"."+kid is the
// immutable, never-deleted account every key — current or retired — is durably stored under,
// written BEFORE any ciphertext is committed under that kid (R1, R2, R4). account alone is a
// convenience pointer to whichever kid is current: resolveKey/matchKey fall back to the per-kid
// account whenever the pointer is stale, absent, or shared by a copied root mid-rotation.
func account(id string) string { return "vault-key/" + id }

var keyCache struct {
	sync.Mutex
	path, vaultID, kid, command, source string
	key                                 *Key
}

// invalidateKey clears the per-process key cache so the next MasterKey call re-resolves from the
// keyring/env/key_command instead of serving a key a concurrent rotation has since retired.
func invalidateKey() {
	keyCache.Lock()
	defer keyCache.Unlock()
	keyCache.key = nil
}

func keyMeta() (id, kid, command string, err error) {
	err = View(func(t *Tx) error {
		id = vaultID(t)
		kid, _ = t.Get(Meta, "kid")
		command, _ = t.Get(Meta, "key_command")
		return nil
	})
	return
}

func MasterKey() (*Key, string, error) {
	id, kid, command, err := keyMeta()
	if err != nil {
		return nil, "", err
	}
	return resolveKey(id, kid, command)
}

// resolveKey resolves the key expected to carry kid for vault id, trying HARNESS_VAULT_KEY, the
// per-process cache, the keyring account, then key_command — the shared core MasterKey and
// ReadSecret both use, parameterized so a reader can anchor on a kid it captured earlier (R3).
//
// An explicitly configured key_command is tried whenever the keyring could not positively
// establish absence (ErrUnavailable/ErrNoAnswer — a locked session, no tool, Windows), not only on
// a clean miss: the keyring plane going dark must not strand the documented recovery path (R3/#3).
// A genuine keyring error with no usable alternative still surfaces as ErrLocked with the
// recovery instructions, never the raw keyring error.
func resolveKey(id, kid, command string) (*Key, string, error) {
	if v := os.Getenv("HARNESS_VAULT_KEY"); v != "" {
		k, err := parseKey(v, id)
		if err != nil {
			return nil, "env", err
		}
		return matchKey(id, k, "env", kid, command)
	}
	keyCache.Lock()
	defer keyCache.Unlock()
	if keyCache.key != nil && keyCache.path == Path() && keyCache.vaultID == id &&
		keyCache.kid == kid && keyCache.command == command {
		return keyCache.key, keyCache.source, nil
	}
	v, found, lookupErr := keyringLookup(account(id))
	recoverable := lookupErr != nil && (errors.Is(lookupErr, keyring.ErrUnavailable) || errors.Is(lookupErr, keyring.ErrNoAnswer))
	if lookupErr != nil && !recoverable {
		return nil, "", lookupErr
	}
	var k *Key
	var err error
	source := "keyring"
	switch {
	case found:
		if k, err = parseKey(v, id); err != nil && kid != "" {
			// Malformed pointer with a kid recorded: the pointer is only a convenience cache
			// of the immutable per-kid account, so fall to matchKey's lookup below instead.
			k, err = nil, nil
		}
	case command != "":
		source = "command"
		k, err = runKeyCommand(command, id)
	case recoverable:
		return nil, "", wrapKeyringUnavailable(lookupErr)
	case kid == "":
		return nil, "", ErrLocked
	}
	if err != nil {
		return nil, source, err
	}
	k, source, err = matchKey(id, k, source, kid, command)
	if err != nil {
		return nil, source, err
	}
	keyCache.path, keyCache.vaultID, keyCache.kid, keyCache.command, keyCache.source, keyCache.key =
		Path(), id, kid, command, source, k
	return k, source, nil
}

// parseKeyCommandArgv decodes meta/key_command as a validated JSON argv array: no shell is ever
// involved, so a quoted executable path or an argument containing spaces or quotes needs no
// quoting grammar of our own (R6).
func parseKeyCommandArgv(raw string) ([]string, error) {
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		return nil, fmt.Errorf("vault: key_command must be a JSON array of strings: %w", err)
	}
	if len(argv) == 0 {
		return nil, errors.New("vault: key_command must not be an empty argv array")
	}
	if argv[0] == "" {
		return nil, errors.New("vault: key_command argv[0] (the executable) must not be empty")
	}
	return argv, nil
}

// runKeyCommand runs raw's decoded argv directly (no shell) and parses its stdout as a master key
// scoped to vaultID.
func runKeyCommand(raw, vaultID string) (*Key, error) {
	argv, err := parseKeyCommandArgv(raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("vault: key_command: %w", ctx.Err())
	}
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			stderr := strings.TrimSpace(string(exit.Stderr))
			if len(stderr) > 200 {
				stderr = stderr[:200]
			}
			if stderr != "" {
				return nil, fmt.Errorf("vault: key_command: %w: %s", err, stderr)
			}
		}
		return nil, fmt.Errorf("vault: key_command: %w", err)
	}
	return parseKey(string(out), vaultID)
}

// matchKey resolves k against the vault's recorded kid: a direct hit returns k unchanged; a
// mismatch falls back to the immutable per-kid account, then to key_command if one is configured
// and wasn't already the source, before giving up with an error naming every source tried (R1,
// R2). A genuine keyring error on the fallback lookup is folded into that error text, never
// surfaced raw, so a locked/no-tool keyring never masquerades as "wrong key".
func matchKey(id string, k *Key, source, kid, command string) (*Key, string, error) {
	if k != nil && (kid == "" || k.Kid == kid) {
		return k, source, nil
	}
	if kid == "" {
		return nil, "", ErrLocked
	}
	tried := []string{source}
	v, found, err := keyringLookup(account(id) + "." + kid)
	switch {
	case err == nil && found:
		if rk, perr := parseKey(v, id); perr == nil && rk.Kid == kid {
			return rk, "keyring", nil
		}
		tried = append(tried, "keyring")
	case err != nil:
		tried = append(tried, fmt.Sprintf("keyring (%v)", err))
	default:
		tried = append(tried, "keyring")
	}
	if command != "" && source != "command" {
		if ck, cerr := runKeyCommand(command, id); cerr == nil && ck.Kid == kid {
			return ck, "command", nil
		} else if cerr != nil {
			tried = append(tried, fmt.Sprintf("key_command (%v)", cerr))
		} else {
			tried = append(tried, "key_command")
		}
	}
	if k == nil {
		return nil, "", ErrLocked
	}
	return nil, source, fmt.Errorf("%w: tried %s", ErrWrongKey, strings.Join(tried, ", "))
}

// wrapKeyringUnavailable turns a keyring that could not positively establish absence into a
// locked vault with recovery instructions, instead of letting EnsureKey generate over it (R6).
func wrapKeyringUnavailable(err error) error {
	if errors.Is(err, keyring.ErrUnavailable) || errors.Is(err, keyring.ErrNoAnswer) {
		return fmt.Errorf("%w: set HARNESS_VAULT_KEY or meta/key_command (%v)", ErrLocked, err)
	}
	return err
}

func EnsureKey() (*Key, error) {
	// Materialize the vault file (and its real vault_id) before any key resolution: the HKDF
	// subkey is salted by vault_id, so deriving one against a not-yet-created vault's empty id
	// would mismatch the id the first write actually persists.
	if _, err := ensureVaultID(); err != nil {
		return nil, err
	}
	k, _, err := MasterKey()
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, ErrLocked) {
		return nil, wrapKeyringUnavailable(err)
	}
	var result *Key
	err = paths.WithFileLock(Path()+".key", func() error {
		k, _, err := MasterKey()
		if err == nil {
			result = k
			return nil
		}
		if !errors.Is(err, ErrLocked) {
			return wrapKeyringUnavailable(err)
		}
		id, err := ensureVaultID()
		if err != nil {
			return err
		}
		_, kid, _, err := keyMeta()
		if err != nil {
			return err
		}
		if kid != "" {
			// A kid is recorded but no source above could open it: locked, not absent.
			return ErrLocked
		}
		_, found, lookupErr := keyringLookup(account(id))
		if lookupErr != nil {
			return wrapKeyringUnavailable(lookupErr)
		}
		if found {
			// A key is already in the keyring; meta/kid being empty here would be a corrupt
			// state, not an invitation to generate a second one.
			return ErrLocked
		}
		newK, value, err := randomKey(id)
		if err != nil {
			return err
		}
		// Persist the genesis key under its immutable per-kid account before the convenience
		// pointer, so matchKey can always find it even if a copied root's pointer diverges later.
		if err := keyringSet(account(id)+"."+newK.Kid, value); err != nil {
			return err
		}
		if err := keyringSet(account(id), value); err != nil {
			return err
		}
		if err := storeKeyID(newK); err != nil {
			return err
		}
		result = newK
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ensureVaultID materializes the vault file (and its random vault_id) if neither exists yet, and
// returns the id; Update backfills vault_id on any pre-existing vault that predates it.
func ensureVaultID() (string, error) {
	var id string
	err := Update(func(t *Tx) error {
		id = vaultID(t)
		return nil
	})
	return id, err
}

func storeKeyID(k *Key) error {
	return Update(func(t *Tx) error {
		kid, _ := t.Get(Meta, "kid")
		if kid != "" && kid != k.Kid {
			return ErrWrongKey
		}
		return t.Put(Meta, "kid", k.Kid)
	})
}

func randomKey(vaultID string) (*Key, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	value := base64.StdEncoding.EncodeToString(raw)
	k, err := parseKey(value, vaultID)
	return k, value, err
}

// RotateKeyOption configures RotateKey; the zero value is the default (guarded) behavior.
type RotateKeyOption func(*rotateKeyOpts)

type rotateKeyOpts struct{ force bool }

// ForceRotate bypasses the guard that refuses rotation while HARNESS_VAULT_KEY or meta/key_command
// is set — use only once the caller has updated that source with the new key immediately after.
func ForceRotate() RotateKeyOption { return func(o *rotateKeyOpts) { o.force = true } }

// RotateKey generates a new master key, re-seals every secret under it, and retains every key —
// outgoing and incoming — forever under its own immutable kid account so an archive, a stalled
// writer, or a sibling root sharing this vault_id mid-rotation can still open records sealed
// under it (R2, R4).
//
// Design (R1, R2): the new key is written to its immutable account+"."+newKid account BEFORE the
// re-seal transaction runs, and the convenience current-key pointer (account(id)) is updated only
// AFTER that transaction commits. Recovery needs no separate "pending" bookkeeping: whatever the
// pointer says, resolveKey/matchKey always find the key matching the committed meta/kid under its
// own immutable account, so a crash at any point between those two writes — or a second root's
// rotation overwriting the shared pointer with an unrelated kid — strands nothing. The only
// outcome of an interrupted rotation is a harmless, never-read orphan key sitting under its own
// kid account.
func RotateKey(opts ...RotateKeyOption) (oldKid, newKid string, err error) {
	var o rotateKeyOpts
	for _, opt := range opts {
		opt(&o)
	}
	err = paths.WithFileLock(Path()+".key", func() error {
		defer invalidateKey()
		old, source, e := MasterKey()
		if e != nil {
			return e
		}
		if source != "keyring" {
			return errors.New("vault: rotation requires a keyring-held master key")
		}
		oldKid = old.Kid
		id, kid, command, e := keyMeta()
		if e != nil {
			return e
		}
		if kid != "" && kid != old.Kid {
			return ErrWrongKey
		}
		if e := rotateGuard(o.force, command); e != nil {
			return e
		}
		acct := account(id)

		// Retain the outgoing key under its immutable kid account; idempotent if an earlier
		// rotation (or this one, resumed after a crash) already wrote it.
		if e := keyringSet(acct+"."+old.Kid, old.raw); e != nil {
			return e
		}

		next, value, e := randomKey(id)
		if e != nil {
			return e
		}
		newKid = next.Kid

		// Persist the new key under its immutable kid account BEFORE any ciphertext is committed
		// under it (R1): a crash after this line but before (or during) the reseal below leaves
		// the new key durable and unreferenced — harmless — never stranded or lost.
		if e := keyringSet(acct+"."+newKid, value); e != nil {
			return e
		}

		if e := Update(func(t *Tx) error {
			curKid, _ := t.Get(Meta, "kid")
			if curKid != "" && curKid != old.Kid {
				return ErrWrongKey
			}
			for _, name := range t.Names(Secrets) {
				r, ok, rerr := t.Record(name)
				if rerr != nil {
					return rerr
				}
				if !ok {
					return fmt.Errorf("vault: invalid record %q", name)
				}
				plain, e := old.Open(name, r)
				if e != nil {
					return e
				}
				r.Sealed = next.Seal(name, plain)
				r.Kid = next.Kid
				if e := t.PutRecord(name, r); e != nil {
					return e
				}
			}
			if err := retireKid(t, old.Kid); err != nil {
				return err
			}
			return t.Put(Meta, "kid", next.Kid)
		}); e != nil {
			return e
		}
		// Convenience pointer only, updated after the atomic reseal commits; resolveKey/matchKey
		// never depend on this being current (R1, R2).
		if e := keyringSet(acct, value); e != nil {
			return e
		}
		audit.Log(audit.KeyRotate, oldKid+"->"+newKid, "")
		return nil
	})
	return
}

// rotateGuard refuses rotation while HARNESS_VAULT_KEY or key_command supplies the key, unless forced.
func rotateGuard(force bool, command string) error {
	switch {
	case force:
		return nil
	case os.Getenv("HARNESS_VAULT_KEY") != "":
		return errors.New("vault: HARNESS_VAULT_KEY is set; rotating would silently break it — " +
			"update the env var with the new key right after, or pass --force")
	case command != "":
		return errors.New("vault: meta/key_command is set; rotating would silently break it — " +
			"update that command's output with the new key right after, or pass --force")
	}
	return nil
}

// retireKid appends kid to meta/retired, reading it raw: a corrupt list refuses rather than losing history.
func retireKid(t *Tx, kid string) error {
	raw, _ := t.Get(Meta, "retired")
	var retired []string
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &retired); err != nil {
			return fmt.Errorf("vault: meta/retired is corrupt; refusing to rotate (history would be lost): %w", err)
		}
	}
	if !slices.Contains(retired, kid) {
		retired = append(retired, kid)
	}
	data, err := json.Marshal(retired)
	if err != nil {
		return err
	}
	return t.Put(Meta, "retired", string(data))
}

// UpdateSealed selects a key outside the tx and verifies meta/kid inside it; a stale key (a
// rotation landed between selection and commit) is refreshed and retried, bounded at 3 attempts.
func UpdateSealed(fn func(*Tx, *Key) error) error {
	var err error
	for range 3 {
		var k *Key
		k, err = EnsureKey()
		if err != nil {
			return err
		}
		testHookAfterKey()
		err = Update(func(t *Tx) error {
			kid, ok := t.Get(Meta, "kid")
			if !ok || kid == "" {
				if e := t.Put(Meta, "kid", k.Kid); e != nil {
					return e
				}
			} else if kid != k.Kid {
				return errStaleKey
			}
			return fn(t, k)
		})
		if errors.Is(err, errStaleKey) {
			invalidateKey()
			continue
		}
		return err
	}
	if errors.Is(err, errStaleKey) {
		return ErrWrongKey
	}
	return err
}

// ReadSecret reads one record and its kid in a single coherent View, then opens it with the key
// for that exact kid, retrying once if a concurrent rotation invalidated the key mid-read (R3).
func ReadSecret(name string) (value string, found bool, err error) {
	for attempt := range 2 {
		var id, kid, command string
		var record Record
		var ok bool
		if verr := View(func(t *Tx) error {
			id = vaultID(t)
			kid, _ = t.Get(Meta, "kid")
			command, _ = t.Get(Meta, "key_command")
			var rerr error
			record, ok, rerr = t.Record(name)
			return rerr
		}); verr != nil {
			return "", false, verr
		}
		if !ok {
			return "", false, nil
		}
		k, _, kerr := resolveKey(id, kid, command)
		if kerr != nil {
			if errors.Is(kerr, ErrWrongKey) && attempt == 0 {
				invalidateKey()
				continue
			}
			return "", false, kerr
		}
		v, oerr := k.Open(name, record)
		if oerr != nil {
			if errors.Is(oerr, ErrWrongKey) && attempt == 0 {
				invalidateKey()
				continue
			}
			return "", false, oerr
		}
		return v, true, nil
	}
	return "", false, ErrWrongKey
}
