'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {
  windowLabelFor,
  formatResetDuration,
  formatResetCountdown,
  buildUsageModel,
  statusBarText,
  statusBarTooltip,
} = require('../lib/usage-model');

function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(__dirname, 'fixtures', name), 'utf8'));
}

// deep-cloned per use so a test that mutates .providers/.billing never leaks into another test.
function subscriptionFixture() {
  return fixture('usage-subscription.fixture.json');
}

const apiFixture = fixture('usage-api.fixture.json');

test('windowLabelFor: known windows abbreviate, unknown/absent fall back to the raw string', () => {
  assert.equal(windowLabelFor('5h'), '5h');
  assert.equal(windowLabelFor('weekly'), 'wk');
  assert.equal(windowLabelFor('monthly'), 'mo');
  assert.equal(windowLabelFor('quarterly'), 'quarterly');
  assert.equal(windowLabelFor(undefined), '');
  assert.equal(windowLabelFor(''), '');
});

test('formatResetDuration: minutes only below an hour', () => {
  assert.equal(formatResetDuration(47 * 60000), '47m');
  assert.equal(formatResetDuration(1 * 60000), '1m');
});

test('formatResetDuration: hours and minutes below a day', () => {
  assert.equal(formatResetDuration(3 * 3600000 + 54 * 60000), '3h 54m');
});

test('formatResetDuration: days and hours at or above a day', () => {
  assert.equal(formatResetDuration(6 * 86400000 + 7 * 3600000), '6d 7h');
});

test('formatResetDuration: zero or negative floors to "now"', () => {
  assert.equal(formatResetDuration(0), 'now');
  assert.equal(formatResetDuration(-5000), 'now');
  assert.equal(formatResetDuration(undefined), 'now');
});

test('formatResetCountdown: wraps the duration, "now" becomes "Resets now"', () => {
  assert.equal(formatResetCountdown(3 * 3600000 + 54 * 60000), 'Resets in 3h 54m');
  assert.equal(formatResetCountdown(0), 'Resets now');
});

test('buildUsageModel: Claude sorts first regardless of providers[] order', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  assert.equal(model.providers[0].id, 'anthropic');
  assert.equal(model.providers[0].name, 'Claude');
  const ids = model.providers.map((p) => p.id);
  assert.deepEqual(new Set(ids), new Set(['anthropic', 'github', 'openai', 'google']));
});

test('buildUsageModel: the three Claude rows become 5h/wk/Fable windows with the right pct and state', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  const claude = model.providers.find((p) => p.id === 'anthropic');
  assert.equal(claude.plan, 'Max');
  assert.equal(claude.windows.length, 3);
  assert.deepEqual(
    claude.windows.map((w) => [w.label, w.usedPct, w.leftPct, w.state]),
    [
      ['5h', 24, 76, 'ok'],
      ['wk', 51, 49, 'ok'],
      ['Fable', 18, 82, 'ok'],
    ]
  );
});

test('buildUsageModel: Antigravity windows label as "<group> · <abbrev window>"', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  const google = model.providers.find((p) => p.id === 'google');
  const labels = google.windows.map((w) => w.label);
  assert.deepEqual(labels, [
    'Gemini Models · wk',
    'Gemini Models · 5h',
    'Claude and GPT models · wk',
    'Claude and GPT models · 5h',
    'Gemini 4 Argon · wk',
  ]);
});

test('buildUsageModel: resetLabel is the countdown to the soonest window, built at call time', (t) => {
  const fiveHReset = Date.parse('2026-10-24T05:29:59Z');
  const now = fiveHReset - 11 * 60 * 1000;
  t.mock.timers.enable({ apis: ['Date'], now });
  const model = buildUsageModel(subscriptionFixture(), {}, now);
  assert.equal(model.providers[0].resetLabel, 'Resets in 11m');
});

test('buildUsageModel: billing absent defaults to subscription mode', () => {
  const json = subscriptionFixture();
  delete json.billing;
  const model = buildUsageModel(json, {}, Date.now());
  assert.equal(model.mode, 'subscription');
});

test('buildUsageModel: billing.mode "api" is honored, carrying the billingProvider through', () => {
  const model = buildUsageModel(apiFixture, {}, Date.now());
  assert.equal(model.mode, 'api');
  assert.equal(model.billingProvider, 'anthropic');
  assert.equal(model.api.length, 1);
  assert.equal(model.api[0].today, 1200000);
});

test('buildUsageModel: two github accounts become two distinct providers, each with its own plan/windows/state', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  const githubs = model.providers.filter((p) => p.id === 'github');
  assert.equal(githubs.length, 2, 'expected two github provider entries, one per account');

  const edotau = githubs.find((p) => p.account === 'edotau');
  assert.ok(edotau, 'expected the edotau account');
  assert.equal(edotau.plan, 'Pro+');
  assert.equal(edotau.windows.length, 1);
  assert.equal(edotau.windows[0].state, 'ok');

  const work = githubs.find((p) => p.account === 'work-account');
  assert.ok(work, 'expected the work-account account');
  assert.equal(work.plan, 'Business');
  assert.equal(work.windows.length, 1);
  assert.equal(work.windows[0].state, 'exceeded');
  assert.equal(work.windows[0].usedPct, 100);
});

