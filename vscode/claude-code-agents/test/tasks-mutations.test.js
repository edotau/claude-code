'use strict';
// Unit tests for lib/tasks-mutations.js: optimistic patch, per-id serialisation, rollback + error on failure.
const test = require('node:test');
const assert = require('node:assert/strict');
const { MutationQueue } = require('../lib/tasks-mutations');

function makeStore(initial) {
  const rows = new Map(Object.entries(initial));
  const changes = [];
  const queue = new MutationQueue({
    getItem: (id) => rows.get(id),
    setItem: (id, row) => rows.set(id, row),
    refetch: async (id) => rows.get(id) || null,
    onChange: () => changes.push(Date.now()),
  });
  return { rows, changes, queue };
}

function defer() {
  let resolve, reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

test('enqueue applies the optimistic patch synchronously before write runs', () => {
  const { rows, queue } = makeStore({ a: { id: 'a', status: 'open' } });
  let patchedBeforeWrite = null;
  const d = defer();
  queue.enqueue({
    id: 'a',
    patch: { status: 'closed' },
    write: async () => {
      patchedBeforeWrite = { ...rows.get('a') };
      return d.promise;
    },
  });
  // synchronous assertion: patch already applied before the write's body even ran
  assert.deepEqual(rows.get('a'), { id: 'a', status: 'closed' });
  d.resolve();
});

test('isPending(id) is true during the write and false after it settles', async () => {
  const { queue } = makeStore({ a: { id: 'a', status: 'open' } });
  const d = defer();
  const p = queue.enqueue({ id: 'a', patch: { status: 'closed' }, write: () => d.promise });
  assert.equal(queue.isPending('a'), true);
  d.resolve('ok');
  await p;
  assert.equal(queue.isPending('a'), false);
});

test('on success: refetch(id) is called and its non-null result replaces the row', async () => {
  const rows = new Map(Object.entries({ a: { id: 'a', status: 'open' } }));
  const refetchCalls = [];
  const queue = new MutationQueue({
    getItem: (id) => rows.get(id),
    setItem: (id, row) => rows.set(id, row),
    refetch: async (id) => {
      refetchCalls.push(id);
      return { id: 'a', status: 'closed', serverField: 'fresh' };
    },
    onChange: () => {},
  });
  await queue.enqueue({ id: 'a', patch: { status: 'closed' }, write: async () => 'ok' });
  assert.deepEqual(refetchCalls, ['a']);
  assert.deepEqual(rows.get('a'), { id: 'a', status: 'closed', serverField: 'fresh' });
});

test('on success with refetch returning null: the optimistic row stands (not overwritten)', async () => {
  const { rows, queue } = makeStore({ a: { id: 'a', status: 'open' } });
  // override refetch to return null
  queue.refetch = async () => null;
  await queue.enqueue({ id: 'a', patch: { status: 'closed' }, write: async () => 'ok' });
  assert.deepEqual(rows.get('a'), { id: 'a', status: 'closed' });
});

test('on failure: the row is restored to the pre-patch object', async () => {
  const { rows, queue } = makeStore({ a: { id: 'a', status: 'open' } });
  const before = { ...rows.get('a') };
  await assert.rejects(
    queue.enqueue({
      id: 'a',
      patch: { status: 'closed' },
      write: async () => {
        throw new Error('boom');
      },
    }),
  );
  assert.deepEqual(rows.get('a'), before);
});

test('on failure: errorFor(id) is {message, retry} and the promise rejects', async () => {
  const { queue } = makeStore({ a: { id: 'a', status: 'open' } });
  await assert.rejects(
    queue.enqueue({
      id: 'a',
      patch: { status: 'closed' },
      write: async () => {
        throw new Error('write failed');
      },
    }),
    /write failed/,
  );
  const err = queue.errorFor('a');
  assert.equal(err.message, 'write failed');
  assert.equal(typeof err.retry, 'function');
});

test('retry() re-runs the same write', async () => {
  const { rows, queue } = makeStore({ a: { id: 'a', status: 'open' } });
  let attempts = 0;
  await assert.rejects(
    queue.enqueue({
      id: 'a',
      patch: { status: 'closed' },
      write: async () => {
        attempts += 1;
        if (attempts === 1) throw new Error('first fails');
        return 'ok';
      },
    }),
  );
  assert.equal(attempts, 1);
  assert.ok(queue.errorFor('a'));
  const err = queue.errorFor('a');
  await err.retry();
  assert.equal(attempts, 2);
  assert.equal(queue.errorFor('a'), null); // retry re-enqueues, clearing the prior error on success
  assert.deepEqual(rows.get('a'), { id: 'a', status: 'closed' });
});

test('two enqueues for the same id run their writes in order, second starts only after the first settles', async () => {
  const { queue } = makeStore({ a: { id: 'a', status: 'open', n: 0 } });
  const order = [];
  const d1 = defer();
  const p1 = queue.enqueue({
    id: 'a',
    patch: { n: 1 },
    write: async () => {
      order.push('write1-start');
      await d1.promise;
      order.push('write1-end');
    },
  });
  const p2 = queue.enqueue({
    id: 'a',
    patch: { n: 2 },
    write: async () => {
      order.push('write2-start');
    },
  });
  // give write2 a chance to run prematurely if the queue were broken
  await new Promise((r) => setTimeout(r, 10));
  assert.deepEqual(order, ['write1-start']); // write2 must NOT have started yet
  d1.resolve();
  await Promise.all([p1, p2]);
  assert.deepEqual(order, ['write1-start', 'write1-end', 'write2-start']);
});

test('ordering holds even when the first write rejects: second write still starts only after the first settles', async () => {
  const { queue } = makeStore({ a: { id: 'a', status: 'open' } });
  const order = [];
  const d1 = defer();
  const p1 = queue.enqueue({
    id: 'a',
    patch: { status: 'x' },
    write: async () => {
      order.push('write1-start');
      await d1.promise.catch(() => {});
      throw new Error('fail1');
    },
  });
  const p2 = queue.enqueue({
    id: 'a',
    patch: { status: 'y' },
    write: async () => {
      order.push('write2-start');
    },
  });
  await new Promise((r) => setTimeout(r, 10));
  assert.deepEqual(order, ['write1-start']);
  d1.resolve();
  await assert.rejects(p1);
  await p2;
  assert.deepEqual(order, ['write1-start', 'write2-start']);
});

test('enqueues for different ids run concurrently', async () => {
  const { queue } = makeStore({ a: { id: 'a' }, b: { id: 'b' } });
  const order = [];
  const dA = defer();
  const dB = defer();
  const pA = queue.enqueue({
    id: 'a',
    patch: {},
    write: async () => {
      order.push('a-start');
      await dA.promise;
      order.push('a-end');
    },
  });
  const pB = queue.enqueue({
    id: 'b',
    patch: {},
    write: async () => {
      order.push('b-start');
      await dB.promise;
      order.push('b-end');
    },
  });
  await new Promise((r) => setTimeout(r, 10));
  // both should have started even though neither has resolved yet (no cross-id serialization)
  assert.deepEqual(order, ['a-start', 'b-start']);
  dB.resolve();
  await pB;
  dA.resolve();
  await pA;
});

test('onChange fires at least on enqueue and on settle', async () => {
  const { changes, queue } = makeStore({ a: { id: 'a', status: 'open' } });
  const countAtEnqueue = (() => {
    const before = changes.length;
    queue.enqueue({ id: 'a', patch: { status: 'closed' }, write: async () => 'ok' });
    return changes.length - before;
  })();
  assert.ok(countAtEnqueue >= 1); // fired synchronously for the optimistic patch
  await new Promise((r) => setTimeout(r, 10));
  assert.ok(changes.length >= 2); // fired again on settle
});

test('clearError(id) drops the error', async () => {
  const { queue } = makeStore({ a: { id: 'a', status: 'open' } });
  await assert.rejects(
    queue.enqueue({
      id: 'a',
      patch: { status: 'closed' },
      write: async () => {
        throw new Error('nope');
      },
    }),
  );
  assert.ok(queue.errorFor('a'));
  queue.clearError('a');
  assert.equal(queue.errorFor('a'), null);
});

// -- teeth: rollback must actually happen, not merely not-crash --

test('TEETH: a failed write leaves the row exactly as it was before the optimistic patch, not merged with the patch', async () => {
  const { rows, queue } = makeStore({ a: { id: 'a', status: 'open', count: 5 } });
  await assert.rejects(
    queue.enqueue({
      id: 'a',
      patch: { status: 'closed', count: 999 },
      write: async () => {
        throw new Error('server rejected');
      },
    }),
  );
  const row = rows.get('a');
  assert.equal(row.status, 'open');
  assert.equal(row.count, 5);
  assert.notEqual(row.status, 'closed');
  assert.notEqual(row.count, 999);
});
