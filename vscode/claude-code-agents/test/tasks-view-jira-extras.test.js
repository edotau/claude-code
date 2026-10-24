'use strict';
// Unit tests for the Jira "extras" in lib/tasks-view.js (list view only): sortable headers,
// status-group rows, status quick-filter chips + "Edit as JQL", Favourites menu, current-sprint
// checkbox, and the JQL preview line.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { renderTasksHtml, JIRA_GRID } = require('../lib/tasks-view');
const { mapIssue } = require('../lib/tasks-jira');
const { jiraColumns } = require('../lib/tasks-board');

const BASE = 'https://jira.biscrum.com';
const ISSUES = JSON.parse(fs.readFileSync(path.join(__dirname, 'fixtures', 'jira', 'search.json'), 'utf8')).issues;
const ROWS = ISSUES.map((i) => mapIssue(i, { baseUrl: BASE })); // In Progress, To Do, Done

function model(overrides) {
  return {
    sources: [{ id: 'jira' }, { id: 'github' }],
    source: 'jira',
    scope: 'all',
    state: 'all',
    query: '',
    presets: [],
    activePreset: null,
    defaultPreset: null,
    repos: [],
    repoCount: 1,
    banner: null,
    loading: false,
    loadingPage: null,
    items: ROWS,
    errors: [],
    failedRepos: 0,
    hardError: null,
    page: 1,
    totalPages: 1,
    now: Date.parse('2026-10-24T12:00:00Z'),
    settings: { expanded: false, defaultSource: 'jira', jira: { status: { ok: true }, visible: true, repos: [] } },
    ...(overrides || {}),
  };
}

const opts = { nonce: 'n', cspSource: 'c' };

function count(html, needle) {
  return html.split(needle).length - 1;
}

// ===== sortable header =====

test('headerRow: Key, Title, Assignee, Status, Priority, Updated carry data-sort buttons', () => {
  const html = renderTasksHtml(model(), opts);
  for (const field of ['key', 'title', 'assignee', 'status', 'priority', 'updated']) {
    assert.ok(html.includes(`data-sort="${field}"`), `missing data-sort="${field}"`);
  }
  // Actions has no sort control
  assert.equal(count(html, 'data-sort='), 6);
});

test('headerRow: the active sort field shows the ↑ arrow and aria-sort="ascending"', () => {
  const html = renderTasksHtml(model({ jiraSort: { field: 'status', dir: 'asc' } }), opts);
  assert.ok(html.includes('data-sort="status" aria-sort="ascending" title="Sort by status">Status ↑'));
  // every other sort button stays inactive (no aria-sort)
  assert.ok(html.includes('data-sort="key" title="Sort by key">Key<'));
  assert.equal(count(html, 'aria-sort='), 1);
});

test('headerRow: a descending sort shows the ↓ arrow and aria-sort="descending"', () => {
  const html = renderTasksHtml(model({ jiraSort: { field: 'updated', dir: 'desc' } }), opts);
  assert.ok(html.includes('data-sort="updated" aria-sort="descending" title="Sort by updated">Updated ↓'));
});

test('headerRow: with no jiraSort, no sort button is active and no arrow renders', () => {
  const html = renderTasksHtml(model(), opts);
  assert.equal(html.includes('↑'), false);
  assert.equal(html.includes('↓'), false);
  assert.equal(count(html, 'sort-btn active'), 0);
});

// ===== status-group rows (model.jiraGroups) =====

test('jiraGroups: renders one .row.group per group, with its key, pill label, count, and a collapse chevron', () => {
  const groups = jiraColumns(ROWS, ['To Do', 'In Progress', 'Done']);
  const html = renderTasksHtml(model({ jiraGroups: groups }), opts);
  for (const g of groups) {
    assert.ok(html.includes(`class="row group" data-group="${g.key}"`), `missing group row for ${g.key}`);
    assert.ok(html.includes(`data-collapse-group="${g.key}"`), `missing collapse control for ${g.key}`);
    assert.ok(html.includes(`>${g.label}<`), `missing label for ${g.key}`);
    assert.ok(html.includes(`<span class="count">${g.rows.length}</span>`), `missing count for ${g.key}`);
  }
  assert.equal(count(html, 'class="row group"'), groups.length);
  // every row still renders once, nested under its group
  assert.equal(count(html, 'class="row" role="button"'), ROWS.length);
});

test('jiraGroups: without model.jiraGroups, no group rows render (plain item list)', () => {
  const html = renderTasksHtml(model(), opts);
  assert.equal(count(html, 'class="row group"'), 0);
  assert.equal(count(html, 'class="row" role="button"'), ROWS.length);
});

test('jiraGroups: an empty groups array renders no item rows and no group rows', () => {
  const html = renderTasksHtml(model({ jiraGroups: [] }), opts);
  assert.equal(count(html, 'class="row group"'), 0);
  assert.equal(count(html, 'class="row" role="button"'), 0);
});

// ===== status quick-filter chips + Edit as JQL =====

const CATALOG = [
  { name: 'To Do', category: 'new', count: 1 },
  { name: 'In Progress', category: 'indeterminate', count: 1 },
  { name: 'Done', category: 'done', count: 1 },
];

