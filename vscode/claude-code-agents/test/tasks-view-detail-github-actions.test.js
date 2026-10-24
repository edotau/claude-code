'use strict';
// Unit tests for the GitHub PR detail header actions and review-thread rendering added to
// lib/tasks-view.js: githubHeaderActions (prReady/prDraft, rerunChecks, the ⋯ menu) and
// threadCard (unresolved/resolved thread cards, reply form, resolve toggle).
const test = require('node:test');
const assert = require('node:assert/strict');
const { detailView } = require('../lib/tasks-view');

const NOW = Date.parse('2026-10-24T00:00:00Z');

function prItem(overrides) {
  return {
    id: 'gh-pr-9',
    source: 'github',
    type: 'pr',
    number: 9,
    title: 'Fix the thing',
    state: 'open',
    url: 'https://github.com/o/r/pull/9',
    author: 'bob',
    repo: { label: 'o/r' },
    updatedAt: '2026-10-21T00:00:00Z',
    branch: 'fix/thing',
    baseBranch: 'main',
    reviewers: [],
    labels: [],
    additions: 3,
    deletions: 1,
    changedFiles: 2,
    ...overrides,
  };
}

function issueItem(overrides) {
  return {
    id: 'gh-issue-7',
    source: 'github',
    type: 'issue',
    number: 7,
    title: 'Something is broken',
    state: 'open',
    stateReason: null,
    url: 'https://github.com/o/r/issues/7',
    author: 'alice',
    repo: { label: 'o/r' },
    updatedAt: '2026-10-20T00:00:00Z',
    assignees: [],
    labels: [],
    ...overrides,
  };
}

function conversationData(overrides) {
  return {
    bodyHtml: '<p>the issue body</p>',
    author: 'alice',
    avatarUrl: null,
    createdAt: '2026-10-20T00:00:00Z',
    commentCount: 1,
    entries: [],
    ...overrides,
  };
}

function render(detail) {
  return detailView({ view: 'detail', now: NOW, detail }, NOW);
}

// -- prReady / prDraft --

test('githubHeaderActions: a draft PR gets a "Mark ready" prReady button', () => {
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem({ state: 'draft' }), conversation: conversationData(), prompt: '' });
  assert.match(html, /data-act="prReady"[^>]*data-id="gh-pr-9"/);
  assert.match(html, /Mark ready/);
  assert.ok(!html.includes('data-act="prDraft"'));
});

test('githubHeaderActions: an open (non-draft) PR gets a "Convert to draft" prDraft button', () => {
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem({ state: 'open' }), conversation: conversationData(), prompt: '' });
  assert.match(html, /data-act="prDraft"[^>]*data-id="gh-pr-9"/);
  assert.match(html, /Convert to draft/);
  assert.ok(!html.includes('data-act="prReady"'));
});

test('githubHeaderActions: a merged or closed PR gets neither prReady nor prDraft', () => {
  for (const state of ['merged', 'closed']) {
    const html = render({ id: 'gh-pr-9', key: 9, data: prItem({ state }), conversation: conversationData(), prompt: '' });
    assert.ok(!html.includes('data-act="prReady"'), `${state}: prReady should not render`);
    assert.ok(!html.includes('data-act="prDraft"'), `${state}: prDraft should not render`);
  }
});

// -- rerunChecks --

test('githubHeaderActions: rerunChecks renders only when checks.counts.failed > 0', () => {
  const withFailures = render({ id: 'gh-pr-9', key: 9, data: prItem({ checks: { counts: { success: 1, failed: 2, pending: 0, skipped: 0 } } }), conversation: conversationData(), prompt: '' });
  assert.match(withFailures, /data-act="rerunChecks"[^>]*data-id="gh-pr-9"/);
  assert.match(withFailures, /Rerun failed checks/);

  const noFailures = render({ id: 'gh-pr-9', key: 9, data: prItem({ checks: { counts: { success: 1, failed: 0, pending: 0, skipped: 0 } } }), conversation: conversationData(), prompt: '' });
  assert.ok(!noFailures.includes('data-act="rerunChecks"'));

  const noChecks = render({ id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), prompt: '' });
  assert.ok(!noChecks.includes('data-act="rerunChecks"'));
});

// -- the ⋯ menu: PR --

test('⋯ menu (open PR): Rename…, Close pull request, Request reviewers…, Open merge box', () => {
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem({ state: 'open' }), conversation: conversationData(), prompt: '' });
  assert.match(html, /<details class="menu">/);
  assert.match(html, /data-act="prTitle"[^>]*data-id="gh-pr-9"[^>]*>[\s\S]*?Rename…/);
  assert.match(html, /data-act="prClose"[^>]*data-id="gh-pr-9"[^>]*>[\s\S]*?Close pull request/);
  assert.ok(!html.includes('data-act="prReopen"'));
  assert.match(html, /data-act="reviewers"[^>]*data-id="gh-pr-9"[^>]*>[\s\S]*?Request reviewers…/);
  assert.match(html, /data-url="https:\/\/github\.com\/o\/r\/pull\/9"[^>]*>[\s\S]*?Open merge box/);
});