test('buildUsageModel: window.usedCredits/limitCredits are the numbers from the row when present, null otherwise', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  const edotau = model.providers.find((p) => p.id === 'github' && p.account === 'edotau');
  assert.equal(edotau.windows[0].usedCredits, 2009);
  assert.equal(edotau.windows[0].limitCredits, 7000);

  const claude = model.providers.find((p) => p.id === 'anthropic');
  for (const w of claude.windows) {
    assert.equal(w.usedCredits, null);
    assert.equal(w.limitCredits, null);
  }
});

test('buildUsageModel: a note-only Claude row (no window) surfaces as provider.note, no windows', () => {
  const json = subscriptionFixture();
  json.providers = json.providers.filter((p) => p.provider !== 'anthropic');
  json.providers.unshift({ provider: 'anthropic', used_percent: 0, left_percent: 100, note: 'not logged in' });
  const model = buildUsageModel(json, {}, Date.now());
  const claude = model.providers.find((p) => p.id === 'anthropic');
  assert.equal(claude.windows.length, 0);
  assert.equal(claude.note, 'not logged in');
});

test('statusBarText: subscription mode, show="used" (default), exact rendering', (t) => {
  const fiveHReset = Date.parse('2026-10-24T05:29:59Z');
  const now = fiveHReset - 11 * 60 * 1000;
  t.mock.timers.enable({ apis: ['Date'], now });
  const model = buildUsageModel(subscriptionFixture(), {}, now);
  assert.equal(statusBarText(model, 'used'), '$(sparkle) 24% 11m · 51% 2d 10h · 18% Fable');
});

test('statusBarText: subscription mode, show="left" swaps in left_percent, keeping the same countdowns', (t) => {
  const fiveHReset = Date.parse('2026-10-24T05:29:59Z');
  const now = fiveHReset - 11 * 60 * 1000;
  t.mock.timers.enable({ apis: ['Date'], now });
  const model = buildUsageModel(subscriptionFixture(), {}, now);
  assert.equal(statusBarText(model, 'left'), '$(sparkle) 76% 11m · 49% 2d 10h · 82% Fable');
});

test('statusBarText: api mode renders "$(key) <provider> <N> tok today" for both show values', () => {
  const model = buildUsageModel(apiFixture, {}, Date.now());
  assert.equal(statusBarText(model, 'used'), '$(key) anthropic 1.2M tok today');
  assert.equal(statusBarText(model, 'left'), '$(key) anthropic 1.2M tok today');
});

test('statusBarText: a missing/note-only Claude row renders "$(sparkle) Claude: <note>"', () => {
  const json = subscriptionFixture();
  json.providers = json.providers.filter((p) => p.provider !== 'anthropic');
  json.providers.unshift({ provider: 'anthropic', used_percent: 0, left_percent: 100, note: 'not logged in' });
  const model = buildUsageModel(json, {}, Date.now());
  assert.equal(statusBarText(model, 'used'), '$(sparkle) Claude: not logged in');
});

test('statusBarText: no Claude row at all and no note falls back to "no data"', () => {
  const json = subscriptionFixture();
  json.providers = json.providers.filter((p) => p.provider !== 'anthropic');
  const model = buildUsageModel(json, {}, Date.now());
  assert.equal(statusBarText(model, 'used'), '$(sparkle) Claude: no data');
});

test('statusBarTooltip: one markdown line per window, naming provider, label, pct and reset countdown', (t) => {
  const fiveHReset = Date.parse('2026-10-24T05:29:59Z');
  const now = fiveHReset - 11 * 60 * 1000;
  t.mock.timers.enable({ apis: ['Date'], now });
  const model = buildUsageModel(subscriptionFixture(), {}, now);
  const tooltip = statusBarTooltip(model, 'used');
  const lines = tooltip.split('  \n');

  assert.match(lines[0], /^Claude 5h .* 24% used · resets in 11m$/);
  assert.match(lines[1], /^Claude wk .* 51% used · resets in 2d 10h$/);
  assert.match(lines[2], /^Claude Fable .* 18% used · resets in 2d 10h$/);
  assert.ok(lines.some((l) => l === '**ChatGPT**: unavailable: codex not signed in to ChatGPT'));
  assert.ok(lines.some((l) => /^Antigravity Gemini Models · wk .* 8% used/.test(l)));
});

test('statusBarTooltip: api mode lists "<provider> <model>: <N> today" per entry', () => {
  const model = buildUsageModel(apiFixture, {}, Date.now());
  const tooltip = statusBarTooltip(model, 'used');
  assert.match(tooltip, /\*\*anthropic usage\*\*/);
  assert.match(tooltip, /anthropic claude-sonnet-5: 1\.2M today/);
});

test('statusBarTooltip: an empty providers array never throws and renders no window lines', () => {
  const model = buildUsageModel({ providers: [] }, {}, Date.now());
  assert.doesNotThrow(() => statusBarTooltip(model, 'used'));
  assert.equal(statusBarTooltip(model, 'used'), '');
});
