'use strict';
// Unit tests for the Tasks page state helpers: presetsFor, getPageNumbers, formatRelativeTime,
// cacheKey and PageCache (TTL, single-flight, force, invalidate).
const test = require('node:test');
const assert = require('node:assert/strict');
const {
  PRESETS,
  presetsFor,
  getPageNumbers,
  formatRelativeTime,
  PageCache,
} = require('../lib/tasks-state');

// -- presetsFor --

test('presetsFor: github returns its 4 presets with exact queries from PRESETS.github', () => {
  const presets = presetsFor('github');
  assert.deepEqual(presets, PRESETS.github);
  assert.deepEqual(
    presets.map((p) => [p.id, p.query]),
    [
      ['assigned', 'is:issue is:open assignee:@me'],
      ['created', 'is:open author:@me'],
      ['review', 'is:pr is:open review-requested:@me'],
      ['myprs', 'is:pr is:open author:@me'],
    ]
  );
});

test('presetsFor: bitbucket returns its 3 presets with exact queries from PRESETS.bitbucket', () => {
  const presets = presetsFor('bitbucket');
  assert.deepEqual(presets, PRESETS.bitbucket);
  assert.deepEqual(
    presets.map((p) => [p.id, p.query]),
    [
      ['review', 'is:pr is:open review-requested:@me'],
      ['myprs', 'is:pr is:open author:@me'],
      ['repo', 'is:pr is:open'],
    ]
  );
});

test('presetsFor: an unknown source returns []', () => {
  assert.deepEqual(presetsFor('unknown-source'), []);
});

// -- getPageNumbers --

test('getPageNumbers: total <= 9 returns every page 0..total-1 in order', () => {
  assert.deepEqual(getPageNumbers(0, 0), []);
  assert.deepEqual(getPageNumbers(0, 1), [0]);
  assert.deepEqual(getPageNumbers(4, 9), [0, 1, 2, 3, 4, 5, 6, 7, 8]);
  assert.deepEqual(getPageNumbers(8, 9), [0, 1, 2, 3, 4, 5, 6, 7, 8]);
});

test('getPageNumbers: total=20 current=0 is the leading window, an ellipsis, then the last page', () => {
  assert.deepEqual(getPageNumbers(0, 20), [0, 1, 2, 'ellipsis', 19]);
});

test('getPageNumbers: total=20 current=10 is a window around 10 with an ellipsis on both sides', () => {
  assert.deepEqual(getPageNumbers(10, 20), [0, 'ellipsis', 8, 9, 10, 11, 12, 'ellipsis', 19]);
});

test('getPageNumbers: total=20 current=19 (last page) has no trailing ellipsis', () => {
  assert.deepEqual(getPageNumbers(19, 20), [0, 'ellipsis', 17, 18, 19]);
});

// -- formatRelativeTime --

const NOW = Date.parse('2026-06-15T00:00:00Z');

test('formatRelativeTime: an unparseable input returns the empty string', () => {
  assert.equal(formatRelativeTime('not-a-date', NOW), '');
});

test('formatRelativeTime: 30 seconds before now is "just now"', () => {
  assert.equal(formatRelativeTime(new Date(NOW - 30 * 1000).toISOString(), NOW), 'just now');
});

test('formatRelativeTime: 5 minutes before now is "5m ago"', () => {
  assert.equal(formatRelativeTime(new Date(NOW - 5 * 60 * 1000).toISOString(), NOW), '5m ago');
});

test('formatRelativeTime: 3 hours before now is "3h ago"', () => {
  assert.equal(formatRelativeTime(new Date(NOW - 3 * 60 * 60 * 1000).toISOString(), NOW), '3h ago');
});

test('formatRelativeTime: 2 days before now is "2d ago"', () => {
  assert.equal(formatRelativeTime(new Date(NOW - 2 * 24 * 60 * 60 * 1000).toISOString(), NOW), '2d ago');
});

test('formatRelativeTime: >=7 days before now in the same year is "Mon D"', () => {
  assert.equal(formatRelativeTime('2026-03-02T00:00:00Z', NOW), 'Mar 2');
});

test('formatRelativeTime: a different year is "Mon D, YYYY"', () => {
  assert.equal(formatRelativeTime('2025-03-02T00:00:00Z', NOW), 'Mar 2, 2025');
});

// -- PageCache --

const PARTS = { source: 'github', repos: ['o/r'], query: 'is:open', page: 0 };

test('PageCache: within TTL a second load() returns the cached value and the loader runs once', async () => {
  const cache = new PageCache({ ttlMs: 120000 });
  let calls = 0;
  const loader = async () => {
    calls += 1;
    return 'page-one';
  };
  const first = await cache.load(PARTS, loader, { nowMs: 1000 });
  const second = await cache.load(PARTS, loader, { nowMs: 5000 });
  assert.equal(first, 'page-one');
  assert.equal(second, 'page-one');
  assert.equal(calls, 1);
});

test('PageCache: after the TTL expires, load() re-invokes the loader', async () => {
  const cache = new PageCache({ ttlMs: 120000 });
  let calls = 0;
  const loader = async () => {
    calls += 1;
    return `page-${calls}`;
  };
  const first = await cache.load(PARTS, loader, { nowMs: 1000 });
  const second = await cache.load(PARTS, loader, { nowMs: 1000 + 120001 });
  assert.equal(first, 'page-1');
  assert.equal(second, 'page-2');
  assert.equal(calls, 2);
});

test('PageCache: concurrent loads of the same key share one in-flight loader call', async () => {
  const cache = new PageCache({ ttlMs: 120000 });
  let calls = 0;
  const loader = async () => {
    calls += 1;
    await new Promise((resolve) => setTimeout(resolve, 20));
    return 'shared-page';
  };
  const p1 = cache.load(PARTS, loader, { nowMs: 1000 });
  const p2 = cache.load(PARTS, loader, { nowMs: 1000 });
  const [r1, r2] = await Promise.all([p1, p2]);
  assert.equal(r1, 'shared-page');
  assert.equal(r2, 'shared-page');
  assert.equal(calls, 1);
});

test('PageCache: force:true bypasses the cache and re-invokes the loader within TTL', async () => {
  const cache = new PageCache({ ttlMs: 120000 });
  let calls = 0;
  const loader = async () => {
    calls += 1;
    return `page-${calls}`;
  };
  await cache.load(PARTS, loader, { nowMs: 1000 });
  const forced = await cache.load(PARTS, loader, { nowMs: 2000, force: true });
  assert.equal(forced, 'page-2');
  assert.equal(calls, 2);
});

test('PageCache: invalidate() clears entries so the next load() re-invokes the loader within TTL', async () => {
  const cache = new PageCache({ ttlMs: 120000 });
  let calls = 0;
  const loader = async () => {
    calls += 1;
    return `page-${calls}`;
  };
  await cache.load(PARTS, loader, { nowMs: 1000 });
  cache.invalidate();
  const after = await cache.load(PARTS, loader, { nowMs: 2000 });
  assert.equal(after, 'page-2');
  assert.equal(calls, 2);
});
