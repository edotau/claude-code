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
