'use strict';
// Board projection of the list rows (Orca's task-page-jira-board-sections / project-board-columns): pure grouping.
// The board is a view over the same page of rows, never a second query.

const JIRA_CATEGORY_ORDER = ['new', 'indeterminate', 'done'];
const JIRA_CATEGORY_LABELS = { new: 'To Do', indeterminate: 'In Progress', done: 'Done' };

/** Jira: one column per status name, ordered by category (To Do · In Progress · Done) then name; `order` (status names) wins. */
function jiraColumns(rows, order, ensure) {
  const seen = new Map();
  for (const col of ensure || []) seen.set(col.label, { key: `status:${col.label}`, label: col.label, category: col.category, rows: [] });
  for (const row of rows) {
    const st = row.status || { name: row.state === 'closed' ? 'Done' : 'Open', category: row.state === 'closed' ? 'done' : 'new' };
    if (!seen.has(st.name)) seen.set(st.name, { key: `status:${st.name}`, label: st.name, category: st.category, rows: [] });
    seen.get(st.name).rows.push(row);
  }
  const cols = [...seen.values()];
  const rank = (c) => {
    const i = (order || []).indexOf(c.label);
    if (i !== -1) return i;
    return 1000 + JIRA_CATEGORY_ORDER.indexOf(c.category) * 100;
  };
  cols.sort((a, b) => rank(a) - rank(b) || a.label.localeCompare(b.label));
  return cols;
}

/** GitHub PRs: Draft · Open · Changes requested · Approved · Merged · Closed (from state + reviewDecision). */
function githubPrColumn(row) {
  if (row.state === 'merged') return 'merged';
  if (row.state === 'closed') return 'closed';
  if (row.state === 'draft') return 'draft';
  const d = String(row.reviewDecision || '').toUpperCase();
  if (d === 'APPROVED') return 'approved';
  if (d === 'CHANGES_REQUESTED') return 'changes';
  return 'open';
}

const GITHUB_PR_COLUMNS = [
  ['draft', 'Draft'], ['open', 'Open'], ['changes', 'Changes requested'], ['approved', 'Approved'], ['merged', 'Merged'], ['closed', 'Closed'],
];
const GITHUB_ISSUE_COLUMNS = [['open', 'Open'], ['closed', 'Closed']];
const BITBUCKET_COLUMNS = [['open', 'Open'], ['needs_work', 'Needs work'], ['approved', 'Approved'], ['merged', 'Merged'], ['closed', 'Declined']];

function bitbucketColumn(row) {
  if (row.state === 'merged') return 'merged';
  if (row.state === 'closed') return 'closed';
  const r = row.reviewers || [];
  if (r.some((x) => x.status === 'CHANGES_REQUESTED')) return 'needs_work';
  if (r.some((x) => x.status === 'APPROVED')) return 'approved';
  return 'open';
}

function fixedColumns(spec, rows, pick) {
  const cols = spec.map(([key, label]) => ({ key, label, rows: [] }));
  for (const row of rows) {
    const key = pick(row);
    (cols.find((c) => c.key === key) || cols[0]).rows.push(row);
  }
  return cols;
}

/** `groupRows(rows, source, scope, opts)` → `[{key, label, category?, rows}]`; empty columns are kept so drops have targets. */
function groupRows(rows, source, scope, opts) {
  const list = Array.isArray(rows) ? rows : [];
  if (source === 'jira') return jiraColumns(list, opts && opts.statusOrder, opts && opts.ensure);
  if (source === 'bitbucket') return fixedColumns(BITBUCKET_COLUMNS, list, bitbucketColumn);
  const prs = list.filter((r) => r.type === 'pr');
  const issues = list.filter((r) => r.type !== 'pr');
  if (scope === 'pr' || (prs.length && !issues.length)) return fixedColumns(GITHUB_PR_COLUMNS, list, githubPrColumn);
  if (scope === 'issue' || (issues.length && !prs.length)) return fixedColumns(GITHUB_ISSUE_COLUMNS, list, (r) => (r.state === 'closed' ? 'closed' : 'open'));
  return [...fixedColumns(GITHUB_ISSUE_COLUMNS, issues, (r) => (r.state === 'closed' ? 'closed' : 'open')).map((c) => ({ ...c, key: `issue:${c.key}`, label: `Issues · ${c.label}` })), ...fixedColumns(GITHUB_PR_COLUMNS, prs, githubPrColumn).map((c) => ({ ...c, key: `pr:${c.key}`, label: `PRs · ${c.label}` }))];
}

/** The transitions that land a Jira row in `column` (by target status name, then category); [] when none. */
function jiraTransitionsTo(transitions, column) {
  const list = transitions || [];
  const byName = list.filter((t) => t.to && t.to.name === column.label);
  if (byName.length) return byName;
  return list.filter((t) => t.to && t.to.category && t.to.category === column.category && !column.label);
}

module.exports = { groupRows, jiraColumns, githubPrColumn, bitbucketColumn, jiraTransitionsTo, JIRA_CATEGORY_LABELS, GITHUB_PR_COLUMNS, GITHUB_ISSUE_COLUMNS, BITBUCKET_COLUMNS };
