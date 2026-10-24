'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { buildUsageModel } = require('../lib/usage-model');
const { pivotGroupedWindows, renderUsageHtml } = require('../lib/usage-view');

function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(__dirname, 'fixtures', name), 'utf8'));
}

// deep-cloned per use so a test that mutates .providers never leaks into another test.
function subscriptionFixture() {
  return fixture('usage-subscription.fixture.json');
}

function packageJson() {
  return JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'package.json'), 'utf8'));
}

function countOccurrences(haystack, needle) {
  return haystack.split(needle).length - 1;
}

function buildFrozenModel(t) {
  const fiveHReset = Date.parse('2026-10-24T05:29:59Z');
  const now = fiveHReset - 11 * 60 * 1000;
  t.mock.timers.enable({ apis: ['Date'], now });
  return buildUsageModel(subscriptionFixture(), {}, now);
}

test('pivotGroupedWindows: Antigravity google windows pivot into shared 5h/wk columns, one row per group', (t) => {
  const model = buildFrozenModel(t);
  const google = model.providers.find((p) => p.id === 'google');
  const pivot = pivotGroupedWindows(google.windows);

  // columns in order of first appearance of each window label across the row-ordered windows
  assert.deepEqual(pivot.columns, ['wk', '5h']);

  const groupNames = pivot.rows.map((r) => r.group);
  assert.deepEqual(groupNames, ['Gemini Models', 'Claude and GPT models', 'Gemini 4 Argon']);
});

test('pivotGroupedWindows: a group missing a window (Gemini 4 Argon has no 5h row) gets a null cell', (t) => {
  const model = buildFrozenModel(t);
  const google = model.providers.find((p) => p.id === 'google');
  const pivot = pivotGroupedWindows(google.windows);

  const argon = pivot.rows.find((r) => r.group === 'Gemini 4 Argon');
  assert.ok(argon, 'Gemini 4 Argon row must exist');
  assert.equal(argon.cells['5h'], null);
  assert.ok(argon.cells.wk, 'the weekly cell for Gemini 4 Argon must be the real window, not null');
  assert.equal(argon.cells.wk.usedPct, 46);
});

test('renderUsageHtml: the grouped Antigravity provider shows each group name once and exactly one "—" placeholder', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });

  assert.equal(countOccurrences(html, 'Gemini Models'), 1);
  assert.equal(countOccurrences(html, 'Claude and GPT models'), 1);
  assert.equal(countOccurrences(html, 'Gemini 4 Argon'), 1);
  assert.equal(countOccurrences(html, '—'), 1, 'exactly one em-dash placeholder for the missing Gemini 4 Argon 5h cell');
});

test('renderUsageHtml: Claude (ungrouped) still renders 5h, wk and Fable rows', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });

  assert.ok(html.includes('5h'), 'expected a 5h row for Claude');
  assert.ok(html.includes('wk'), 'expected a wk row for Claude');
  assert.ok(html.includes('Fable'), 'expected a Fable row for Claude');
});

test('package.json: claudeCodeAgents.openUsageEditor command exists with the pulse icon', () => {
  const pkg = packageJson();
  const cmd = (pkg.contributes.commands || []).find((c) => c.command === 'claudeCodeAgents.openUsageEditor');
  assert.ok(cmd, 'claudeCodeAgents.openUsageEditor command must be declared');
  assert.equal(cmd.icon, '$(pulse)');
});

test('package.json: openUsageEditor appears in editor/title with group navigation, gated on usageEditorTitleButton', () => {
  const pkg = packageJson();
  const entries = pkg.contributes.menus['editor/title'] || [];
  const entry = entries.find((e) => e.command === 'claudeCodeAgents.openUsageEditor');
  assert.ok(entry, 'editor/title must contain an entry for claudeCodeAgents.openUsageEditor');
  assert.equal(entry.group, 'navigation');
  assert.equal(entry.when, 'config.claudeCodeAgents.usageEditorTitleButton');
});

