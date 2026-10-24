'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { buildUsageModel } = require('../lib/usage-model');
const { renderUsageHtml } = require('../lib/usage-view');

function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(__dirname, 'fixtures', name), 'utf8'));
}

// deep-cloned per use so a test that mutates .agents/.window_days never leaks into another test.
function subscriptionFixture() {
  return fixture('usage-subscription.fixture.json');
}

// Fixed "now" the fixture's daily entries (2026-10-20, 10-22, 10-24) were built against: a 7-day window
// ending today spans 10-18..10-24, inclusive.
const NOW_MS = Date.parse('2026-10-24T12:00:00Z');

function sumMixFromFixture(json) {
  const totals = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
  for (const a of json.agents) {
    if (!a.mix) continue;
    totals.input += a.mix.input || 0;
    totals.output += a.mix.output || 0;
    totals.cacheRead += a.mix.cache_read || 0;
    totals.cacheWrite += a.mix.cache_write || 0;
  }
  return totals;
}

test('buildUsageModel: stats.totalTokens/sessions/turns sum every agent', () => {
  const json = subscriptionFixture();
  const model = buildUsageModel(json, {}, NOW_MS);
  const wantTokens = json.agents.reduce((sum, a) => sum + (a.tokens || 0), 0);
  const wantSessions = json.agents.reduce((sum, a) => sum + (a.sessions || 0), 0);
  const wantTurns = json.agents.reduce((sum, a) => sum + (a.turns || 0), 0);
  assert.equal(model.stats.totalTokens, wantTokens);
  assert.equal(model.stats.sessions, wantSessions);
  assert.equal(model.stats.turns, wantTurns);
  assert.equal(model.stats.windowDays, 7);
});

test('buildUsageModel: stats.mix sums every agent\'s mix, and cacheShare is cacheRead/mixTotal', () => {
  const json = subscriptionFixture();
  const model = buildUsageModel(json, {}, NOW_MS);
  const want = sumMixFromFixture(json);
  assert.deepEqual(model.stats.mix, want);
  const mixTotal = want.input + want.output + want.cacheRead + want.cacheWrite;
  assert.ok(Math.abs(model.stats.cacheShare - want.cacheRead / mixTotal) < 1e-9);
});

test('buildUsageModel: stats.daily is filled to every day of the 7-day window, ascending, zero where idle', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, NOW_MS);
  assert.equal(model.stats.daily.length, 7);
  const dates = model.stats.daily.map((d) => d.date);
  assert.deepEqual(dates, ['2026-10-18', '2026-10-19', '2026-10-20', '2026-10-21', '2026-10-22', '2026-10-23', '2026-10-24']);
  const byDate = new Map(model.stats.daily.map((d) => [d.date, d.tokens]));
  assert.equal(byDate.get('2026-10-18'), 0);
  assert.equal(byDate.get('2026-10-19'), 0);
  assert.equal(byDate.get('2026-10-20'), 1701190); // copilot's day
  assert.equal(byDate.get('2026-10-21'), 0);
  assert.equal(byDate.get('2026-10-22'), 320866); // opencode's day
  assert.equal(byDate.get('2026-10-23'), 0);
  assert.equal(byDate.get('2026-10-24'), 25258); // codex's day
  assert.equal(model.stats.activeDays, 3);
});

test('buildUsageModel: stats.bestDay is the day with the most tokens across every agent', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, NOW_MS);
  assert.deepEqual(model.stats.bestDay, { date: '2026-10-20', tokens: 1701190 });
});

test('buildUsageModel: stats.bestDay is null when there is no window and no agent has any daily data', () => {
  const json = { agents: [{ agent: 'codex', sessions: 1, turns: 1, tokens: 500 }] }; // no window_days -> no zero-fill
  const model = buildUsageModel(json, {}, NOW_MS);
  assert.equal(model.stats.bestDay, null);
});

test('buildUsageModel: stats.bestDay is the (zero-token) first window day when the window is known but every day is idle', () => {
  const json = subscriptionFixture();
  for (const a of json.agents) {
    delete a.daily;
    delete a.active_days;
  }
  const model = buildUsageModel(json, {}, NOW_MS);
  assert.deepEqual(model.stats.bestDay, { date: '2026-10-18', tokens: 0 });
  assert.equal(model.stats.activeDays, 0);
});

test('buildUsageModel: stats.costPartial is true when an agent with tokens has no priced model (codex/openai, claude with no model)', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, NOW_MS);
  assert.equal(model.stats.costPartial, true);
});

test('buildUsageModel: stats.costPartial is false when every token-bearing agent is priced (own cost_usd, or a priced Anthropic model)', () => {
  const json = {
    window_days: 1,
    agents: [
      { agent: 'opencode', sessions: 1, turns: 1, tokens: 1000, cost_usd: 0.5, models: [{ provider: 'anthropic', model: 'claude-sonnet-5', turns: 1, tokens: 1000 }] },
      {
        agent: 'claude',
        sessions: 1,
        turns: 1,
        tokens: 2000,
        mix: { input: 1500, output: 400, cache_read: 50, cache_write: 50 },
        models: [{ provider: 'anthropic', model: 'claude-sonnet-5', turns: 1, tokens: 2000 }],
      },
    ],
  };
  const model = buildUsageModel(json, {}, NOW_MS);
  assert.equal(model.stats.costPartial, false);
  assert.ok(model.stats.costUsd > 0);
});

test('renderUsageHtml: detailed mode renders the four stat tile labels', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, NOW_MS);
  const html = renderUsageHtml(model, { detailed: true, show: 'used' });
  for (const label of ['Total tokens', 'Est. cost', 'Sessions', 'Active days']) {
    assert.ok(html.includes(label), `expected the stats tile row to include "${label}"`);
  }
});

test('renderUsageHtml: detailed mode renders one heatmap cell per day of the window', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, NOW_MS);
  const html = renderUsageHtml(model, { detailed: true, show: 'used' });
  const cellMatches = html.match(/class="heat-cell" style/g) || [];
  assert.equal(cellMatches.length, model.stats.daily.length);
  assert.equal(cellMatches.length, 7);
});

test('renderUsageHtml: detailed mode renders the token-mix legend with all four segment labels', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, NOW_MS);
  const html = renderUsageHtml(model, { detailed: true, show: 'used' });
  for (const label of ['Input', 'Output', 'Cache read', 'Cache write']) {
    assert.ok(html.includes(label), `expected the token-mix legend to include "${label}"`);
  }
});

test('renderUsageHtml: compact mode omits the rendered stat tiles (the CSS rule itself is static, always present)', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, NOW_MS);
  const html = renderUsageHtml(model, { detailed: false, show: 'used' });
  assert.ok(!html.includes('class="stat-tile"'));
  assert.ok(!html.includes('Total tokens'));
  assert.ok(!html.includes('Active days'));
});

test('buildUsageModel: an agent with no mix/daily at all (older binary) folds in as zero without throwing', () => {
  const json = { window_days: 1, agents: [{ agent: 'codex', sessions: 1, turns: 1, tokens: 500 }] };
  assert.doesNotThrow(() => buildUsageModel(json, {}, NOW_MS));
  const model = buildUsageModel(json, {}, NOW_MS);
  assert.deepEqual(model.stats.mix, { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 });
  assert.equal(model.stats.cacheShare, 0);
});
