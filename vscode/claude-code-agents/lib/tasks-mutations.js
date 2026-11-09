'use strict';
// Per-item mutation queue (queue shape): optimistic patch now, writes serialised per item,
// refetch after success, rollback + inline error on failure. Pure: the host supplies get/set/refetch/onChange.

class MutationQueue {
  /** `getItem(id)` → row | undefined; `setItem(id, row)`; `refetch(id)` → fresh row | null; `onChange()` re-renders. */
  constructor({ getItem, setItem, refetch, onChange }) {
    this.getItem = getItem;
    this.setItem = setItem;
    this.refetch = refetch;
    this.onChange = onChange || (() => {});
    this.tails = new Map(); // id → promise chain so writes to one item land in order
    this.pending = new Map(); // id → number of in-flight writes
    this.errors = new Map(); // id → { message, retry }
    this.bases = new Map(); // id → row before the first in-flight write
    this.inflight = new Map(); // id → entries { patch } of in-flight writes, oldest first
  }

  isPending(id) {
    return (this.pending.get(id) || 0) > 0;
  }

  errorFor(id) {
    return this.errors.get(id) || null;
  }

  clearError(id) {
    this.errors.delete(id);
    this.onChange();
  }

  /** Row = base with each in-flight patch layered on, oldest first. */
  static layer(base, entries) {
    return entries.reduce((row, e) => (e.patch ? { ...row, ...e.patch } : row), base);
  }

  /** Drops `entry` from the in-flight list and rebuilds the row from the base plus the writes still pending. */
  settle(id, entry, { fresh, failed }) {
    const rest = (this.inflight.get(id) || []).filter((e) => e !== entry);
    if (rest.length) this.inflight.set(id, rest);
    else this.inflight.delete(id);
    if (fresh) this.bases.set(id, fresh);
    const base = this.bases.get(id);
    if (!rest.length) this.bases.delete(id);
    if (base && (failed || fresh)) this.setItem(id, MutationQueue.layer(failed ? base : fresh, rest));
  }

  /** Applies `patch` to the row immediately, then runs `write()`; a failure reverts only that write's patch. */
  enqueue({ id, patch, write }) {
    const current = this.getItem(id);
    if (!(this.inflight.get(id) || []).length) this.bases.set(id, current);
    const entry = { patch };
    this.inflight.set(id, [...(this.inflight.get(id) || []), entry]);
    if (current && patch) this.setItem(id, { ...current, ...patch });
    this.errors.delete(id);
    this.pending.set(id, (this.pending.get(id) || 0) + 1);
    this.onChange();
    const run = async () => {
      try {
        const result = await write();
        const fresh = this.refetch ? await this.refetch(id).catch(() => null) : null;
        this.settle(id, entry, { fresh, failed: false });
        return result;
      } catch (err) {
        this.settle(id, entry, { fresh: null, failed: true });
        this.errors.set(id, { message: err && err.message ? err.message : String(err), retry: () => this.enqueue({ id, patch, write }) });
        throw err;
      } finally {
        const left = (this.pending.get(id) || 1) - 1;
        if (left > 0) this.pending.set(id, left);
        else this.pending.delete(id);
        this.onChange();
      }
    };
    const tail = (this.tails.get(id) || Promise.resolve()).then(run, run);
    this.tails.set(id, tail.catch(() => {}));
    return tail;
  }
}

module.exports = { MutationQueue };
