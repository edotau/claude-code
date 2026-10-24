'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { renderTasksHtml, usesPrGrid, ISSUE_GRID, PR_GRID, SKELETON_ROWS } = require('../lib/tasks-view');
const { mapPullRequest, mapIssue } = require('../lib/tasks-github');
const { presetsFor } = require('../lib/tasks-state');

const REPO = { owner: 'acme', name: 'widgets' };
const prs = JSON.parse(fs.readFileSync(path.join(__dirname, 'fixtures', 'github', 'pr-list.json'), 'utf8')).map((p) => mapPullRequest(p, REPO));
const issues = JSON.parse(fs.readFileSync(path.join(__dirname, 'fixtures', 'github', 'issue-search.json'), 'utf8')).map((i) => mapIssue(i, REPO));

function model(overrides) {
  return {
    sources: [{ id: 'github' }, { id: 'bitbucket' }],
    source: 'github',
    scope: 'all',
    state: 'open',
    query: 'is:open',
    presets: presetsFor('github'),
    activePreset: null,
    defaultPreset: 'assigned',
    repos: [{ label: 'acme/widgets', selected: true }],
    repoCount: 1,
    banner: null,
    loading: false,
    loadingPage: null,
    items: [...prs, ...issues],
    errors: [],
    failedRepos: 0,
    hardError: null,
    page: 1,
    totalPages: 1,
    now: Date.parse('2026-10-24T12:00:00Z'),
    settings: { expanded: false, defaultSource: 'github', github: { status: { ok: true, login: 'dev1' }, visible: true, repos: [] }, bitbucket: { status: { ok: false, message: 'No token' }, visible: true, repos: [], baseUrl: 'https://bb.example.com' } },
    ...(overrides || {}),
  };
}

const opts = { nonce: 'n', cspSource: 'c' };

function count(html, needle) {
  return html.split(needle).length - 1;
}

test('usesPrGrid: pr scope, bitbucket, or an all-PR list → PR grid; issue scope or a mixed list → issue grid', () => {
  assert.equal(usesPrGrid(model()), false, 'mixed issues + PRs use the issue grid');
  assert.equal(usesPrGrid(model({ scope: 'pr' })), true);
  assert.equal(usesPrGrid(model({ source: 'bitbucket' })), true);
  assert.equal(usesPrGrid(model({ items: prs })), true, 'every row a PR → PR grid');
  assert.equal(usesPrGrid(model({ scope: 'issue', items: prs })), false);
});

test('renderTasksHtml: one row per item, the issue grid template, and the Status column for the mixed list', () => {
  const html = renderTasksHtml(model(), opts);
  assert.equal(count(html, 'class="row" role="button"'), prs.length + issues.length);
  assert.ok(html.includes(`--grid:${ISSUE_GRID}`));
  assert.ok(html.includes('>Status<') && !html.includes('>Reviewers<'));
});

test('renderTasksHtml: the PR grid renders Reviewers/Checks/Merge cells from the fixture (3 running, Checks failing, Draft)', () => {
  const html = renderTasksHtml(model({ scope: 'pr', items: prs }), opts);
  assert.ok(html.includes(`--grid:${PR_GRID}`));
  assert.ok(html.includes('>Reviewers<') && html.includes('>Checks<') && html.includes('>Merge<'));
  assert.ok(html.includes('3 running'), 'PR 33127 has 3 in-progress checks in the fixture');
  assert.ok(html.includes('Checks failing'), 'UNSTABLE merge state renders as Checks failing');
  assert.ok(html.includes('>Draft'), 'draft PRs render the Draft merge pill');
  assert.equal(count(html, 'id-pill tone-muted'), 2, 'two draft PRs get the muted draft ID pill');
});

test('renderTasksHtml: loading renders exactly 12 skeleton rows and no header row', () => {
  const html = renderTasksHtml(model({ loading: true, items: [] }), opts);
  assert.equal(count(html, 'row skeleton'), SKELETON_ROWS);
  assert.ok(html.includes('class="row head"'), 'the header stays above the skeletons');
});

test('renderTasksHtml: the empty state replaces the grid and hides pagination', () => {
  const html = renderTasksHtml(model({ items: [], totalPages: 1 }), opts);
  assert.ok(html.includes('Nothing matches'));
  assert.ok(!html.includes('class="row head"'));
  assert.ok(!html.includes('class="pagination"'));
});

test('renderTasksHtml: pagination shows Orca\'s page strip only when totalPages > 1, with the loading target spinning', () => {
  const none = renderTasksHtml(model({ totalPages: 1 }), opts);
  assert.ok(!none.includes('class="pagination"'));
  const html = renderTasksHtml(model({ page: 4, totalPages: 20, loadingPage: 6 }), opts);
  assert.ok(html.includes('class="pagination"'));
  assert.ok(html.includes('class="page-num active" data-page="4"'));
  assert.ok(html.includes('data-page="6"'), 'page 6 is in the ±2 window');
  assert.ok(/data-page="6"[^>]*><span class="spin">/.test(html), 'the loading target renders the spinner');
  assert.ok(html.includes('data-page="20"'), 'the last page is always listed');
  assert.ok(html.includes('page-ellipsis'));
});

test('renderTasksHtml: error rows and the "N of M repos failed" banner render alongside surviving rows', () => {
  const html = renderTasksHtml(model({ repoCount: 2, failedRepos: 1, errors: [{ repo: 'acme/gizmos', kind: 'issues', message: 'Repository not found or no access' }] }), opts);
  assert.ok(html.includes('1 of 2 repos failed'));
  assert.equal(count(html, 'row error-row'), 1);
  assert.ok(html.includes('acme/gizmos · issues'));
});

test('renderTasksHtml: the availability banner carries its action button with the source', () => {
  const html = renderTasksHtml(model({ source: 'bitbucket', banner: { kind: 'warn', text: 'Bitbucket: No token', action: { act: 'signIn', label: 'Set token' } } }), opts);
  assert.ok(html.includes('Bitbucket: No token'));
  assert.ok(html.includes('data-act="signIn" data-source="bitbucket"'));
});

test('renderTasksHtml: preset chips mark the active and default presets; the query input holds the query', () => {
  const html = renderTasksHtml(model({ query: 'is:pr is:open author:@me', activePreset: 'myprs' }), opts);
  assert.ok(html.includes('class="chip active" data-preset="myprs"'));
  assert.ok(/data-preset="assigned"[^>]*><span class="default-dot"/.test(html));
  assert.ok(html.includes('value="is:pr is:open author:@me"'));
});

test('renderTasksHtml: titles and labels are HTML-escaped', () => {
  const evil = { ...issues[0], title: '<img src=x onerror=alert(1)> & "quotes"', labels: ['<b>'] };
  const html = renderTasksHtml(model({ items: [evil] }), opts);
  assert.ok(!html.includes('<img src=x'));
  assert.ok(html.includes('&lt;img src=x onerror=alert(1)&gt; &amp; &quot;quotes&quot;'));
  assert.ok(html.includes('&lt;b&gt;'));
});

test('renderTasksHtml: CSP uses the nonce and cspSource, and the settings section lists both source cards', () => {
  const html = renderTasksHtml(model(), { nonce: 'abc123', cspSource: 'vscode-resource:' });
  assert.ok(html.includes("script-src 'nonce-abc123'"));
  assert.ok(html.includes("style-src 'unsafe-inline' vscode-resource:"));
  assert.ok(html.includes('<script nonce="abc123">'));
  assert.ok(html.includes('>Connected<') && html.includes('Connected as dev1'));
  assert.ok(html.includes('data-visible="bitbucket"'));
});
