'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { parseTreeOutput, buildAgentRootNodes, emptyStateNode } = require('../lib/tree-model');
const { dispatchStartArgv, AGENTS } = require('../lib/argv');

function fixture(name) {
  return fs.readFileSync(path.join(__dirname, 'fixtures', name), 'utf8');
}

test('parseTreeOutput: valid JSON with a sessions array returns {sessions}', () => {
  const out = parseTreeOutput(fixture('agent-tree-empty.fixture.json'));
  assert.deepEqual(out, { sessions: [] });
});

test('parseTreeOutput: invalid JSON returns {error}, never throws', () => {
  const out = parseTreeOutput('not json at all');
  assert.ok(out.error);
  assert.equal(out.sessions, undefined);
});

test('parseTreeOutput: valid JSON missing a "sessions" array returns {error}', () => {
  assert.ok(parseTreeOutput('{}').error);
  assert.ok(parseTreeOutput('{"sessions": "nope"}').error);
  assert.ok(parseTreeOutput('null').error);
  assert.ok(parseTreeOutput('[]').error);
});

test('buildAgentRootNodes: empty sessions yields a single empty-state node, not an empty tree', () => {
  const nodes = buildAgentRootNodes({ sessions: [] });
  assert.equal(nodes.length, 1);
  assert.equal(nodes[0].kind, 'empty');
  assert.deepEqual(nodes[0], emptyStateNode());
});

test('buildAgentRootNodes: non-empty sessions produce repo nodes, no empty-state node', () => {
  const data = JSON.parse(fixture('agent-tree.fixture.json'));
  const nodes = buildAgentRootNodes(data);
  assert.ok(nodes.length > 0);
  assert.ok(nodes.every((n) => n.kind === 'repo'));
});

test('dispatchStartArgv: rejects an agent outside the allowlist', () => {
  assert.throws(() => dispatchStartArgv('claude-code', { host: 'h', agent: 'not-a-real-agent', dir: '', task: 't' }), /must be one of/);
});

test('dispatchStartArgv: the allowlist matches the quick-pick list exported as AGENTS', () => {
  for (const agent of AGENTS) {
    assert.doesNotThrow(() => dispatchStartArgv('claude-code', { host: 'h', agent, dir: '', task: 't' }));
  }
});
