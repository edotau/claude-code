'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { formatCount, formatTokens, formatAge, formatActivity, formatLeg, formatEventLine, formatSnapshotText } = require('../lib/format');

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

test('formatActivity: "tool arg" when both are present', () => {
  assert.equal(formatActivity({ tool: 'Read', arg: '/a/b.go' }), 'Read /a/b.go');
});

test('formatActivity: bare tool name when arg is absent', () => {
  assert.equal(formatActivity({ tool: 'Bash' }), 'Bash');
});

test('formatActivity: falls back to the text blurb when there is no tool', () => {
  assert.equal(formatActivity({ text: 'thinking about the fix' }), 'thinking about the fix');
});

test('formatActivity: absent/empty activity renders as empty string, never throws', () => {
  assert.equal(formatActivity(null), '');
  assert.equal(formatActivity(undefined), '');
  assert.equal(formatActivity({}), '');
});

test('formatLeg: plain running leg, no fallback', () => {
  assert.equal(
    formatLeg({ agent: 'codex', model: 'gpt-6-astra', status: 'running', duration: '2m14s' }),
    '↳ codex gpt-6-astra running 2m14s'
  );
});

test('formatLeg: a fallback leg renders "from→to" and appends the warning marker', () => {
  const line = formatLeg({ agent: 'claude', fallback: 'codex', model: 'opus', status: 'ok', duration: '1m40s' });
  assert.equal(line, '↳ codex→claude opus ok 1m40s ⚠ fallback');
});

test('formatLeg: missing model/status/duration collapse to single spaces, not ragged gaps', () => {
  assert.equal(formatLeg({ agent: 'gemini' }), '↳ gemini');
});

test('formatEventLine: a tool event renders "HH:MM:SS tool Name arg"', () => {
  const line = formatEventLine({ at: '2026-10-18T03:04:05Z', kind: 'tool', tool: 'Read', arg: 'scan.go' });
  assert.match(line, /tool Read scan\.go$/);
});

test('formatEventLine: a text event quotes its blurb', () => {
  const line = formatEventLine({ at: '2026-10-18T03:04:05Z', kind: 'text', text: 'done' });
  assert.match(line, /text "done"$/);
});

test('formatEventLine: a result event renders its byte size', () => {
  const line = formatEventLine({ at: '2026-10-18T03:04:05Z', kind: 'result', size: 12 });
  assert.match(line, /result 12 B$/);
});

test('formatEventLine: an unknown kind degrades to just the timestamp, never throws', () => {
  assert.doesNotThrow(() => formatEventLine({ at: '2026-10-18T03:04:05Z', kind: 'mystery' }));
});

test('formatSnapshotText: renders id/label, status/model/tokens, activity, edited files, legs, events, result', () => {
  const text = formatSnapshotText({
    id: 'fakewfagent002',
    label: 'repair the tracker removal',
    workflow: 'wf_fake0001-abc',
    phase: 'Gate',
    status: 'finished',
    model: 'opus',
    effort: 'high',
    tokens: { in: 1200, out: 300 },
    activity: { tool: 'Edit', arg: 'scan.go' },
    filesEdited: ['internal/agenttree/scan.go'],
    legs: [{ agent: 'codex', model: 'gpt-6-astra', status: 'ok', duration: '2m14s' }],
    events: [{ at: '2026-10-18T03:04:05Z', kind: 'text', text: 'green.' }],
    result: 'green.',
  });
  assert.match(text, /^fakewfagent002 {2}repair the tracker removal/);
  assert.match(text, /workflow wf_fake0001-abc {2}phase Gate/);
  assert.match(text, /status finished {2}model opus effort=high/);
  assert.match(text, /last activity: Edit scan\.go/);
  assert.match(text, / {2}edited internal\/agenttree\/scan\.go/);
  assert.match(text, / {2}↳ codex gpt-6-astra ok 2m14s/);
  assert.match(text, /text "green\."/);
  assert.match(text, /result: green\.$/);
});

test('formatSnapshotText: a bare-minimum snapshot (no workflow/activity/legs/events/result) never throws', () => {
  assert.doesNotThrow(() => formatSnapshotText({ id: 'a1', status: 'running' }));
});
