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

// Why: review state stays a pill on the card; columns only split by lifecycle so a mixed board fits two columns.
const GITHUB_PR_COLUMNS = [['pr:open', 'Open'], ['pr:merged', 'Merged'], ['pr:closed', 'Closed']];
const GITHUB_ISSUE_COLUMNS = [['issue:open', 'Open'], ['issue:closed', 'Closed']];
const COLLAPSED_BY_DEFAULT = new Set(['pr:merged', 'pr:closed', 'issue:closed']);
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
  const prLifecycle = (r) => `pr:${r.state === 'merged' ? 'merged' : r.state === 'closed' ? 'closed' : 'open'}`;
  const issueLifecycle = (r) => `issue:${r.state === 'closed' ? 'closed' : 'open'}`;
  const withDefaults = (cols) => cols.map((c) => ({ ...c, collapsed: COLLAPSED_BY_DEFAULT.has(c.key) }));
  if (scope === 'pr' || (prs.length && !issues.length)) return withDefaults(fixedColumns(GITHUB_PR_COLUMNS, list, prLifecycle));
  if (scope === 'issue' || (issues.length && !prs.length)) return withDefaults(fixedColumns(GITHUB_ISSUE_COLUMNS, list, issueLifecycle));
  // Mixed scope: one column per kind, lifecycle stacked as sections (closed/merged collapsed) so the board stays two wide.
  const issueSections = withDefaults(fixedColumns(GITHUB_ISSUE_COLUMNS, issues, issueLifecycle));
  const prSections = withDefaults(fixedColumns(GITHUB_PR_COLUMNS, prs, prLifecycle));
  return [
    { key: 'issues', label: 'Issues', rows: issues, sections: issueSections },
    { key: 'prs', label: 'Pull requests', rows: prs, sections: prSections },
  ];
}

/** Flat drop targets: a column's sections when it has them, else the column itself. */
function dropTargets(columns) {
  return (columns || []).flatMap((c) => (c.sections ? c.sections.map((s) => ({ ...s, column: c.key, label: `${c.label} · ${s.label}` })) : [{ ...c, column: c.key }]));
}

/** The transitions that land a Jira row in `column` (by target status name, then category); [] when none. */
function jiraTransitionsTo(transitions, column) {
  const list = transitions || [];
  const byName = list.filter((t) => t.to && t.to.name === column.label);
  if (byName.length) return byName;
  return list.filter((t) => t.to && t.to.category && t.to.category === column.category && !column.label);
}

module.exports = { groupRows, dropTargets, jiraColumns, githubPrColumn, bitbucketColumn, jiraTransitionsTo, JIRA_CATEGORY_LABELS, GITHUB_PR_COLUMNS, GITHUB_ISSUE_COLUMNS, BITBUCKET_COLUMNS };
