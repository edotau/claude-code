'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { detectJiraIssueKey } = require('../lib/tasks-model');

// -- detectJiraIssueKey: ported cases from claude-orcastra's jira-issue-key-detection.test.ts --

test('detectJiraIssueKey: branch wins over title', () => {
  assert.equal(
    detectJiraIssueKey({ branch: 'feature/ABC-1-x', title: 'ACME-2 other' }),
    'ABC-1'
  );
});

test('detectJiraIssueKey: falls back to the title when the branch has no key', () => {
  assert.equal(
    detectJiraIssueKey({ branch: 'bugfix/rapidr-scan', title: 'Feature/ACME-9679 mmseqs pathogen mimicry' }),
    'ACME-9679'
  );
});

test('detectJiraIssueKey: reads the key out of a feature branch', () => {
  assert.equal(
    detectJiraIssueKey({ branch: 'feature/ACME-9679-mmseqs-pathogen-mimicry-ai-summary-feature-frontend-eau' }),
    'ACME-9679'
  );
});

test('detectJiraIssueKey: strips refs/heads/ before matching', () => {
  assert.equal(detectJiraIssueKey({ branch: 'refs/heads/ACME-6318/frontend' }), 'ACME-6318');
});

test('detectJiraIssueKey: uppercases a lowercase project', () => {
  assert.equal(detectJiraIssueKey({ branch: 'bugfix/acme-42-lowercase' }), 'ACME-42');
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
  assert.equal(detectJiraIssueKey({ branch: 'fix/utf-8-in-ACME-7' }), 'ACME-7');
});

test('detectJiraIssueKey: only accepts known projects when provided', () => {
  assert.equal(detectJiraIssueKey({ branch: 'feature/ABC-12-x', knownProjectKeys: ['acme'] }), null);
  assert.equal(
    detectJiraIssueKey({ branch: 'feature/ABC-12-ACME-5', knownProjectKeys: ['acme'] }),
    'ACME-5'
  );
});

test('detectJiraIssueKey: returns null with nothing to read', () => {
  assert.equal(detectJiraIssueKey({}), null);
});
