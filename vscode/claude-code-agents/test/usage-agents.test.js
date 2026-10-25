'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { formatCount } = require('../lib/format');
const { buildUsageModel, agentsStatusText, statusBarText, statusBarTooltip } = require('../lib/usage-model');
const { renderUsageHtml } = require('../lib/usage-view');

function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(__dirname, 'fixtures', name), 'utf8'));
}

// deep-cloned per use so a test that mutates .agents never leaks into another test.
function subscriptionFixture() {
  return fixture('usage-subscription.fixture.json');
}

test('buildUsageModel: agents[] carries sessions/turns/tokens/costUsd/topModel/models/note per agent', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  assert.equal(model.agents.length, 4);

  const codex = model.agents.find((a) => a.id === 'codex');
  assert.equal(codex.name, 'Codex');
  assert.equal(codex.sessions, 3);
  assert.equal(codex.turns, 5);
  assert.equal(codex.tokens, 25258);
  assert.equal(codex.costUsd, null);
  assert.equal(codex.topModel, 'openai/gpt-6-astra');
  assert.equal(codex.models.length, 1);
  assert.equal(codex.note, null);

  const opencode = model.agents.find((a) => a.id === 'opencode');
  assert.equal(opencode.name, 'OpenCode');
  assert.equal(opencode.sessions, 6);
  assert.equal(opencode.turns, 97);
  assert.equal(opencode.tokens, 320866);
  assert.equal(opencode.costUsd, 0.08);
  assert.equal(opencode.topModel, 'github-copilot/mai-code-1.1-flash');

  const copilot = model.agents.find((a) => a.id === 'copilot');
  assert.equal(copilot.name, 'Copilot CLI');
  assert.equal(copilot.sessions, 2);
  assert.equal(copilot.tokens, 1701190);
  assert.equal(copilot.topModel, 'github-copilot/claude-opus-5.5');

  const claude = model.agents.find((a) => a.id === 'claude');
  assert.equal(claude.name, 'Claude');
  assert.equal(claude.tokens, 4200000000);
  assert.equal(claude.sessions, 0);
  assert.equal(claude.topModel, null);
  assert.deepEqual(claude.models, []);
});

test('buildUsageModel: agents absent from usage JSON yields an empty array, never throws', () => {
  const json = subscriptionFixture();
  delete json.agents;
  const model = buildUsageModel(json, {}, Date.now());
  assert.deepEqual(model.agents, []);
});

test('agentsStatusText: excludes claude and joins the rest as "id formatCount(tokens)"', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  const want =
    ' · codex ' +
    formatCount(25258) +
    ' · opencode ' +
    formatCount(320866) +
    ' · copilot ' +
    formatCount(1701190);
  assert.equal(agentsStatusText(model), want);
});

test('agentsStatusText: a zero-token agent is excluded from the chip list', () => {
  const json = subscriptionFixture();
  json.agents.push({ agent: 'codex2', tokens: 0, sessions: 1 });
  const model = buildUsageModel(json, {}, Date.now());
  assert.ok(!agentsStatusText(model).includes('codex2'));
});

test('agentsStatusText: no non-claude agents with tokens>0 renders the empty string', () => {
  const model = buildUsageModel({ agents: [{ agent: 'claude', tokens: 4200000000 }] }, {}, Date.now());
  assert.equal(agentsStatusText(model), '');
});

test('agentsStatusText: no agents key at all renders the empty string, never throws', () => {
  const model = buildUsageModel({}, {}, Date.now());
  assert.doesNotThrow(() => agentsStatusText(model));
  assert.equal(agentsStatusText(model), '');
});

test('statusBarText: opts.agents appends the agent chip suffix to the subscription-mode text', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  const base = statusBarText(model, 'used');
  const withAgents = statusBarText(model, 'used', { agents: true });
  assert.equal(withAgents, base + agentsStatusText(model));
  assert.equal(statusBarText(model, 'used', { agents: false }), base);
});

