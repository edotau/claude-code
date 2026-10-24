'use strict';
// Unit tests for the GitHub detail rendering added to lib/tasks-view.js (detailView -> githubDetail):
// back button, issue status/assignees controls, PR tab strip, conversation tab (body, comment/review
// bubbles, timeline-row events, composer), checks tab, rail, and the loading/error states.
const test = require('node:test');
const assert = require('node:assert/strict');
const { detailView } = require('../lib/tasks-view');

const NOW = Date.parse('2026-10-24T00:00:00Z');

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

function conversationData(overrides) {
  return {
    bodyHtml: '<p>the issue body</p>',
    author: 'alice',
    avatarUrl: null,
    createdAt: '2026-10-20T00:00:00Z',
    commentCount: 1,
    entries: [
      { kind: 'comment', id: '1', event: 'commented', actor: 'carol', avatarUrl: null, createdAt: '2026-10-20T01:00:00Z', bodyHtml: '<p>a comment</p>', url: null },
      { kind: 'review', id: '2', event: 'reviewed', actor: 'dave', avatarUrl: null, createdAt: '2026-10-20T02:00:00Z', state: 'CHANGES_REQUESTED', bodyHtml: '<p>fix this</p>', url: null },
      { kind: 'event', id: '3', event: 'labeled', actor: 'eve', avatarUrl: null, createdAt: '2026-10-20T03:00:00Z', text: 'added label bug', url: null },
    ],
    ...overrides,
  };
}

// -- back button, always present --

test('detailView: back button is always rendered for a github item', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-issue-7', key: 7, data: issueItem(), tab: undefined, conversation: conversationData(), checks: undefined, prompt: '' } }, NOW);
  assert.match(html, /data-act="back"/);
});

// -- loading / error --

test('detailView: loading state shows a loading line, no back-less rendering', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-issue-7', key: 7, loading: true } }, NOW);
  assert.match(html, /data-act="back"/);
  assert.match(html, /Loading 7/);
});

test('detailView: error state shows a retry banner', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-issue-7', key: 7, error: 'boom' } }, NOW);
  assert.match(html, /data-act="detail"/);
  assert.match(html, /Retry/);
  assert.match(html, /boom/);
});

// -- issue: status menu + assignees button --

test('detailView (issue): Status menu has the data-issue-state items', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-issue-7', key: 7, data: issueItem(), conversation: conversationData(), prompt: '' } }, NOW);
  assert.match(html, /data-issue-state="open"/);
  assert.match(html, /data-issue-state="closed"[^>]*data-reason="completed"/);
  assert.match(html, /data-issue-state="closed"[^>]*data-reason="not_planned"/);
  assert.match(html, /data-issue-state="closed"[^>]*data-reason="duplicate"/);
});

test('detailView (issue): Assignees button is present', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-issue-7', key: 7, data: issueItem(), conversation: conversationData(), prompt: '' } }, NOW);
  assert.match(html, /data-act="assignees"/);
});

test('detailView (issue): no PR tab strip', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-issue-7', key: 7, data: issueItem(), conversation: conversationData(), prompt: '' } }, NOW);
  assert.ok(!html.includes('data-tab='));
});

// -- PR: tab strip --

test('detailView (PR): tab strip has conversation/checks/files, files is a link-out to <url>/files', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), prompt: '' } }, NOW);
  assert.match(html, /data-tab="conversation"/);
  assert.match(html, /data-tab="checks"/);
  assert.match(html, /data-tab="files"[^>]*data-url="https:\/\/github\.com\/o\/r\/pull\/9\/files"/);
});

test('detailView (PR): conversation tab is active by default (no explicit tab)', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), prompt: '' } }, NOW);
  assert.match(html, /class="tab active" role="tab" data-tab="conversation"/);
});

// -- conversation tab: body, bubbles, timeline-row, composer --

test('detailView (PR conversation): body renders in a .prose container', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), prompt: '' } }, NOW);
  assert.match(html, /<div class="prose"><p>the issue body<\/p><\/div>/);
});

test('detailView (PR conversation): one .bubble per comment/review entry, one .timeline-row per event entry', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), prompt: '' } }, NOW);
  assert.equal((html.match(/class="bubble"/g) || []).length, 2); // comment + review
  assert.equal((html.match(/class="timeline-row"/g) || []).length, 1); // labeled event
  assert.match(html, /added label bug/);
});

