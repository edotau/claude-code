'use strict';
// The read-only rule: items that aren't the user's own take no writes from this UI.
const test = require('node:test');
const assert = require('node:assert/strict');
const o = require('../lib/tasks-ownership');
const { renderTasksHtml, rowCells } = require('../lib/tasks-view');
const { state, fakeView, fakeContext, reset } = require('./fake-vscode');
const { TasksViewProvider } = require('../extension');

const logins = { jira: 'eric@x', jiraDisplay: 'Eric', github: 'edotau', bitbucket: 'eric' };

test('isMine / canWrite: assignee or author match per source; unknown login cannot judge; scope all allows', () => {
  assert.equal(o.isMine({ source: 'jira', assignee: { name: 'ERIC@x', displayName: 'Eric' } }, logins), true);
  assert.equal(o.isMine({ source: 'jira', assignee: { name: 'sam', displayName: 'Sam' } }, logins), false);
  assert.equal(o.isMine({ source: 'github', author: 'bot', assignees: ['edotau'] }, logins), true);
  assert.equal(o.isMine({ source: 'bitbucket', author: 'simon' }, logins), false);
  assert.equal(o.canWrite({ source: 'bitbucket', author: 'simon' }, logins, 'mine'), false);
  assert.equal(o.canWrite({ source: 'bitbucket', author: 'simon' }, logins, 'all'), true);
  assert.equal(o.canWrite({ source: 'bitbucket', author: 'simon' }, {}, 'mine'), true, 'no login known → cannot judge');
  assert.match(o.readOnlyReason({ source: 'jira', assignee: { displayName: 'Sam' } }), /assigned to Sam/);
});

const base = { sources: [{ id: 'github' }], source: 'github', query: '', scope: 'all', state: 'open', presets: [], repos: [], repoCount: 0, loading: false, errors: [], page: 1, totalPages: 1, now: 0, view: 'list', viewMode: 'list', rowErrors: {}, mergeMethods: {}, settings: { jira: {}, github: {}, bitbucket: {}, tailscale: {} } };
const pr = { source: 'github', id: 'g:1', type: 'pr', number: 1, title: 't', state: 'open', url: 'https://x/1', repo: { owner: 'o', name: 'r', label: 'o/r' }, author: 'simon', updatedAt: '', labels: [], reviewers: [{ name: 'eric', status: 'PENDING' }], assignees: [], checks: { state: 'success', label: '1 passed', counts: { success: 1, failed: 0, pending: 0 } }, merge: { state: 'ready', label: 'Ready' }, hydrated: true };

test('read-only rows: no Start, no status/merge menus, no assignee/reviewer buttons, a lock marker', () => {
  const cells = rowCells({ ...pr, readOnly: true }, { ...base, items: [pr] }, 'pr');
  assert.doesNotMatch(cells.actions, /data-act="start"|data-act="startNew"/);
  assert.match(cells.actions, /Read-only/);
  assert.doesNotMatch(cells.merge, /data-merge=|data-auto-merge=/);
  assert.doesNotMatch(cells.reviewers, /data-act="reviewers"/);
  assert.match(cells.title, /read-only/);
  const own = rowCells({ ...pr, readOnly: false }, { ...base, items: [pr] }, 'pr');
  assert.match(own.actions, /data-act="start"/);
  assert.match(own.merge, /data-merge=/);
});

test('read-only GitHub detail: no header actions, no composer, no thread reply/resolve; board card not draggable', () => {
  const detail = { id: pr.id, key: 1, data: { ...pr, readOnly: true }, tab: 'conversation', conversation: { bodyHtml: '<p>b</p>', entries: [], commentCount: 0 }, threads: [{ id: 'T', resolved: false, path: 'a', line: 1, comments: [{ id: '1', actor: 'x', bodyHtml: '<p>c</p>', createdAt: '' }] }] };
  const html = renderTasksHtml({ ...base, items: [pr], view: 'detail', detail }, { nonce: 'n' });
  const markup = html.slice(0, html.indexOf('<script')); // the script block always names these selectors
  assert.doesNotMatch(markup, /data-act="prDraft"|data-act="rerunChecks"|id="composer"|data-act="resolveThread"|thread-reply/);
  assert.match(html, /Read-only: pull request by simon/);
  const board = renderTasksHtml({ ...base, items: [pr], viewMode: 'board', board: [{ key: 'pr:open', label: 'Open', rows: [{ ...pr, readOnly: true }] }] }, { nonce: 'n' });
  assert.match(board, /class="card board-card[^"]*readonly" draggable="false"/);
  assert.doesNotMatch(board, /data-move-to=/);
});

test('provider: every write path refuses a teammate item with an inline reason, even when the webview posts it', async () => {
  reset();
  const p = new TasksViewProvider({ show() {}, hide() {} }, fakeContext());
  p.load = async () => {};
  p.loadBranch = async () => {};
  p.doctor = async () => {};
  p.status = { github: { ok: true, login: 'edotau' }, bitbucket: null, jira: null, tailscale: null };
  p.result = { items: [pr], errors: [], totalPages: 1, failedRepos: 0 };
  p.initialLoad = false;
  p.source = 'github';
  p.resolveWebviewView(fakeView('claudeCodeTasks.panel'));
  let wrote = 0;
  const writes = [
    () => p.runMutation(pr.id, { title: 'x' }, async () => { wrote++; }),
    () => p.handleMessage({ type: 'comment', id: pr.id, body: 'hi' }),
    () => p.handleMessage({ type: 'merge', id: pr.id, method: 'squash' }),
    () => p.handleMessage({ type: 'issueState', id: pr.id, state: 'closed', reason: 'completed' }),
    () => p.handleMessage({ type: 'start', id: pr.id }),
    () => p.handleMessage({ type: 'move', id: pr.id, to: 'pr:merged' }),
  ];
  for (const w of writes) await w();
  assert.equal(wrote, 0);
  assert.match(p.mutations.errorFor(pr.id).message, /Read-only: pull request by simon/);
  assert.equal(state.panels.length, 0);
  assert.equal(require('./fake-vscode').vscode.window.terminals.length, 0);
});