test('jiraStatusCatalog: renders one chip per status with data-status-chip', () => {
  const html = renderTasksHtml(model({ jiraStatusCatalog: CATALOG }), opts);
  for (const st of CATALOG) assert.ok(html.includes(`data-status-chip="${st.name}"`), `missing chip for ${st.name}`);
});

test('jiraStatuses: the active chip(s) carry class "active"; inactive ones do not', () => {
  const html = renderTasksHtml(model({ jiraStatusCatalog: CATALOG, jiraStatuses: ['To Do'] }), opts);
  assert.ok(html.includes('class="chip active" data-status-chip="To Do"'));
  assert.ok(html.includes('class="chip" data-status-chip="In Progress"'));
  assert.ok(html.includes('class="chip" data-status-chip="Done"'));
});

test('jiraStatuses: with at least one active chip, an "Edit as JQL" chip appears', () => {
  const html = renderTasksHtml(model({ jiraStatusCatalog: CATALOG, jiraStatuses: ['Done'] }), opts);
  assert.ok(html.includes('data-act="editJql"'));
  assert.ok(html.includes('>Edit as JQL<'));
});

test('jiraStatuses: with none active, no "Edit as JQL" chip appears', () => {
  const html = renderTasksHtml(model({ jiraStatusCatalog: CATALOG, jiraStatuses: [] }), opts);
  assert.equal(html.includes('data-act="editJql"'), false);
});

test('jiraStatusCatalog: when empty (or absent), no status chips render', () => {
  // (the static SCRIPT block references the `[data-status-chip]` selector regardless; check for the attribute value instead)
  const html = renderTasksHtml(model({ jiraStatusCatalog: [] }), opts);
  assert.equal(html.includes('data-status-chip="'), false);
  const html2 = renderTasksHtml(model(), opts);
  assert.equal(html2.includes('data-status-chip="'), false);
});

// ===== Favourites menu =====

const FAVOURITES = [
  { id: '10021', name: 'My open stories', jql: 'assignee = currentUser() ORDER BY updated DESC' },
  { id: '10022', name: 'Sprint work', jql: 'sprint in openSprints() ORDER BY updated DESC' },
];

test('jiraFavourites: renders a details.fav-menu with one [data-favourite] item per favourite', () => {
  const html = renderTasksHtml(model({ jiraFavourites: FAVOURITES }), opts);
  assert.ok(html.includes('<details class="menu fav-menu"'));
  for (const f of FAVOURITES) assert.ok(html.includes(`data-favourite="${f.id}"`) && html.includes(`>${f.name}<`));
});

test('jiraFavourites: an empty (or absent) list hides the Favourites menu entirely', () => {
  // (the static CSS block has a `.fav-menu` rule regardless; check for the actual <details> element instead)
  const html = renderTasksHtml(model({ jiraFavourites: [] }), opts);
  assert.equal(html.includes('class="menu fav-menu"'), false);
  const html2 = renderTasksHtml(model(), opts);
  assert.equal(html2.includes('class="menu fav-menu"'), false);
});

// ===== current-sprint checkbox =====

test('jiraSprint: the sprint checkbox is present for Jira and checked only when model.jiraSprint is true', () => {
  const on = renderTasksHtml(model({ jiraSprint: true }), opts);
  assert.ok(on.includes('<input type="checkbox" data-filter="sprint" checked>'));
  const off = renderTasksHtml(model({ jiraSprint: false }), opts);
  assert.ok(off.includes('<input type="checkbox" data-filter="sprint">'));
  assert.equal(off.includes('data-filter="sprint" checked'), false);
});

// ===== JQL preview =====

test('jiraJqlPreview: renders a muted .jql-preview line with the exact text when set', () => {
  const jql = 'assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC';
  const html = renderTasksHtml(model({ jiraJqlPreview: jql }), opts);
  assert.ok(html.includes(`<div class="jql-preview muted mono truncate" title="${jql}">${jql}</div>`));
});

test('jiraJqlPreview: renders nothing when unset or empty', () => {
  // (the static CSS block has a `.jql-preview` rule regardless; check for the actual <div> element instead)
  const html = renderTasksHtml(model(), opts);
  assert.equal(html.includes('<div class="jql-preview'), false);
  const html2 = renderTasksHtml(model({ jiraJqlPreview: '' }), opts);
  assert.equal(html2.includes('<div class="jql-preview'), false);
});

// teeth: a status/favourite name with HTML-special characters must come through escaped, never raw markup.
test('TEETH: a status chip name and a favourite name containing "<" are escaped, not injected as markup', () => {
  const html = renderTasksHtml(
    model({
      jiraStatusCatalog: [{ name: '<script>x</script>', category: 'new', count: 1 }],
      jiraFavourites: [{ id: '1', name: '<b>bold</b>', jql: 'x' }],
    }),
    opts
  );
  assert.equal(html.includes('<script>x</script>'), false);
  assert.ok(html.includes('&lt;script&gt;x&lt;/script&gt;'));
  assert.equal(html.includes('<b>bold</b>'), false);
  assert.ok(html.includes('&lt;b&gt;bold&lt;/b&gt;'));
});

// sanity: this is still the Jira grid (not an issue/PR grid regression from the other changes).
test('sanity: renderTasksHtml for source:jira still uses the Jira grid template', () => {
  const html = renderTasksHtml(model(), opts);
  assert.ok(html.includes(`--grid:${JIRA_GRID}`));
});