test('statusBarTooltip: one markdown line per agent, naming the agent, its top model, tokens and sessions', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  const tooltip = statusBarTooltip(model, 'used');
  const lines = tooltip.split('  \n');

  const codexLine = lines.find((l) => l.startsWith('**Codex**'));
  assert.ok(codexLine, 'expected a **Codex** line in the tooltip');
  assert.match(codexLine, /openai\/gpt-6-astra/);
  assert.match(codexLine, new RegExp(formatCount(25258).replace('.', '\\.') + ' tokens'));
  assert.match(codexLine, /3 sessions/);

  const opencodeLine = lines.find((l) => l.startsWith('**OpenCode**'));
  assert.ok(opencodeLine, 'expected an **OpenCode** line in the tooltip');
  assert.match(opencodeLine, /\$0\.08/);

  const copilotLine = lines.find((l) => l.startsWith('**Copilot CLI**'));
  assert.ok(copilotLine, 'expected a **Copilot CLI** line in the tooltip');

  const claudeLine = lines.find((l) => l.startsWith('**Claude**'));
  assert.ok(claudeLine, 'expected a **Claude** line in the tooltip (tooltip, unlike the status chip, is not claude-exclusive)');
});

test('statusBarTooltip: no cost on an agent omits the "$" cost fragment', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  const tooltip = statusBarTooltip(model, 'used');
  const codexLine = tooltip.split('  \n').find((l) => l.startsWith('**Codex**'));
  assert.ok(!codexLine.includes('$'));
});

test('renderUsageHtml: an Agents section lists each agent name with its "N sessions" fragment', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  const html = renderUsageHtml(model, { detailed: true, show: 'used' });
  assert.match(html, /Agents/);
  for (const name of ['Codex', 'OpenCode', 'Copilot CLI']) {
    assert.ok(html.includes(name), `expected the Agents section to name ${name}`);
  }
  assert.ok(html.includes('6 sessions'), 'expected the OpenCode row\'s session count in the Agents section');
});

test('renderUsageHtml: one .agent-model-row per models[] entry, carrying its cost when present', () => {
  const model = buildUsageModel(subscriptionFixture(), {}, Date.now());
  const html = renderUsageHtml(model, { detailed: true, show: 'used' });
  const rows = [...html.matchAll(/<div class="agent-model-row">/g)];
  // one models[] row per agent that has one: codex, opencode, copilot (claude has none in the fixture).
  assert.equal(rows.length, 3, 'expected one .agent-model-row per priced agent');
  assert.ok(html.includes('github-copilot/mai-code-1.1-flash'), 'expected the opencode model label');
  assert.ok(/\$0\.08/.test(html), 'expected the opencode model row to carry its cost');
  assert.ok(html.includes('openai/gpt-6-astra'), 'expected the codex model label');
});

test('renderUsageHtml: an agent with an empty models[] array renders no .agent-models block', () => {
  const json = subscriptionFixture();
  const claude = json.agents.find((a) => a.agent === 'claude');
  claude.tokens = 10; // keep it non-muted (tokens>0) so renderAgentCard takes the normal branch
  // claude has no `models` key -> buildAgent folds it to models: []; this is the empty-models case under test.
  const model = buildUsageModel(json, {}, Date.now());
  const html = renderUsageHtml(model, { detailed: true, show: 'used' });
  const claudeIdx = html.indexOf('<div class="agent-name card-head">Claude</div>');
  assert.ok(claudeIdx !== -1, 'expected a Claude agent-card');
  const nextCardIdx = html.indexOf('<div class="agent-card">', claudeIdx + 1);
  const claudeCard = html.slice(claudeIdx, nextCardIdx === -1 ? html.length : nextCardIdx);
  assert.ok(!claudeCard.includes('class="agent-models"'), 'an agent with no models[] must render no .agent-models block');
});

test('renderUsageHtml: no agents array produces no Agents section and never throws', () => {
  const json = subscriptionFixture();
  delete json.agents;
  const model = buildUsageModel(json, {}, Date.now());
  assert.doesNotThrow(() => renderUsageHtml(model, { detailed: true, show: 'used' }));
  const html = renderUsageHtml(model, { detailed: true, show: 'used' });
  // the page header itself now reads "Agents Dashboard" unconditionally; what must be absent is the
  // agents SECTION (the per-agent cards block), not the literal substring "Agents" anywhere on the page.
  assert.ok(!html.includes('class="agents-section"'));
});