test('detailView (PR conversation): a CHANGES_REQUESTED review bubble shows a danger pill with lower-cased, space-joined state text', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), prompt: '' } }, NOW);
  assert.match(html, /pill-danger"[^>]*>changes requested</);
});

test('detailView (PR conversation): an APPROVED review bubble shows an emerald pill', () => {
  const conv = conversationData({ entries: [{ kind: 'review', id: '5', event: 'reviewed', actor: 'frank', avatarUrl: null, createdAt: '2026-10-20T04:00:00Z', state: 'APPROVED', bodyHtml: '<p>lgtm</p>', url: null }] });
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), conversation: conv, prompt: '' } }, NOW);
  assert.match(html, /pill-emerald"[^>]*>approved</);
});

test('detailView (PR conversation): any other review state shows a muted pill', () => {
  const conv = conversationData({ entries: [{ kind: 'review', id: '6', event: 'reviewed', actor: 'gina', avatarUrl: null, createdAt: '2026-10-20T05:00:00Z', state: 'DISMISSED', bodyHtml: '<p>n/a</p>', url: null }] });
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), conversation: conv, prompt: '' } }, NOW);
  assert.match(html, /pill-muted"[^>]*>dismissed</);
});

test('detailView (PR conversation): the composer form has #comment-body and #comment-send', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), prompt: '' } }, NOW);
  assert.match(html, /<form class="composer" id="composer">/);
  assert.match(html, /id="comment-body"/);
  assert.match(html, /id="comment-send"/);
});

test('detailView (issue conversation): same conversation tab renders for an issue too', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-issue-7', key: 7, data: issueItem(), conversation: conversationData(), prompt: '' } }, NOW);
  assert.match(html, /<div class="prose"><p>the issue body<\/p><\/div>/);
  assert.match(html, /id="comment-body"/);
});

// -- checks tab --

test('detailView (PR checks tab): lists runs with name, state pill, and a data-url link', () => {
  const checks = { total: 2, runs: [
    { name: 'build', status: 'completed', conclusion: 'success', url: 'https://ci/1', app: 'gha', state: 'success' },
    { name: 'test', status: 'completed', conclusion: 'failure', url: 'https://ci/2', app: 'gha', state: 'failed' },
  ] };
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), tab: 'checks', conversation: conversationData(), checks, prompt: '' } }, NOW);
  assert.match(html, /build/);
  assert.match(html, /test/);
  assert.match(html, /data-url="https:\/\/ci\/1"/);
  assert.match(html, /data-url="https:\/\/ci\/2"/);
  assert.match(html, /pill-emerald/);
  assert.match(html, /pill-danger/);
});

test('detailView (PR checks tab): empty runs render "No checks"', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), tab: 'checks', conversation: conversationData(), checks: { total: 0, runs: [] }, prompt: '' } }, NOW);
  assert.match(html, /No checks/);
});

test('detailView (PR checks tab): undefined checks render a loading spinner', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), tab: 'checks', conversation: conversationData(), checks: undefined, prompt: '' } }, NOW);
  assert.match(html, /class="spin"/);
});

// -- rail --

test('detailView rail (PR): Open in GitHub, Copy URL, Copy branch name, Copy prompt', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-pr-9', key: 9, data: prItem(), conversation: conversationData(), prompt: 'do the thing' } }, NOW);
  assert.match(html, /<aside class="rail">/);
  assert.match(html, /data-url="https:\/\/github\.com\/o\/r\/pull\/9">[\s\S]*?Open in GitHub/);
  assert.match(html, /data-label="URL"/);
  assert.match(html, /data-act="copy"[^>]*data-text="fix\/thing"[^>]*data-label="Branch name"/);
  assert.match(html, /data-act="copy"[^>]*data-text="do the thing"/);
});

test('detailView rail (issue): no Copy branch name button (issue has no branch)', () => {
  const html = detailView({ view: 'detail', now: NOW, detail: { id: 'gh-issue-7', key: 7, data: issueItem(), conversation: conversationData(), prompt: '' } }, NOW);
  assert.ok(!html.includes('Branch name'));
});