test('package.json: openUsageEditor appears in view/title for the claudeCodeAgents.usage view', () => {
  const pkg = packageJson();
  const entries = pkg.contributes.menus['view/title'] || [];
  const entry = entries.find((e) => e.command === 'claudeCodeAgents.openUsageEditor');
  assert.ok(entry, 'view/title must contain an entry for claudeCodeAgents.openUsageEditor');
  assert.equal(entry.when, 'view == claudeCodeAgents.usage');
});

test('package.json: usageOpenIn setting is a sidebar|editor enum defaulting to sidebar', () => {
  const pkg = packageJson();
  const prop = pkg.contributes.configuration.properties['claudeCodeAgents.usageOpenIn'];
  assert.ok(prop, 'claudeCodeAgents.usageOpenIn must be declared');
  assert.equal(prop.type, 'string');
  assert.deepEqual(prop.enum, ['sidebar', 'editor']);
  assert.equal(prop.default, 'sidebar');
});

test('package.json: usageEditorTitleButton setting is a boolean defaulting to true', () => {
  const pkg = packageJson();
  const prop = pkg.contributes.configuration.properties['claudeCodeAgents.usageEditorTitleButton'];
  assert.ok(prop, 'claudeCodeAgents.usageEditorTitleButton must be declared');
  assert.equal(prop.type, 'boolean');
  assert.equal(prop.default, true);
});

// Pending restyle (section headers, title attrs, nowrap/ellipsis): see vscode/claude-code-agents/lib/usage-view.js.

test('renderUsageHtml (detailed): shows OVERVIEW, QUOTAS, AGENTS section headers in document order', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });

  const headers = ['OVERVIEW', 'QUOTAS', 'AGENTS'];
  const indices = headers.map((h) => html.indexOf(h));
  indices.forEach((idx, i) => assert.ok(idx !== -1, `expected header "${headers[i]}" in detailed html`));
  for (let i = 1; i < indices.length; i++) {
    assert.ok(indices[i] > indices[i - 1], `expected "${headers[i]}" to follow "${headers[i - 1]}" in document order`);
  }
});

test('renderUsageHtml (detailed): AGENTS header carries "· last 7 days" from stats.windowDays (fixture window_days: 7)', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });

  assert.ok(html.includes('AGENTS · last 7 days'), 'expected "AGENTS · last 7 days" in the html');
});

test('renderUsageHtml (compact): still shows all three section headers but no stat-tile class', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: false, show: 'used', nonce: 'n', cspSource: 'c' });

  for (const h of ['OVERVIEW', 'QUOTAS', 'AGENTS']) {
    assert.ok(html.includes(h), `expected header "${h}" in compact html`);
  }
  assert.ok(!html.includes('class="stat-tile"'), 'compact mode must not render stat-tile elements');
});

test('renderUsageHtml (detailed): every .agent-stats element carries a title attribute equal to its visible text', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });

  const re = /<div class="agent-stats[^"]*" title="([^"]*)">([^<]*)<\/div>/g;
  const matches = [...html.matchAll(re)];
  assert.ok(matches.length > 0, 'expected at least one .agent-stats element with a title attribute');
  for (const m of matches) {
    const [, title, text] = m;
    assert.equal(title, text, 'title attribute must equal the visible text');
  }
});

test('renderUsageHtml (detailed): no <h1> text other than "Agents Dashboard"', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });

  const h1s = [...html.matchAll(/<h1[^>]*>([^<]*)<\/h1>/g)].map((m) => m[1]);
  assert.ok(h1s.length > 0, 'expected at least one <h1>');
  for (const text of h1s) assert.equal(text, 'Agents Dashboard');
});

// Splits the html into one slice per top-level .provider-card sibling inside .quotas-grid, in document order.
function providerCardSlices(html) {
  const gridOpen = html.indexOf('<div class="quotas-grid">');
  const gridBodyStart = gridOpen + '<div class="quotas-grid">'.length;
  const nextHeader = html.indexOf('<div class="section-header">', gridBodyStart);
  const grid = html.slice(gridBodyStart, nextHeader);
  const opens = [...grid.matchAll(/<div class="provider-card[^"]*">/g)];
  return opens.map((m, i) => grid.slice(m.index, i + 1 < opens.length ? opens[i + 1].index : grid.length));
}

test('renderUsageHtml (detailed): exactly one .provider-card per provider, all inside .quotas-grid', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });

  assert.ok(html.includes('<div class="quotas-grid">'), 'expected a .quotas-grid container');
  const cards = providerCardSlices(html);
  assert.equal(cards.length, model.providers.length, 'expected one .provider-card per provider in the model');
});

