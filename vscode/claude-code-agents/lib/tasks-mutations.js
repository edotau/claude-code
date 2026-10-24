'use strict';
// Per-item mutation queue (Orca's jira-issue-mutation-queue shape): optimistic patch now, writes serialised per item,
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

  /** Applies `patch` to the row immediately, then runs `write()`; a failure restores the pre-patch row and records the error. */
  enqueue({ id, patch, write }) {
    const before = this.getItem(id);
    if (before && patch) this.setItem(id, { ...before, ...patch });
    this.errors.delete(id);
    this.pending.set(id, (this.pending.get(id) || 0) + 1);
    this.onChange();
    const run = async () => {
      try {
        const result = await write();
        const fresh = this.refetch ? await this.refetch(id).catch(() => null) : null;
        if (fresh) this.setItem(id, fresh);
        return result;
      } catch (err) {
        if (before) this.setItem(id, before);
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