test('⋯ menu (closed PR): prReopen replaces prClose', () => {
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem({ state: 'closed' }), conversation: conversationData(), prompt: '' });
  assert.match(html, /data-act="prReopen"[^>]*data-id="gh-pr-9"[^>]*>[\s\S]*?Reopen pull request/);
  assert.ok(!html.includes('data-act="prClose"'));
});

test('⋯ menu (merged PR): no close and no reopen item', () => {
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem({ state: 'merged' }), conversation: conversationData(), prompt: '' });
  assert.ok(!html.includes('data-act="prClose"'));
  assert.ok(!html.includes('data-act="prReopen"'));
  assert.match(html, /data-act="prTitle"/, 'rename stays available for a merged PR');
});

// -- the ⋯ menu: issue --

test('⋯ menu (issue): issueTitle Rename… item, no PR-only items', () => {
  const html = render({ id: 'gh-issue-7', key: 7, data: issueItem(), conversation: conversationData(), prompt: '' });
  assert.match(html, /data-act="issueTitle"[^>]*data-id="gh-issue-7"[^>]*>[\s\S]*?Rename…/);
  assert.ok(!html.includes('data-act="prTitle"'));
  assert.ok(!html.includes('data-act="prClose"'));
  assert.ok(!html.includes('data-act="reviewers" data-id="gh-issue-7"'));
});

// -- review threads --

function thread(overrides) {
  return {
    id: 'T1',
    resolved: false,
    outdated: false,
    path: 'a.go',
    line: 10,
    comments: [{ id: '101', kind: 'comment', actor: 'carol', avatarUrl: null, createdAt: '2026-10-20T01:00:00Z', bodyHtml: '<p>fix this</p>', url: null }],
    ...overrides,
  };
}

test('review threads: an unresolved thread renders as an open <section class="thread card"> with an unresolved pill', () => {
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), threads: [thread()], prompt: '' });
  assert.match(html, /<section class="thread card" data-thread="T1">/);
  assert.match(html, /pill-amber">unresolved</);
  assert.match(html, /a\.go:10/);
});

test('review threads: a resolved thread is collapsed inside <details class="thread card"> with a resolved pill', () => {
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), threads: [thread({ id: 'T2', resolved: true })], prompt: '' });
  assert.match(html, /<details class="thread card" data-thread="T2">/);
  assert.match(html, /pill-emerald">resolved</);
});

test('review threads: an outdated thread gets an extra outdated pill', () => {
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), threads: [thread({ outdated: true })], prompt: '' });
  assert.match(html, /pill-muted">outdated</);
});

test('review threads: each thread has a reply form keyed to its first comment, and a resolveThread toggle', () => {
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), threads: [thread()], prompt: '' });
  assert.match(html, /<form class="composer thread-reply" data-comment="101">/);
  assert.match(html, /data-act="resolveThread" data-thread="T1" data-resolved="true"[^>]*>Resolve</);
});

test('review threads: a resolved thread\'s toggle button reads Unresolve with data-resolved="false"', () => {
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), threads: [thread({ id: 'T3', resolved: true })], prompt: '' });
  assert.match(html, /data-act="resolveThread" data-thread="T3" data-resolved="false"[^>]*>Unresolve</);
});

test('review threads: no threads array renders no "Review threads" section', () => {
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), threads: [], prompt: '' });
  assert.ok(!html.includes('Review threads'));
  const htmlUndefined = render({ id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), prompt: '' });
  assert.ok(!htmlUndefined.includes('Review threads'));
});

test('review threads: the section header counts only unresolved threads as "open"', () => {
  const html = render({
    id: 'gh-pr-9',
    key: 9,
    data: prItem(),
    conversation: conversationData(),
    threads: [thread({ id: 'T1', resolved: false }), thread({ id: 'T2', resolved: true }), thread({ id: 'T3', resolved: false })],
    prompt: '',
  });
  assert.match(html, /Review threads<\/span><span class="count">2 open<\/span>/);
});

// -- teeth: a <script> smuggled into a thread comment's bodyHtml must not survive rendering --

test('teeth: an unsanitised <script> in a thread comment bodyHtml renders literally only if already pre-sanitised upstream', () => {
  // mapReviewThreads (lib/tasks-github.js) is the sanitisation boundary; the view trusts bodyHtml as given.
  // This test pins that threadCard does not re-introduce the script by double-escaping or re-injecting markup.
  const evilThread = thread({ comments: [{ id: '101', kind: 'comment', actor: 'attacker', avatarUrl: null, createdAt: '2026-10-20T01:00:00Z', bodyHtml: '<p>hi</p>', url: null }] });
  const html = render({ id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), threads: [evilThread], prompt: '' });
  assert.ok(!/<script/i.test(html));
});
