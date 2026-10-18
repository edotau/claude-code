'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {
  buildAgentTree,
  countRunning,
  buildDispatchNodes,
  nestByDepth,
  iconForStatus,
} = require('../lib/tree-model');

function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(__dirname, 'fixtures', name), 'utf8'));
}

const treeFixture = fixture('agent-tree.fixture.json');
const emptyFixture = fixture('agent-tree-empty.fixture.json');
const NOW = Date.parse('2026-10-18T07:00:00-04:00');

test('iconForStatus: running/active -> spinner, finished -> check, stale/idle -> warning, else null', () => {
  assert.equal(iconForStatus('running'), 'sync~spin');
  assert.equal(iconForStatus('active'), 'sync~spin');
  assert.equal(iconForStatus('finished'), 'check');
  assert.equal(iconForStatus('stale'), 'warning');
  assert.equal(iconForStatus('idle'), 'warning');
  assert.equal(iconForStatus('unknown-status'), null);
  assert.equal(iconForStatus(undefined), null);
});

test('nestByDepth: flat depth-1 list stays flat', () => {
  const nested = nestByDepth([{ id: 'a', depth: 1 }, { id: 'b', depth: 1 }]);
  assert.equal(nested.length, 2);
  assert.deepEqual(nested.map((n) => n.id), ['a', 'b']);
  assert.deepEqual(nested[0].children, []);
});

test('nestByDepth: a depth-2 item nests under the preceding depth-1 item', () => {
  const nested = nestByDepth([
    { id: 'parent', depth: 1 },
    { id: 'child', depth: 2 },
  ]);
  assert.equal(nested.length, 1);
  assert.equal(nested[0].id, 'parent');
  assert.equal(nested[0].children.length, 1);
  assert.equal(nested[0].children[0].id, 'child');
});

test('nestByDepth: depth drops back out to a sibling, not a cousin', () => {
  const nested = nestByDepth([
    { id: 'a', depth: 1 },
    { id: 'a.1', depth: 2 },
    { id: 'a.1.1', depth: 3 },
    { id: 'b', depth: 1 },
  ]);
  assert.equal(nested.length, 2);
  assert.equal(nested[0].id, 'a');
  assert.equal(nested[0].children[0].id, 'a.1');
  assert.equal(nested[0].children[0].children[0].id, 'a.1.1');
  assert.equal(nested[1].id, 'b');
  assert.deepEqual(nested[1].children, []);
});

test('nestByDepth: missing depth defaults to 1', () => {
  const nested = nestByDepth([{ id: 'a' }, { id: 'b' }]);
  assert.equal(nested.length, 2);
});

test('buildAgentTree: groups sessions by the basename of cwd', () => {
  const repos = buildAgentTree(treeFixture, { now: NOW });
  const names = repos.map((r) => r.id).sort();
  assert.deepEqual(names, ['repo-alpha', 'repo-beta']);
  const alpha = repos.find((r) => r.id === 'repo-alpha');
  const beta = repos.find((r) => r.id === 'repo-beta');
  assert.equal(alpha.children.length, 3);
  assert.equal(beta.children.length, 2);
});

test('buildAgentTree: a session node nests its subagents by depth', () => {
  const repos = buildAgentTree(treeFixture, { now: NOW });
  const alpha = repos.find((r) => r.id === 'repo-alpha');
  const active = alpha.children.find((s) => s.id === '31a2f0a5-36b4-41c5-9e1d-8d2ff875b284');
  assert.ok(active);
  assert.equal(active.kind, 'session');
  // the fixture's subagents are all depth 1, so they stay flat under the session.
  assert.equal(active.children.filter((c) => c.kind === 'subagent').length, 27);
});

// A minimal hand-built session (not the shared fixture) isolating one relay subagent with an
// Activity and two vendor Legs, so assertions here don't depend on the big fixture's shape.
function sessionWithLegsAndActivity() {
  return {
    sessions: [
      {
        id: 'sess-legs-1',
        title: 'repo-legs',
        cwd: '/Users/fake/repo-legs',
        status: 'active',
        updated: '2026-10-18T06:59:00-04:00',
        subagents: [
          {
            id: 'relay-1',
            type: 'codex',
            description: 'review the change',
            status: 'running',
            depth: 1,
            tokens: { in: 10, out: 5 },
            updated: '2026-10-18T06:58:00-04:00',
            lastTool: 'Bash',
            activity: { tool: 'Bash', arg: 'go test ./...' },
            legs: [
              { id: 'leg-a', agent: 'codex', model: 'gpt-6-astra', status: 'running', duration: '2m14s' },
              {
                id: 'leg-b', agent: 'claude', fallback: 'codex', model: 'opus', status: 'ok',
                duration: '1m40s', fallbackReason: 'rate limited',
              },
            ],
          },
        ],
      },
    ],
  };
}

test('buildAgentTree: a subagent node carries its activity in the description', () => {
  const repos = buildAgentTree(sessionWithLegsAndActivity(), { now: NOW });
  const relay = repos[0].children[0].children.find((c) => c.kind === 'subagent' && c.id === 'relay-1');
  assert.ok(relay, 'relay-1 subagent node not found');
  assert.match(relay.description, /Bash go test \.\/\.\.\.$/);
});

