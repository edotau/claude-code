'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { formatCount, formatTokens, formatAge } = require('../lib/format');

test('formatCount: plain numbers below 1000', () => {
  assert.equal(formatCount(0), '0');
  assert.equal(formatCount(999), '999');
  assert.equal(formatCount(-5), '-5');
});

test('formatCount: thousands get the "k" suffix, trailing .0 dropped', () => {
  assert.equal(formatCount(1200), '1.2k');
  assert.equal(formatCount(1000), '1k');
});

test('formatCount: millions get the "M" suffix, trailing .0 dropped', () => {
  assert.equal(formatCount(3400000), '3.4M');
  assert.equal(formatCount(1000000), '1M');
});

test('formatCount: non-finite or non-number input reads as 0', () => {
  assert.equal(formatCount(NaN), '0');
  assert.equal(formatCount(Infinity), '0');
  assert.equal(formatCount('5'), '0');
  assert.equal(formatCount(undefined), '0');
});

test('formatTokens: renders "in / out" with real counts', () => {
  assert.equal(formatTokens({ in: 1200, out: 999 }), '1.2k in / 999 out');
});

test('formatTokens: missing fields read as 0', () => {
  assert.equal(formatTokens({}), '0 in / 0 out');
  assert.equal(formatTokens(undefined), '0 in / 0 out');
  assert.equal(formatTokens({ in: 50 }), '50 in / 0 out');
});

test('formatAge: just now for sub-minute deltas', () => {
  const now = Date.parse('2026-10-18T00:00:30Z');
  assert.equal(formatAge('2026-10-18T00:00:00Z', now), 'just now');
});

test('formatAge: minutes, hours, days buckets', () => {
  const base = Date.parse('2026-10-18T00:00:00Z');
  assert.equal(formatAge('2026-10-17T23:55:00Z', base), '5m');
  assert.equal(formatAge('2026-10-17T21:00:00Z', base), '3h');
  assert.equal(formatAge('2026-10-16T00:00:00Z', base), '2d');
});

test('formatAge: a future timestamp clamps to "just now" rather than going negative', () => {
  const base = Date.parse('2026-10-18T00:00:00Z');
  assert.equal(formatAge('2026-10-18T00:05:00Z', base), 'just now');
});

test('formatAge: missing or unparseable input reports "unknown"', () => {
  assert.equal(formatAge(''), 'unknown');
  assert.equal(formatAge(undefined), 'unknown');
  assert.equal(formatAge('not-a-date'), 'unknown');
});
