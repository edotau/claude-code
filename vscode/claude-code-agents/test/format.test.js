'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { formatCount } = require('../lib/format');

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