test('buildAgentTree: a subagent\'s legs become leg children, indented ahead of any nested subagents', () => {
  const repos = buildAgentTree(sessionWithLegsAndActivity(), { now: NOW });
  const relay = repos[0].children[0].children.find((c) => c.kind === 'subagent' && c.id === 'relay-1');
  const legs = relay.children.filter((c) => c.kind === 'leg');
  assert.equal(legs.length, 2);
  assert.equal(legs[0].id, 'relay-1:leg:leg-a');
  assert.match(legs[0].label, /^↳ codex gpt-6-astra running 2m14s$/);
  assert.equal(legs[0].icon, 'link');
});

test('buildAgentTree: a fallback leg gets the warning icon and surfaces fallbackReason as its tooltip', () => {
  const repos = buildAgentTree(sessionWithLegsAndActivity(), { now: NOW });
  const relay = repos[0].children[0].children.find((c) => c.kind === 'subagent' && c.id === 'relay-1');
  const fallbackLeg = relay.children.find((c) => c.kind === 'leg' && c.id === 'relay-1:leg:leg-b');
  assert.ok(fallbackLeg);
  assert.match(fallbackLeg.label, /⚠ fallback$/);
  assert.equal(fallbackLeg.icon, 'warning');
  assert.equal(fallbackLeg.tooltip, 'rate limited');
});

test('buildAgentTree: a subagent with no activity/legs has an unmodified description and no leg children', () => {
  const data = sessionWithLegsAndActivity();
  delete data.sessions[0].subagents[0].activity;
  delete data.sessions[0].subagents[0].legs;
  const repos = buildAgentTree(data, { now: NOW });
  const relay = repos[0].children[0].children.find((c) => c.kind === 'subagent' && c.id === 'relay-1');
  assert.equal(relay.children.filter((c) => c.kind === 'leg').length, 0);
  assert.doesNotMatch(relay.description, / · $/);
});

test('buildAgentTree: workflow phases become phase nodes nested under a workflow node', () => {
  const repos = buildAgentTree(treeFixture, { now: NOW });
  const beta = repos.find((r) => r.id === 'repo-beta');
  const withWorkflow = beta.children.find((s) => (s.children || []).some((c) => c.kind === 'workflow'));
  assert.ok(withWorkflow);
  const wf = withWorkflow.children.find((c) => c.kind === 'workflow');
  assert.ok(wf.children.length > 0);
  for (const phase of wf.children) {
    assert.equal(phase.kind, 'phase');
  }
});

test('buildAgentTree: empty sessions produces no repo nodes (empty-state source data)', () => {
  const repos = buildAgentTree(emptyFixture, { now: NOW });
  assert.deepEqual(repos, []);
});

test('buildAgentTree: missing/null data does not throw and yields an empty tree', () => {
  assert.deepEqual(buildAgentTree(null), []);
  assert.deepEqual(buildAgentTree(undefined), []);
  assert.deepEqual(buildAgentTree({}), []);
});

test('countRunning: counts active sessions plus running subagents and workflow agents', () => {
  const n = countRunning(treeFixture);
  // 1 active session + however many depth-nested subagents/workflow agents report status "running".
  assert.ok(n >= 1, `expected at least the 1 active session, got ${n}`);
});

test('countRunning: an empty/malformed data object counts zero, never throws', () => {
  assert.equal(countRunning(emptyFixture), 0);
  assert.equal(countRunning(null), 0);
  assert.equal(countRunning({}), 0);
});

test('buildDispatchNodes: a host with sessions maps Attached/CurrentCmd into the node description', () => {
  const dispatchFixture = fixture('dispatch-list.fixture.json');
  const nodes = buildDispatchNodes([{ host: 'myhost', sessions: dispatchFixture }]);
  assert.equal(nodes.length, 1);
  assert.equal(nodes[0].label, 'myhost');
  assert.equal(nodes[0].children.length, 2);
  const attached = nodes[0].children.find((c) => c.session === 'cc-claude-a1b2');
  assert.equal(attached.icon, 'radio-tower');
  assert.match(attached.description, /^attached/);
  const detached = nodes[0].children.find((c) => c.session === 'cc-codex-9f3d');
  assert.equal(detached.icon, 'circle-outline');
  assert.match(detached.description, /^detached/);
});

test('buildDispatchNodes: a host error produces an error child node, not a thrown exception', () => {
  const nodes = buildDispatchNodes([{ host: 'badhost', error: 'ssh: connection refused' }]);
  assert.equal(nodes[0].description, 'error');
  assert.equal(nodes[0].children[0].kind, 'error');
  assert.equal(nodes[0].children[0].label, 'ssh: connection refused');
});

test('buildDispatchNodes: no hosts/empty input yields no nodes', () => {
  assert.deepEqual(buildDispatchNodes([]), []);
  assert.deepEqual(buildDispatchNodes(undefined), []);
});
