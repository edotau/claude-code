'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {
  detectJiraIssueKey,
  buildBranchNodes,
  buildPrListNodes,
  statusBarText,
} = require('../lib/tasks-model');

const fixture = JSON.parse(
  fs.readFileSync(path.join(__dirname, 'fixtures', 'bitbucket-pr.fixture.json'), 'utf8')
);

// -- detectJiraIssueKey: ported cases from claude-orcastra's jira-issue-key-detection.test.ts --

test('detectJiraIssueKey: branch wins over title', () => {
  assert.equal(
    detectJiraIssueKey({ branch: 'feature/ABC-1-x', title: 'DIGITALBTD-2 other' }),
    'ABC-1'
  );
});

test('detectJiraIssueKey: falls back to the title when the branch has no key', () => {
  assert.equal(
    detectJiraIssueKey({ branch: 'bugfix/rapidr-scan', title: 'Feature/DIGITALBTD-9679 mmseqs pathogen mimicry' }),
    'DIGITALBTD-9679'
  );
});

test('detectJiraIssueKey: reads the key out of a feature branch', () => {
  assert.equal(
    detectJiraIssueKey({ branch: 'feature/DIGITALBTD-9679-mmseqs-pathogen-mimicry-ai-summary-feature-frontend-eau' }),
    'DIGITALBTD-9679'
  );
});

test('detectJiraIssueKey: strips refs/heads/ before matching', () => {
  assert.equal(detectJiraIssueKey({ branch: 'refs/heads/DIGITALBTD-6318/frontend' }), 'DIGITALBTD-6318');
});

test('detectJiraIssueKey: uppercases a lowercase project', () => {
  assert.equal(detectJiraIssueKey({ branch: 'bugfix/digitalbtd-42-lowercase' }), 'DIGITALBTD-42');
});

test('detectJiraIssueKey: no key-shaped token returns null', () => {
  assert.equal(detectJiraIssueKey({ branch: 'mgl-enhancements-frontend-v2' }), null);
});

for (const branch of ['fix/utf-8-decoding', 'chore/iso-8859-cleanup', 'build/es-2015-target', 'sha-256-hash']) {
  test(`detectJiraIssueKey: rejects the standard-name token in ${branch}`, () => {
    assert.equal(detectJiraIssueKey({ branch }), null);
  });
}

test('detectJiraIssueKey: skips a rejected token and keeps the next key', () => {
  assert.equal(detectJiraIssueKey({ branch: 'fix/utf-8-in-DIGITALBTD-7' }), 'DIGITALBTD-7');
});

test('detectJiraIssueKey: only accepts known projects when provided', () => {
  assert.equal(detectJiraIssueKey({ branch: 'feature/ABC-12-x', knownProjectKeys: ['digitalbtd'] }), null);
  assert.equal(
    detectJiraIssueKey({ branch: 'feature/ABC-12-DIGITALBTD-5', knownProjectKeys: ['digitalbtd'] }),
    'DIGITALBTD-5'
  );
});

test('detectJiraIssueKey: returns null with nothing to read', () => {
  assert.equal(detectJiraIssueKey({}), null);
});

// -- buildBranchNodes --

test('buildBranchNodes: a PR on the branch renders one node carrying its PR details', () => {
  const nodes = buildBranchNodes(fixture, { jiraKey: 'DIGITALBTD-10051', jiraBaseUrl: 'https://jira.example.com' });
  assert.equal(nodes.length, 1);
  const node = nodes[0];
  assert.equal(node.label, '#1662 add excel for gdb mart dl');
  assert.equal(node.icon, 'git-pull-request');
  assert.equal(node.url, fixture[0].url);
  assert.match(node.description, /OPEN/);
  assert.match(node.description, /reviewers 1\/3/);
});

test('buildBranchNodes: PR node children include reviewers, build, and the Jira link', () => {
  const nodes = buildBranchNodes(fixture, { jiraKey: 'DIGITALBTD-10051', jiraBaseUrl: 'https://jira.example.com' });
  const children = nodes[0].children;
  assert.equal(children.filter((c) => c.icon === 'person').length, 3);
  const buildChild = children.find((c) => c.label.startsWith('Build'));
  assert.ok(buildChild, 'expected a Build child node');
  assert.equal(buildChild.icon, 'check');
  const jiraChild = children.find((c) => c.label === 'DIGITALBTD-10051');
  assert.ok(jiraChild, 'expected a Jira link child node');
  assert.equal(jiraChild.url, 'https://jira.example.com/browse/DIGITALBTD-10051');
});

test('buildBranchNodes: no PR renders a placeholder node naming the branch', () => {
  const nodes = buildBranchNodes({ branch: 'DIGITALBTD-10051/frontend', prs: [] }, {});
  assert.equal(nodes.length, 1);
  assert.match(nodes[0].label, /No pull request for DIGITALBTD-10051\/frontend/);
  assert.equal(nodes[0].icon, 'git-branch');
});

test('buildBranchNodes: no PR but a Jira key still appends the Jira link node', () => {
  const nodes = buildBranchNodes(
    { branch: 'DIGITALBTD-10051/frontend', prs: [] },
    { jiraKey: 'DIGITALBTD-10051', jiraBaseUrl: 'https://jira.example.com' }
  );
  assert.equal(nodes.length, 2);
  assert.equal(nodes[1].label, 'DIGITALBTD-10051');
  assert.equal(nodes[1].url, 'https://jira.example.com/browse/DIGITALBTD-10051');
});

// -- buildPrListNodes --

test('buildPrListNodes: sorts rows by updated, newest first', () => {
  const rows = {
    prs: [
      { id: 1, title: 'old', updated: '2026-01-01T00:00:00Z', repo: 'r1', state: 'OPEN' },
      { id: 2, title: 'new', updated: '2026-06-01T00:00:00Z', repo: 'r2', state: 'OPEN' },
      { id: 3, title: 'mid', updated: '2026-03-01T00:00:00Z', repo: 'r3', state: 'OPEN' },
    ],
  };
  const nodes = buildPrListNodes(rows);
  assert.deepEqual(nodes.map((n) => n.label), ['#2 new', '#3 mid', '#1 old']);
});

test('buildPrListNodes: empty input yields no nodes', () => {
  assert.deepEqual(buildPrListNodes({ prs: [] }), []);
  assert.deepEqual(buildPrListNodes({}), []);
});

test('buildPrListNodes: description carries repo, state and the build symbol', () => {
  const nodes = buildPrListNodes(fixture.map((p) => ({ ...p, repo: 'digitalbtd-portal-frontend' })));
  assert.match(nodes[0].description, /digitalbtd-portal-frontend/);
  assert.match(nodes[0].description, /OPEN/);
  assert.match(nodes[0].description, /build ✓/);
});

// -- statusBarText --

test('statusBarText: a PR with a build symbol and reviewer summary', () => {
  const text = statusBarText({ pr: fixture[0] });
  assert.equal(text, '$(git-pull-request) #1662 ✓ · 1/3');
});

test('statusBarText: no PR but a Jira key reads "no PR"', () => {
  assert.equal(statusBarText({ pr: null, jiraKey: 'DIGITALBTD-10051' }), '$(git-pull-request) no PR');
});

test('statusBarText: nothing to show returns the empty string', () => {
  assert.equal(statusBarText({}), '');
  assert.equal(statusBarText(undefined), '');
});