test('renderUsageHtml (detailed): the Claude card shows its 5h/wk/Fable window labels; no card shows "›"', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });

  const cards = providerCardSlices(html);
  const claudeCard = cards.find((c) => c.includes('>Claude<'));
  assert.ok(claudeCard, 'expected a Claude provider card');
  assert.ok(claudeCard.includes('5h'), 'expected a 5h label in the Claude card');
  assert.ok(claudeCard.includes('wk'), 'expected a wk label in the Claude card');
  assert.ok(claudeCard.includes('Fable'), 'expected a Fable label in the Claude card');

  for (const card of cards) {
    assert.ok(!card.includes('›'), 'no provider-card may contain the "›" chevron');
  }
});

test('renderUsageHtml (detailed): the Antigravity card contains its group names', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });

  const cards = providerCardSlices(html);
  const antigravityCard = cards.find((c) => c.includes('>Antigravity<'));
  assert.ok(antigravityCard, 'expected an Antigravity provider card');
  for (const group of ['Gemini Models', 'Claude and GPT models', 'Gemini 4 Argon']) {
    assert.ok(antigravityCard.includes(group), `expected group "${group}" inside the Antigravity card`);
  }
});

test('renderUsageHtml (compact): provider cards still render but use .chip, never .window-row', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: false, show: 'used', nonce: 'n', cspSource: 'c' });

  const cards = providerCardSlices(html);
  assert.equal(cards.length, model.providers.length, 'expected one .provider-card per provider in compact mode');
  for (const card of cards) {
    if (card.includes('provider-card-muted')) continue; // note-only card has neither chip nor window-row
    assert.ok(card.includes('class="chip'), 'expected .chip elements inside a compact provider card');
  }
  assert.ok(!html.includes('class="window-row'), 'compact mode must not render .window-row elements');
});

test('renderUsageHtml (detailed): no MORE section, no #accounts row', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });

  assert.ok(!html.includes('MORE'), 'the MORE section must be removed');
  assert.ok(!html.includes('id="accounts"'), 'the #accounts row must be removed');
});

test('renderUsageHtml: the QUOTAS toggle reads "Usage"/"Remaining", never "Used"/"Left"', (t) => {
  const model = buildFrozenModel(t);
  const html = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });

  assert.ok(html.includes('>Usage<'), 'expected the toggle label "Usage"');
  assert.ok(html.includes('>Remaining<'), 'expected the toggle label "Remaining"');
  assert.ok(!html.includes('>Used<'), 'the old "Used" label must be gone');
  assert.ok(!html.includes('>Left<'), 'the old "Left" label must be gone');
});

test('renderUsageHtml: the QUOTAS header carries a Used/Left toggle reflecting opts.show', () => {
  const usage = JSON.parse(fs.readFileSync(path.join(__dirname, 'fixtures', 'usage-subscription.fixture.json'), 'utf8'));
  const model = buildUsageModel(usage, {}, Date.now());
  const used = renderUsageHtml(model, { detailed: true, show: 'used', nonce: 'n', cspSource: 'c' });
  const left = renderUsageHtml(model, { detailed: true, show: 'left', nonce: 'n', cspSource: 'c' });
  assert.ok(used.includes('id="show-used" class="active"') && used.includes('>Usage<'));
  assert.ok(used.includes('id="show-left" class=""') && used.includes('>Remaining<'));
  assert.match(left, /id="show-left" class="active"/);
  assert.ok(left.includes("postMessage({ type: 'show', show: mode })"));
  // Claude 5h: 24% used in the fixture, so Left renders 76%.
  assert.ok(used.includes('>24%<') && left.includes('>76%<'));
});
