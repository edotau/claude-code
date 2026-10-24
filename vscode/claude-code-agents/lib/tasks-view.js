'use strict';
// Webview HTML for the Tasks page (Orca's Task page, GitHub + Bitbucket DC) — no vscode dependency, so node:test
// can render it. Same nonce/CSP shape as usage-view.js; the page re-renders whole on every model change.
const { ICONS } = require('../media/icons');
const { getPageNumbers, formatRelativeTime } = require('./tasks-state');

const SKELETON_ROWS = 12;
const ISSUE_GRID = '72px minmax(320px,1fr) 84px 100px 92px 122px';
const PR_GRID = '72px minmax(360px,2fr) 132px 128px 132px 92px 158px';
// Jira: ID (key) · Title · Assignee · Status · Priority · Updated · Actions; the key is wider than a #number.
const JIRA_GRID = '156px minmax(320px,1fr) 110px 120px 100px 92px 158px';
const SOURCE_LABELS = { github: 'GitHub', bitbucket: 'Bitbucket', jira: 'Jira' };
const GRIDS = { pr: PR_GRID, issue: ISSUE_GRID, jira: JIRA_GRID };

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function attr(obj) {
  return JSON.stringify(obj).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;');
}

/** `jira` for the Jira source, else `pr` / `issue` per usesPrGrid. */
function gridKind(model) {
  return model.source === 'jira' ? 'jira' : usesPrGrid(model) ? 'pr' : 'issue';
}

/** The PR grid when the query scope is pr, Bitbucket is active, or every loaded row is a PR; else the issue grid. */
function usesPrGrid(model) {
  if (model.source === 'jira') return false;
  if (model.source === 'bitbucket' || model.scope === 'pr') return true;
  if (model.scope === 'issue') return false;
  return model.items.length > 0 && model.items.every((i) => i.type === 'pr');
}

function pill(tone, text, title) {
  return `<span class="pill pill-${tone}"${title ? ` title="${esc(title)}"` : ''}>${esc(text)}</span>`;
}

function stateTone(state) {
  if (state === 'open' || state === 'draft') return 'emerald';
  if (state === 'merged') return 'primary';
  if (state === 'closed' || state === 'declined') return 'muted';
  return 'muted';
}

/** `<img>` for a Jira-hosted icon (issue type, priority); the CSP allows https images. */
function remoteIcon(ref, cls) {
  return ref && ref.iconUrl ? `<img class="${cls}" src="${esc(ref.iconUrl)}" alt="" title="${esc(ref.name || '')}">` : '';
}

function idPill(item) {
  if (item.source === 'jira') {
    const icon = remoteIcon(item.issueType, 'type-icon') || ICONS.circleDot;
    return `<span class="id-pill" aria-label="${esc((item.issueType && item.issueType.name) || 'Issue')} ${esc(item.key)}" title="${esc((item.issueType && item.issueType.name) || '')}">${icon}<span class="mono">${esc(item.key)}</span></span>`;
  }
  const draft = item.state === 'draft';
  const icon = item.type === 'pr' ? (draft ? ICONS.gitPullRequestDraft : item.state === 'merged' ? ICONS.gitMerge : ICONS.gitPullRequest) : ICONS.circleDot;
  const tone = item.type !== 'pr' ? '' : item.state === 'merged' ? ' tone-primary' : item.state === 'closed' ? ' tone-danger' : draft ? ' tone-muted' : ' tone-emerald';
  const label = `${item.type === 'pr' ? (draft ? 'Draft pull request' : 'Pull request') : 'Issue'} #${item.number}`;
  return `<span class="id-pill${tone}" aria-label="${esc(label)}"${draft ? ' title="Draft"' : ''}>${icon}<span class="mono">#${esc(item.number)}</span></span>`;
}

function deltaText(item) {
  if (item.type !== 'pr' || item.additions == null || item.deletions == null) return '';
  return `+${item.additions} −${item.deletions}`;
}

function titleCell(item, model) {
  const multiRepo = model.repoCount > 1;
  const badge = item.type === 'pr' && (item.state === 'merged' || item.state === 'closed') ? pill(stateTone(item.state), item.state === 'merged' ? 'Merged' : model.source === 'bitbucket' ? 'Declined' : 'Closed') : '';
  const repoBadge = multiRepo ? `<span class="repo-badge"><span class="repo-dot"></span>${esc(item.repo.name)}</span>` : '';
  const meta = [`<span>${esc(item.author || 'unknown author')}</span>`];
  if (!multiRepo) meta.push(`<span>${esc(item.repo.label)}</span>`);
  if (item.state === 'draft') meta.push('<span aria-hidden="true">·</span><span>Draft</span>');
  const delta = deltaText(item);
  if (delta) meta.push(`<span class="meta-icon">${ICONS.files}${esc(delta)}</span>`);
  if (item.jiraKey && item.source !== 'jira') meta.push(`<a class="jira-chip" data-url="${esc(item.jiraUrl || '')}" title="Open ${esc(item.jiraKey)} in Jira">${esc(item.jiraKey)}</a>`);
  for (const label of (item.labels || []).slice(0, 3)) meta.push(`<span class="label-chip">${esc(label)}</span>`);
  const rowError = model.rowErrors && model.rowErrors[item.id];
  const errorLine = rowError ? `<div class="row-error">${ICONS.alertCircle}<span class="truncate" title="${esc(rowError)}">${esc(rowError)}</span><button class="btn btn-outline" data-act="retry" data-id="${esc(item.id)}">Retry</button><button class="icon-btn" data-act="clearError" data-id="${esc(item.id)}" aria-label="Dismiss">${ICONS.x}</button></div>` : '';
  return `<div class="title-row"><h3 class="title">${esc(item.title)}</h3>${badge}${repoBadge}</div><div class="meta">${meta.join('')}</div>${errorLine}`;
}

function initials(name) {
  const clean = String(name || '').replace(/^@/, '');
  const parts = clean.split(/[\s._-]+/).filter(Boolean);
  return (parts.length > 1 ? parts[0][0] + parts[1][0] : clean.slice(0, 2)).toUpperCase();
}

function reviewerTone(status) {
  if (status === 'APPROVED') return 'emerald';
  if (status === 'CHANGES_REQUESTED') return 'danger';
  return 'muted';
}

/** Primary reviewer chip + `+N`; the full roster lives in the tooltip (display only, no picker in phase 1). */
function reviewersCell(item) {
  const list = item.reviewers || [];
  if (!list.length) return `<span class="muted cell-text">${item.merge && item.merge.state === 'loading' ? '…' : item.type === 'pr' ? 'No reviewers' : ''}</span>`;
  const primary = list.find((r) => r.status === 'APPROVED') || list.find((r) => r.status === 'CHANGES_REQUESTED') || list[0];
  const roster = list.map((r) => `${r.name} — ${r.status.toLowerCase().replace('_', ' ')}`).join('\n');
  const more = list.length > 1 ? `<span class="more">+${list.length - 1}</span>` : '';
  return `<span class="reviewers" title="${esc(roster)}"><span class="avatar ring-${reviewerTone(primary.status)}">${esc(initials(primary.name))}</span><span class="reviewer-name">${esc(primary.name)}</span>${more}</span>`;
}

/** GitHub: the roster is a button opening the reviewer QuickPick; Bitbucket stays display-only. */
function reviewersCellFor(item) {
  if (item.source !== 'github' || item.type !== 'pr' || item.state === 'merged' || item.state === 'closed') return reviewersCell(item);
  return `<button class="cell-btn" data-act="reviewers" data-id="${esc(item.id)}" title="Request reviewers">${reviewersCell(item)}${ICONS.chevronDown}</button>`;
}

function checksCell(item) {
  const c = item.checks;
  if (c && c.state === 'loading') return '<span class="muted cell-text">…</span>';
  if (!c || c.state === 'none') return `<span class="cell-text muted">${ICONS.minus}${esc(c ? c.label : 'No checks')}</span>`;
  const icon = c.state === 'success' ? ICONS.checkCircle2 : c.state === 'failed' ? ICONS.alertCircle : ICONS.clock3;
  const tone = c.state === 'success' ? 'emerald' : c.state === 'failed' ? 'danger' : 'amber';
  const url = c.url || item.url;
  return `<button class="cell-text tone-${tone} link-btn" data-url="${esc(url)}" title="Open checks">${icon}${esc(c.label)}</button>`;
}

const MERGE_METHOD_LABELS = { merge: 'Create a merge commit', squash: 'Squash and merge', rebase: 'Rebase and merge' };

function mergeCell(item, model) {
  const m = item.merge;
  if (!m) return '';
  if (m.state === 'loading') return '<span class="muted cell-text">…</span>';
  const tone = m.state === 'ready' ? 'emerald' : m.state === 'merged' ? 'primary' : m.state === 'conflict' ? 'danger' : m.state === 'blocked' ? 'amber' : 'muted';
  const terminal = item.state === 'merged' || item.state === 'closed';
  if (item.source !== 'github' || terminal) {
    const title = item.source === 'bitbucket' ? 'Open the pull request in Bitbucket' : `Open the merge box in browser (${m.label})`;
    return `<button class="pill pill-${tone} pill-btn link-btn" data-url="${esc(item.url)}" title="${esc(title)}">${esc(m.label)}${ICONS.externalLink}</button>`;
  }
  const allowed = (model && model.mergeMethods && model.mergeMethods[item.repo.label]) || { merge: true, squash: true, rebase: true };
  const methods = Object.keys(MERGE_METHOD_LABELS).filter((k) => allowed[k]).map((k) => `<button data-merge="${k}" data-id="${esc(item.id)}">${ICONS.gitMerge}${MERGE_METHOD_LABELS[k]}</button>`).join('');
  const auto = `<button data-auto-merge="${item.autoMergeEnabled ? 'disable' : 'enable'}" data-id="${esc(item.id)}">${ICONS.clock3}${item.autoMergeEnabled ? 'Disable auto-merge' : 'Enable auto-merge'}</button>`;
  return `<details class="menu"><summary class="pill pill-${tone} pill-btn" title="Merge options (${esc(m.label)})">${esc(m.label)}${ICONS.chevronDown}</summary><div class="menu-items">${auto}<div class="menu-sep"></div>${methods}<div class="menu-sep"></div><button data-url="${esc(item.url)}">${ICONS.externalLink}Open GitHub merge box</button></div></details>`;
}

/** Jira status pill toned by category: To Do muted, In Progress primary, Done emerald. */
function jiraStatusCell(item) {
  const st = item.status || { name: item.state === 'closed' ? 'Done' : 'Open', category: item.state === 'closed' ? 'done' : 'new' };
  const tone = st.category === 'done' ? 'emerald' : st.category === 'indeterminate' ? 'primary' : 'muted';
  return pill(tone, st.name, st.name);
}

function priorityCell(item) {
  const p = item.priority;
  if (!p) return '<span class="muted cell-text">—</span>';
  return `<span class="cell-text" title="${esc(p.name)}">${remoteIcon(p, 'type-icon')}${esc(p.name)}</span>`;
}

function statusCell(item) {
  if (item.source === 'jira') return jiraStatusCell(item);
  const closed = item.state === 'closed';
  const reason = item.stateReason ? ` (${String(item.stateReason).replace('_', ' ')})` : '';
  const pillHtml = pill(closed ? 'primary' : 'emerald', closed ? 'Closed' : 'Open', `${closed ? 'Closed' : 'Open'}${reason}`);
  if (item.source !== 'github' || item.type !== 'issue') return pillHtml;
  const id = esc(item.id);
  return `<details class="menu"><summary class="pill-menu" title="Change status">${pillHtml}${ICONS.chevronDown}</summary><div class="menu-items"><button data-issue-state="open" data-id="${id}">${ICONS.circleDot}Open</button><button data-issue-state="closed" data-reason="completed" data-id="${id}">${ICONS.checkCircle2}Close as completed</button><button data-issue-state="closed" data-reason="not_planned" data-id="${id}">${ICONS.minus}Close as not planned</button><button data-issue-state="closed" data-reason="duplicate" data-id="${id}">${ICONS.files}Close as duplicate…</button></div></details>`;
}

function assigneesCell(item) {
  const list = item.assignees || [];
  const more = list.length > 1 ? `<span class="more">+${list.length - 1}</span>` : '';
  const stack = !list.length ? '<span class="muted cell-text">—</span>' : `<span class="reviewers" title="${esc(list.join('\n'))}"><span class="avatar ring-muted">${esc(initials(list[0]))}</span>${more}</span>`;
  if (item.source !== 'github') return stack;
  return `<button class="cell-btn" data-act="assignees" data-id="${esc(item.id)}" title="Change assignees">${stack}${ICONS.chevronDown}</button>`;
}

function updatedCell(item, now) {
  const rel = formatRelativeTime(item.updatedAt, now);
  const abs = Number.isFinite(Date.parse(item.updatedAt)) ? new Date(item.updatedAt).toLocaleString('en-US', { timeZone: 'UTC', timeZoneName: 'short' }) : '';
  return `<span class="muted cell-text" title="${esc(abs)}">${esc(rel)}</span>`;
}

/** PR: `Start` + chevron menu (Start, Open in browser). Issue: `Start | Open` + ⋮ menu. */
function actionsCell(item) {
  const id = esc(item.id);
  const start = `<button class="btn btn-outline" data-act="start" data-id="${id}" title="Copy a task prompt and open Claude">${ICONS.play}Start</button>`;
  if (item.type === 'pr') {
    return `<span class="split">${start}<details class="menu"><summary class="btn btn-outline btn-icon" aria-label="More actions">${ICONS.chevronDown}</summary><div class="menu-items"><button data-act="start" data-id="${id}">${ICONS.plus}Start new workspace</button><button data-act="open" data-url="${esc(item.url)}">${ICONS.externalLink}Open in browser</button></div></details></span>`;
  }
  return `<span class="split">${start}<button class="btn btn-outline" data-act="open" data-url="${esc(item.url)}">${ICONS.externalLink}Open</button><details class="menu"><summary class="btn btn-outline btn-icon" aria-label="More actions">${ICONS.ellipsisVertical}</summary><div class="menu-items"><button data-act="open" data-url="${esc(item.url)}">${ICONS.externalLink}Open in browser</button></div></details></span>`;
}

/** Every cell of a row keyed by name — `row()` renders them; the extension re-posts them to patch cells in place. */
function rowCells(item, model, kind) {
  const cells = { id: idPill(item), title: titleCell(item, model) };
  if (kind === 'pr') {
    cells.reviewers = reviewersCellFor(item);
    cells.checks = checksCell(item);
    cells.merge = mergeCell(item, model);
  } else if (kind === 'jira') {
    cells.assignees = assigneesCell(item);
    cells.status = statusCell(item);
    cells.priority = priorityCell(item);
  } else {
    cells.assignees = assigneesCell(item);
    cells.status = statusCell(item);
  }
  cells.updated = updatedCell(item, model.now);
  cells.actions = actionsCell(item);
  return cells;
}

function row(item, model, kind) {
  const cells = Object.entries(rowCells(item, model, kind)).map(([name, html]) => `<div class="cell${name === 'id' ? ' c-id' : name === 'title' ? ' c-title' : name === 'actions' ? ' c-actions' : ''}" data-cell="${name}">${html}</div>`);
  const hydrate = item.source === 'github' && item.type === 'pr' && item.hydrated === false ? ' data-hydrate="1"' : '';
  return `<div class="row" role="button" tabindex="0" data-id="${esc(item.id)}" data-url="${esc(item.url)}"${hydrate}>${cells.join('')}</div>`;
}

const HEADERS = {
  pr: ['ID', 'Title', 'Reviewers', 'Checks', 'Merge', 'Updated', 'Actions'],
  issue: ['ID', 'Title', 'Assignees', 'Status', 'Updated', 'Actions'],
  jira: ['Key', 'Title', 'Assignee', 'Status', 'Priority', 'Updated', 'Actions'],
};

function headerRow(kind) {
  const cols = HEADERS[kind] || HEADERS.issue;
  return `<div class="row head">${cols.map((c, i) => `<div class="cell${i === 0 ? ' c-id' : i === 1 ? ' c-title' : ''}">${c}</div>`).join('')}</div>`;
}

function skeletonRows(kind) {
  const n = (HEADERS[kind] || HEADERS.issue).length;
  const one = `<div class="row skeleton" aria-hidden="true">${Array.from({ length: n }, (_, i) => `<div class="cell${i === 0 ? ' c-id' : i === 1 ? ' c-title' : ''}"><span class="shimmer${i === 1 ? ' wide' : ''}"></span></div>`).join('')}</div>`;
  return one.repeat(SKELETON_ROWS);
}

function errorRows(model) {
  return (model.errors || []).map((e) => `<div class="row error-row"><div class="cell c-id">${ICONS.alertCircle}</div><div class="cell c-title"><span class="title">${esc(e.repo)} · ${esc(e.kind)}</span><div class="meta">${esc(e.message)}</div></div><div class="cell c-actions"><button class="btn btn-outline" data-act="refresh">${ICONS.refreshCw}Retry</button></div></div>`).join('');
}

function emptyState(model) {
  const title = model.source === 'bitbucket' ? 'No pull requests' : model.source === 'jira' ? 'No issues' : 'Nothing matches';
  const desc = model.source === 'bitbucket' ? 'No Bitbucket pull requests match this filter.' : model.source === 'jira' ? 'No Jira issues match this filter.' : model.repoCount ? 'Try another preset or clear the query.' : 'Add a GitHub repo in Task sources or open a workspace with a GitHub remote.';
  return `<div class="empty"><div class="empty-title">${esc(title)}</div><div class="muted">${esc(desc)}</div></div>`;
}

function listBanner(model) {
  const parts = [];
  if (model.banner) parts.push(`<div class="banner banner-${esc(model.banner.kind || 'warn')}" role="status">${ICONS.alertCircle}<span class="truncate" title="${esc(model.banner.text)}">${esc(model.banner.text)}</span>${model.banner.action ? `<button class="btn btn-outline" data-act="${esc(model.banner.action.act)}" data-source="${esc(model.source)}">${esc(model.banner.action.label)}</button>` : ''}</div>`);
  if (!model.loading && model.failedRepos > 0 && model.repoCount > 0 && model.items.length) {
    parts.push(`<div class="banner banner-warn" role="status">${ICONS.alertCircle}<span>${model.failedRepos} of ${model.repoCount} repos failed</span><button class="btn btn-outline" data-act="refresh">Retry</button></div>`);
  }
  if (!model.loading && model.hardError) parts.push(`<div class="banner banner-error" role="alert">${ICONS.alertCircle}<span class="truncate" title="${esc(model.hardError)}">${esc(model.hardError)}</span><button class="btn btn-outline" data-act="refresh">Retry</button></div>`);
  return parts.join('');
}

function pagination(model) {
  const total = model.totalPages || 1;
  if (total <= 1) return '';
  const current = (model.page || 1) - 1;
  const loading = model.loadingPage != null ? model.loadingPage - 1 : null;
  const nums = getPageNumbers(current, total)
    .map((p, i) => (p === 'ellipsis' ? `<span class="page-ellipsis" aria-hidden="true">…</span>` : `<button class="page-num${p === current ? ' active' : ''}" data-page="${p + 1}"${loading !== null && loading !== p ? ' disabled' : ''}${p === current ? ' aria-current="page"' : ''} aria-label="Page ${p + 1}">${loading === p ? `<span class="spin">${ICONS.loaderCircle}</span>` : p + 1}</button>`))
    .join('');
  return `<nav class="pagination" aria-label="Pagination"><button class="page-btn" data-page="${current}"${current === 0 || loading !== null ? ' disabled' : ''}>${ICONS.chevronLeft}Previous</button>${nums}<button class="page-btn" data-page="${current + 2}"${current >= total - 1 || loading !== null ? ' disabled' : ''}>Next${ICONS.chevronRight}</button></nav>`;
}

function sourceBar(model) {
  return `<div class="source-bar" role="tablist">${model.sources
    .map((s) => `<button class="source-btn${s.id === model.source ? ' active' : ''}" role="tab" aria-selected="${s.id === model.source}" data-source="${esc(s.id)}" title="${esc(SOURCE_LABELS[s.id] || s.id)}">${ICONS[s.id] || ''}</button>`)
    .join('')}<span class="source-name">${esc(SOURCE_LABELS[model.source] || model.source)}</span><span class="spacer"></span><button class="icon-btn" data-act="toggleSettings" title="Task sources">${ICONS.settings}</button></div>`;
}

/** GitHub: repo multi-select (grouped by host) with Select all; Bitbucket: the repo list as plain chips. */
function modeControls(model) {
  const repos = model.repos || [];
  if (!repos.length) return '';
  if (model.source !== 'github') return `<div class="repo-chips">${repos.map((r) => `<span class="label-chip">${esc(r.label)}</span>`).join('')}</div>`;
  const selected = repos.filter((r) => r.selected);
  const summary = selected.length === repos.length ? `All repos (${repos.length})` : selected.length === 1 ? selected[0].label : `${selected.length} of ${repos.length} repos`;
  const allChecked = selected.length === repos.length;
  return `<details class="menu repo-menu"><summary class="btn btn-outline">${esc(summary)}${ICONS.chevronDown}</summary><div class="menu-items repo-items"><label class="menu-check"><input type="checkbox" data-repo="*"${allChecked ? ' checked' : ''}>Select all</label><div class="menu-group">${esc(model.host || 'github.com')}</div>${repos.map((r) => `<label class="menu-check"><input type="checkbox" data-repo="${esc(r.label)}"${r.selected ? ' checked' : ''}>${esc(r.label)}</label>`).join('')}</div></details>`;
}

function filters(model) {
  const chips = (model.presets || [])
    .map((p) => `<button class="chip${p.id === model.activePreset ? ' active' : ''}" data-preset="${esc(p.id)}" title="${esc(p.query)} — right-click to make default">${p.id === model.defaultPreset ? '<span class="default-dot" title="Default"></span>' : ''}${esc(p.label)}</button>`)
    .join('');
  const scope = model.source === 'github' ? `<select class="select" data-filter="scope" aria-label="Type"><option value="all"${model.scope === 'all' ? ' selected' : ''}>Issues and PRs</option><option value="issue"${model.scope === 'issue' ? ' selected' : ''}>Issues</option><option value="pr"${model.scope === 'pr' ? ' selected' : ''}>Pull requests</option></select>` : '';
  const stateOpts = model.source === 'jira' ? [['open', 'Open'], ['closed', 'Done'], ['all', 'All']] : [['open', 'Open'], ['closed', model.source === 'bitbucket' ? 'Declined' : 'Closed'], ['merged', 'Merged'], ['all', 'All']];
  const state = `<select class="select" data-filter="state" aria-label="State">${stateOpts.map(([v, l]) => `<option value="${v}"${(model.state || 'open') === v ? ' selected' : ''}>${l}</option>`).join('')}</select>`;
  const newIssue = model.source === 'github' && (model.repos || []).some((r) => r.selected) ? `<button class="btn btn-outline" data-act="newIssue" title="New issue">${ICONS.plus}New issue</button>` : '';
  const viewToggle = `<div class="segmented" role="group" aria-label="View"><button class="seg${model.viewMode !== 'board' ? ' active' : ''}" data-view-mode="list" title="List">${ICONS.list}</button><button class="seg${model.viewMode === 'board' ? ' active' : ''}" data-view-mode="board" title="Board">${ICONS.columns}</button></div>`;
  return `<div class="filters"><div class="chips">${chips}</div><div class="filter-row">${viewToggle}${scope}${state}<label class="search"><span class="search-icon">${ICONS.search}</span><input id="query" type="text" value="${esc(model.query || '')}" placeholder="${model.source === 'bitbucket' ? 'Search pull requests…' : model.source === 'jira' ? 'Search issues… (project:KEY, status:Blocked, DIGITALBTD-1)' : 'Search issues and pull requests…'}" spellcheck="false" autocomplete="off"><button class="clear${model.query ? '' : ' hidden'}" id="clear" aria-label="Clear">${ICONS.x}</button></label>${newIssue}<button class="icon-btn${model.loading || model.loadingPage != null ? ' spin' : ''}" data-act="refresh" title="Refresh">${ICONS.refreshCw}</button></div></div>`;
}

function statusPill(status) {
  if (!status) return pill('muted', 'Checking…');
  if (status.ok) return pill('emerald', 'Connected', status.login || status.user ? `Connected as ${status.login || status.user}` : 'Connected');
  return pill('danger', 'Disconnected', status.message || status.reason || 'Not connected');
}

/** Task sources cards: status pill, Show-in-Tasks toggle (locked on the last visible one), default radio, repo list. */
function settingsSection(model) {
  const s = model.settings || {};
  const visibleCount = model.sources.length;
  const card = (id, cfg) => {
    const c = cfg || {};
    const lockVisible = c.visible && visibleCount <= 1;
    const repos = (c.repos || []).length ? c.repos.map((r) => `<span class="label-chip">${esc(r)}</span>`).join('') : `<span class="muted">${id === 'jira' ? 'every project' : 'workspace remotes only'}</span>`;
    const signIn = c.status && !c.status.ok ? `<button class="btn btn-outline" data-act="signIn" data-source="${id}">${id === 'github' ? 'gh auth login' : 'Set token'}</button>` : '';
    return `<div class="card"><div class="card-head">${ICONS[id]}<span class="card-title">${esc(SOURCE_LABELS[id])}</span>${statusPill(c.status)}${signIn}</div>
      <div class="card-row"><label class="menu-check"><input type="checkbox" data-visible="${id}"${c.visible ? ' checked' : ''}${lockVisible ? ' disabled title="The last visible source stays on"' : ''}>Show in Tasks</label><label class="menu-check"><input type="radio" name="defaultSource" data-default="${id}"${s.defaultSource === id ? ' checked' : ''}>Default</label></div>
      <div class="card-row repos">${repos}</div>${c.baseUrl ? `<div class="card-row muted truncate">${esc(c.baseUrl)}</div>` : ''}</div>`;
  };
  const net = s.tailscale || {};
  const netPill = !net.status ? pill('muted', 'Checking…') : !net.status.ok ? pill('danger', 'Disconnected', net.status.message) : net.status.exitNode ? pill('emerald', `Exit node: ${net.status.exitNode.hostName}`, net.status.exitNode.dnsName) : pill('muted', 'Direct', 'No exit node in use');
  const netCard = `<div class="card"><div class="card-head">${ICONS.globe}<span class="card-title">Tailscale</span>${netPill}</div><div class="card-row"><button class="btn btn-outline" data-act="exitNode"${net.status && net.status.ok ? '' : ' disabled'}>${ICONS.globe}Exit node…</button>${net.status && net.status.ok && net.status.exitNode ? `<button class="btn btn-outline" data-act="exitNodeOff">${ICONS.x}Direct</button>` : ''}</div><div class="card-row muted truncate">${esc(net.status && net.status.self ? net.status.self : 'VPN-style routing through a tailnet exit node')}</div></div>`;
  return `<details class="settings" id="settings"${s.expanded ? ' open' : ''}><summary class="section-header">TASK SOURCES</summary><div class="cards">${card('jira', s.jira)}${card('github', s.github)}${card('bitbucket', s.bitbucket)}${netCard}</div><div class="card-row"><button class="btn btn-outline" data-act="openSettings">${ICONS.settings}Edit repos and defaults in Settings</button><button class="btn btn-outline" data-act="doctor">${ICONS.refreshCw}Re-check connections</button></div></details>`;
}

// --- board view (Orca ProjectBoard / task-page-jira-board): columns of cards over the same rows ---
function boardCard(item, model) {
  const rowError = model.rowErrors && model.rowErrors[item.id];
  const errorLine = rowError ? `<div class="row-error">${ICONS.alertCircle}<span class="truncate" title="${esc(rowError)}">${esc(rowError)}</span><button class="btn btn-outline" data-act="retry" data-id="${esc(item.id)}">Retry</button><button class="icon-btn" data-act="clearError" data-id="${esc(item.id)}" aria-label="Dismiss">${ICONS.x}</button></div>` : '';
  const labels = (item.labels || []).slice(0, 2).map((l) => `<span class="label-chip">${esc(l)}</span>`).join('') + ((item.labels || []).length > 2 ? `<span class="more">+${item.labels.length - 2}</span>` : '');
  const who = item.source === 'jira' ? (item.assignee ? item.assignee.displayName : '') : (item.assignees || [])[0] || item.author || '';
  const face = who ? `<span class="avatar ring-muted" title="${esc(who)}">${esc(initials(who))}</span>` : '';
  const prio = item.priority ? `<span class="cell-text muted">${remoteIcon(item.priority, 'type-icon')}${esc(item.priority.name)}</span>` : '';
  const checks = item.type === 'pr' && item.checks && item.checks.state !== 'loading' && item.checks.state !== 'none' ? `<span class="cell-text tone-${item.checks.state === 'success' ? 'emerald' : item.checks.state === 'failed' ? 'danger' : 'amber'}">${item.checks.state === 'success' ? ICONS.checkCircle2 : item.checks.state === 'failed' ? ICONS.alertCircle : ICONS.clock3}${esc(item.checks.label)}</span>` : '';
  const moves = (model.board || []).filter((c) => c.key !== item.boardColumn).map((c) => `<button data-move-to="${esc(c.key)}" data-id="${esc(item.id)}">${ICONS.arrowRight}Move to ${esc(c.label)}</button>`).join('');
  const menu = `<details class="menu card-menu"><summary class="icon-btn" aria-label="Card actions">${ICONS.ellipsisVertical}</summary><div class="menu-items">${moves}<div class="menu-sep"></div><button data-act="start" data-id="${esc(item.id)}">${ICONS.play}Start</button><button data-url="${esc(item.url)}">${ICONS.externalLink}Open in browser</button></div></details>`;
  const busy = model.pendingIds && model.pendingIds.includes(item.id);
  return `<div class="card board-card${busy ? ' busy' : ''}" draggable="${busy ? 'false' : 'true'}" role="button" tabindex="0" data-id="${esc(item.id)}" data-url="${esc(item.url)}"><div class="card-top">${idPill(item)}${prio}<span class="spacer"></span>${busy ? `<span class="spin">${ICONS.loaderCircle}</span>` : ''}${menu}</div><div class="card-title">${esc(item.title)}</div>${errorLine}<div class="card-foot">${face}${checks}${labels}<span class="spacer"></span><span class="muted" title="${esc(item.updatedAt)}">${esc(formatRelativeTime(item.updatedAt, model.now))}</span>${model.repoCount > 1 ? `<span class="repo-badge"><span class="repo-dot"></span>${esc(item.repo.name)}</span>` : ''}</div></div>`;
}

function boardView(model) {
  const cols = model.board || [];
  if (model.loading) return `<div class="board">${Array.from({ length: 3 }, () => `<div class="board-col"><div class="board-head"><span class="shimmer" style="width:40%"></span></div><div class="board-body">${'<div class="card board-card skeleton"><span class="shimmer wide"></span><span class="shimmer"></span></div>'.repeat(3)}</div></div>`).join('')}</div>`;
  if (!cols.length) return emptyState(model);
  return `<div class="board">${cols
    .map((c) => `<div class="board-col" data-col="${esc(c.key)}"><div class="board-head"><button class="icon-btn collapse" data-collapse="${esc(c.key)}" aria-label="Collapse">${ICONS.chevronDown}</button><span class="board-label">${esc(c.label)}</span><span class="count">${c.rows.length}</span></div><div class="board-body">${c.rows.map((r) => boardCard({ ...r, boardColumn: c.key }, model)).join('') || '<div class="board-empty muted">Drop here</div>'}</div></div>`)
    .join('')}</div>`;
}

// --- inline detail view (Orca swaps the list for the item; Back returns) ---
function jiraStatusTone(category) {
  return category === 'done' ? 'emerald' : category === 'indeterminate' ? 'sky' : 'muted';
}

function transitionMenu(d) {
  const items = (d.transitions || [])
    .map((t) => {
      const req = t.requirement || { kind: 'none' };
      const form = req.kind === 'form' ? attr({ id: t.id, name: t.name, resolution: req.resolution ? { name: req.resolution.name, options: (req.resolution.allowedValues || []).map((v) => ({ id: v.id || v.value || v.name, label: v.name || v.value || v.id })) } : null, commentRequired: !!req.commentRequired }) : '';
      const note = req.kind === 'unsupported' ? ` <span class="muted">(needs ${esc((req.fields || []).map((f) => f.name || f.key).join(', '))} — open in Jira)</span>` : '';
      return `<button data-transition="${esc(t.id)}" data-req="${esc(req.kind)}"${form ? ` data-form="${form}"` : ''}${req.kind === 'unsupported' ? ' disabled' : ''}>${ICONS.arrowRight}${esc(t.name)}${t.to.name && t.to.name.toLowerCase() !== t.name.toLowerCase() ? `<span class="muted"> → ${esc(t.to.name)}</span>` : ''}${note}</button>`;
    })
    .join('');
  const st = d.status || { name: 'Open', category: 'new' };
  return `<details class="menu"><summary class="pill pill-${jiraStatusTone(st.category)} pill-btn" title="Change status">${esc(st.name)}${ICONS.chevronDown}</summary><div class="menu-items">${items || '<span class="menu-note muted">No transitions available</span>'}</div></details>`;
}

function priorityMenu(d) {
  const current = d.priority ? `${remoteIcon(d.priority, 'type-icon')}${esc(d.priority.name)}` : '<span class="muted">No priority</span>';
  const items = (d.priorities || []).map((p) => `<button data-priority="${esc(p.id)}"${d.priority && d.priority.name === p.name ? ' class="active"' : ''}>${remoteIcon(p, 'type-icon')}${esc(p.name)}</button>`).join('');
  return `<details class="menu"><summary class="btn btn-outline" title="Change priority">${current}${ICONS.chevronDown}</summary><div class="menu-items">${items || '<span class="menu-note muted">Loading…</span>'}</div></details>`;
}

function assigneeButton(d) {
  const a = d.assignee;
  const face = a ? `<span class="avatar ring-muted">${esc(initials(a.displayName))}</span>${esc(a.displayName)}` : '<span class="muted">Unassigned</span>';
  return `<button class="btn btn-outline" data-act="assign" data-id="${esc(d.id)}" title="Change assignee">${face}${ICONS.chevronDown}</button>`;
}

function commentBubble(c, now) {
  return `<div class="bubble"><div class="bubble-head"><span class="avatar ring-muted">${esc(initials(c.author))}</span><span class="bubble-author">${esc(c.author)}</span><span class="muted" title="${esc(c.created)}">${esc(formatRelativeTime(c.created, now))}</span></div><div class="prose">${c.bodyHtml || ''}</div></div>`;
}

function transitionForm() {
  return `<form class="card transition-form hidden" id="transition-form"><div class="card-head"><span class="card-title" id="transition-title">Transition</span></div><label class="field hidden" id="resolution-field"><span>Resolution</span><select class="select" id="resolution"></select></label><label class="field" id="transition-comment-field"><span id="transition-comment-label">Comment</span><textarea id="transition-comment" rows="2" placeholder="Optional comment"></textarea></label><div class="card-row"><button type="submit" class="btn btn-primary">Apply</button><button type="button" class="btn btn-outline" id="transition-cancel">Cancel</button></div></form>`;
}

function reviewTone(state) {
  return state === 'APPROVED' ? 'emerald' : state === 'CHANGES_REQUESTED' ? 'danger' : 'muted';
}

function activityEntry(e, now) {
  if (e.kind === 'event') {
    const text = e.url ? `<a data-url="${esc(e.url)}">${esc(e.text)}</a>` : esc(e.text);
    return `<div class="timeline-row"><span class="avatar ring-muted">${esc(initials(e.actor || '?'))}</span><span><b>${esc(e.actor || '')}</b> ${text}</span><span class="muted" title="${esc(e.createdAt)}">${esc(formatRelativeTime(e.createdAt, now))}</span></div>`;
  }
  const badge = e.kind === 'review' ? pill(reviewTone(e.state), String(e.state || '').toLowerCase().replace('_', ' ')) : '';
  return `<div class="bubble"><div class="bubble-head"><span class="avatar ring-${e.kind === 'review' ? reviewTone(e.state) : 'muted'}">${esc(initials(e.actor || '?'))}</span><span class="bubble-author">${esc(e.actor || '')}</span>${badge}<span class="muted" title="${esc(e.createdAt)}">${esc(formatRelativeTime(e.createdAt, now))}</span>${e.url ? `<span class="spacer"></span><button class="icon-btn" data-url="${esc(e.url)}" title="Open on GitHub">${ICONS.externalLink}</button>` : ''}</div>${e.bodyHtml ? `<div class="prose">${e.bodyHtml}</div>` : ''}</div>`;
}

function checksTab(d) {
  const c = d.checks;
  if (!c) return `<section class="card"><div class="card-head"><span class="card-title">Checks</span><span class="spin">${ICONS.loaderCircle}</span></div></section>`;
  const rows = (c.runs || []).map((r) => {
    const tone = r.state === 'success' ? 'emerald' : r.state === 'failed' ? 'danger' : r.state === 'in_progress' ? 'amber' : 'muted';
    const icon = r.state === 'success' ? ICONS.checkCircle2 : r.state === 'failed' ? ICONS.alertCircle : r.state === 'in_progress' ? ICONS.clock3 : ICONS.minus;
    return `<div class="check-row"><span class="cell-text tone-${tone}">${icon}${esc(r.name)}</span><span class="muted">${esc(r.app)}</span>${pill(tone, r.conclusion || r.status)}<span class="spacer"></span>${r.url ? `<button class="icon-btn" data-url="${esc(r.url)}" title="Open run">${ICONS.externalLink}</button>` : ''}</div>`;
  });
  return `<section class="card"><div class="card-head"><span class="card-title">Checks</span><span class="count">${c.total || 0}</span></div>${rows.join('') || '<div class="muted">No checks</div>'}</section>`;
}

/** GitHub detail (Orca github-item-dialog): header, status/assignee controls, PR tabs, body, activity, composer, rail. */
function githubDetail(model, d, data, head, title, err, now) {
  const isPr = data.type === 'pr';
  const conv = d.conversation || null;
  const controls = isPr
    ? `${reviewersCellFor(data)}${checksCell(data)}${mergeCell(data, model)}`
    : `${statusCell(data)}${assigneesCell(data)}`;
  const metaBar = `<div class="meta-bar">${controls}${(data.labels || []).map((l) => `<span class="label-chip">${esc(l)}</span>`).join('')}${isPr && data.branch ? `<span class="muted cell-text">${ICONS.gitBranch}${esc(data.baseBranch || '')} ← ${esc(data.branch)}</span>` : ''}${deltaText(data) ? `<span class="muted cell-text">${ICONS.files}${esc(deltaText(data))}</span>` : ''}</div>`;
  const tab = d.tab || 'conversation';
  const tabs = isPr ? `<div class="tabs" role="tablist"><button class="tab${tab === 'conversation' ? ' active' : ''}" role="tab" data-tab="conversation">Conversation${conv ? ` <span class="count">${conv.commentCount}</span>` : ''}</button><button class="tab${tab === 'checks' ? ' active' : ''}" role="tab" data-tab="checks">Checks${data.checks && data.checks.counts ? ` <span class="count">${(data.checks.counts.success || 0) + (data.checks.counts.failed || 0) + (data.checks.counts.pending || 0)}</span>` : ''}</button><button class="tab" role="tab" data-tab="files" data-url="${esc(data.url)}/files">Files${data.changedFiles != null ? ` <span class="count">${data.changedFiles}</span>` : ''} ${ICONS.externalLink}</button></div>` : '';
  let main;
  if (tab === 'checks' && isPr) main = checksTab(d);
  else if (!conv) main = `<section class="card"><div class="card-head"><span class="spin">${ICONS.loaderCircle}</span><span class="muted">Loading conversation…</span></div></section>`;
  else {
    const body = `<section class="card"><div class="card-head"><span class="card-title">${esc(conv.author || data.author || '')}</span><span class="muted" title="${esc(conv.createdAt)}">opened ${esc(formatRelativeTime(conv.createdAt, now))}</span></div>${conv.bodyHtml ? `<div class="prose">${conv.bodyHtml}</div>` : '<div class="muted">No description provided.</div>'}</section>`;
    const activity = `<section class="card"><div class="card-head"><span class="card-title">Activity</span><span class="count">${conv.entries.length}</span></div>${conv.entries.map((e) => activityEntry(e, now)).join('') || '<div class="muted">No activity yet.</div>'}<form class="composer" id="composer"><textarea id="comment-body" rows="2" placeholder="Leave a comment…"></textarea><div class="card-row"><button type="submit" class="btn btn-primary" id="comment-send" disabled>${ICONS.send}Comment</button><span class="muted">Markdown · ⌘/Ctrl+Enter</span></div></form></section>`;
    main = body + activity;
  }
  const rail = `<aside class="rail"><button class="btn btn-outline" data-url="${esc(data.url)}">${ICONS.externalLink}Open in GitHub</button><button class="btn btn-outline" data-act="copy" data-text="${esc(data.url)}" data-label="URL">${ICONS.copy}Copy URL</button>${isPr && data.branch ? `<button class="btn btn-outline" data-act="copy" data-text="${esc(data.branch)}" data-label="Branch name">${ICONS.gitBranch}Copy branch name</button>` : ''}<button class="btn btn-outline" data-act="copy" data-text="${esc(d.prompt || '')}" data-label="Prompt">${ICONS.copy}Copy prompt</button></aside>`;
  return `<div class="detail" data-detail-id="${esc(data.id)}">${head}${title}${metaBar}${err}${tabs}<div class="detail-body"><div class="detail-main">${main}</div>${rail}</div></div>`;
}

/** Full-source detail for Jira; GitHub gets the conversation view; Bitbucket the header + browser link. */
function detailView(model, now) {
  const d = model.detail || {};
  const data = d.data || {};
  const back = `<button class="btn btn-outline" data-act="back">${ICONS.chevronLeft}Back</button>`;
  if (d.loading) return `<div class="detail"><div class="detail-head">${back}<span class="spin">${ICONS.loaderCircle}</span><span class="muted">Loading ${esc(d.key || '')}…</span></div></div>`;
  if (d.error) return `<div class="detail"><div class="detail-head">${back}</div><div class="banner banner-error" role="alert">${ICONS.alertCircle}<span>${esc(d.error)}</span><button class="btn btn-outline" data-act="detail" data-id="${esc(d.id)}">Retry</button></div></div>`;
  const key = data.source === 'jira' ? data.key : `#${data.number}`;
  const head = `<div class="detail-head">${back}<span class="mono detail-key">${esc(key)}</span><span class="muted">${esc(data.repo ? data.repo.label : '')}</span><span class="muted" title="${esc(data.updatedAt)}">updated ${esc(formatRelativeTime(data.updatedAt, now))}</span><span class="spacer"></span>${d.pending ? `<span class="spin">${ICONS.loaderCircle}</span>` : ''}<button class="btn btn-outline" data-act="start" data-id="${esc(data.id)}">${ICONS.play}Start</button><button class="icon-btn" data-act="open" data-url="${esc(data.url)}" title="Open in browser">${ICONS.externalLink}</button></div>`;
  const title = `<h2 class="detail-title">${esc(data.title)}</h2>`;
  const err = d.mutationError ? `<div class="banner banner-error" role="alert">${ICONS.alertCircle}<span class="truncate" title="${esc(d.mutationError)}">${esc(d.mutationError)}</span><button class="btn btn-outline" data-act="retry" data-id="${esc(data.id)}">Retry</button><button class="icon-btn" data-act="clearError" data-id="${esc(data.id)}" aria-label="Dismiss">${ICONS.x}</button></div>` : '';
  if (data.source === 'bitbucket') {
    return `<div class="detail">${head}${title}<div class="meta-bar">${reviewersCell(data)}${checksCell(data)}${mergeCell(data, model)}</div>${err}<div class="card"><div class="muted">Comments and approvals for Bitbucket pull requests open in the browser for now.</div><div class="card-row"><button class="btn btn-outline" data-act="open" data-url="${esc(data.url)}">${ICONS.externalLink}Open in Bitbucket</button></div></div></div>`;
  }
  if (data.source === 'github') return githubDetail(model, d, data, head, title, err, now);
  const metaBar = `<div class="meta-bar">${transitionMenu({ ...data, transitions: d.transitions })}${priorityMenu({ ...data, priorities: d.priorities })}${assigneeButton(data)}${(data.labels || []).map((l) => `<span class="label-chip">${esc(l)}</span>`).join('')}${data.issueType ? `<span class="muted cell-text">${remoteIcon(data.issueType, 'type-icon')}${esc(data.issueType.name)}</span>` : ''}</div>`;
  const description = `<section class="card"><div class="card-head"><span class="card-title">Description</span><span class="muted">${esc(data.author || '')}</span></div>${data.descriptionHtml ? `<div class="prose">${data.descriptionHtml}</div>` : '<div class="muted">No description provided.</div>'}</section>`;
  const comments = d.comments || [];
  const commentsCard = `<section class="card"><div class="card-head"><span class="card-title">Comments</span><span class="count">${comments.length}</span></div>${comments.map((c) => commentBubble(c, now)).join('') || '<div class="muted">No comments yet.</div>'}<form class="composer" id="composer"><textarea id="comment-body" rows="2" placeholder="Add a comment…"></textarea><div class="card-row"><button type="submit" class="btn btn-primary" id="comment-send" disabled>${ICONS.send}Send</button><span class="muted">Plain text or Jira wiki markup</span></div></form></section>`;
  const rail = `<aside class="rail"><button class="btn btn-outline" data-act="open" data-url="${esc(data.url)}">${ICONS.externalLink}Open in Jira</button><button class="btn btn-outline" data-act="copy" data-text="${esc(data.url)}" data-label="URL">${ICONS.copy}Copy URL</button><button class="btn btn-outline" data-act="copy" data-text="${esc(data.key)}" data-label="Key">${ICONS.copy}Copy key</button><button class="btn btn-outline" data-act="copy" data-text="${esc(d.branchName || '')}" data-label="Branch name">${ICONS.gitBranch}Copy branch name</button><button class="btn btn-outline" data-act="copy" data-text="${esc(d.prompt || '')}" data-label="Prompt">${ICONS.copy}Copy prompt</button></aside>`;
  return `<div class="detail" data-detail-id="${esc(data.id)}">${head}${title}${metaBar}${err}${transitionForm()}<div class="detail-body"><div class="detail-main">${description}${commentsCard}</div>${rail}</div></div>`;
}

const CSS = `
  :root { --track: color-mix(in srgb, var(--vscode-foreground) 14%, transparent); --border: color-mix(in srgb, var(--vscode-widget-border, var(--vscode-foreground)) 60%, transparent); --hover: var(--vscode-list-hoverBackground); --bg: var(--vscode-sideBar-background); --emerald: #34d399; --amber: #fbbf24; --danger: var(--vscode-editorError-foreground, #f87171); --primary: var(--vscode-button-background); }
  * { box-sizing: border-box; }
  body { font-family: var(--vscode-font-family); font-size: 13px; color: var(--vscode-foreground); background: var(--bg); margin: 0; height: 100vh; display: flex; flex-direction: column; overflow: hidden; }
  svg { width: 14px; height: 14px; flex: none; }
  .muted { color: var(--vscode-descriptionForeground); }
  .truncate { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .mono { font-family: var(--vscode-editor-font-family, monospace); font-size: 11px; }
  .spacer { flex: 1; }
  .hidden { display: none !important; }
  button { font: inherit; color: inherit; }
  .chrome { flex: none; display: flex; flex-direction: column; gap: 8px; padding: 8px 12px; }
  .source-bar { display: flex; align-items: center; gap: 6px; }
  .source-btn { width: 32px; height: 32px; display: inline-flex; align-items: center; justify-content: center; border-radius: 6px; border: 1px solid transparent; background: none; color: var(--vscode-descriptionForeground); cursor: pointer; }
  .source-btn svg { width: 16px; height: 16px; }
  .source-btn:hover { background: var(--hover); color: var(--vscode-foreground); }
  .source-btn.active { border-color: color-mix(in srgb, var(--vscode-foreground) 40%, transparent); background: color-mix(in srgb, var(--track) 70%, transparent); color: var(--vscode-foreground); }
  .source-name { font-weight: 600; margin-left: 4px; }
  .icon-btn { background: none; border: none; color: var(--vscode-foreground); cursor: pointer; padding: 4px; border-radius: 4px; display: inline-flex; }
  .icon-btn:hover { background: var(--hover); }
  .banner { display: flex; align-items: center; gap: 8px; max-width: 48rem; border: 1px solid var(--border); border-radius: 6px; padding: 6px 10px; font-size: 12px; color: var(--vscode-descriptionForeground); background: color-mix(in srgb, var(--track) 30%, transparent); }
  .banner-error { border-color: var(--danger); color: var(--vscode-foreground); }
  .filters { display: flex; flex-direction: column; gap: 8px; }
  .chips { display: flex; flex-wrap: wrap; gap: 6px; }
  .chip { display: inline-flex; align-items: center; gap: 5px; border-radius: 999px; border: 1px solid var(--border); background: none; padding: 2px 10px; font-size: 12px; cursor: pointer; color: var(--vscode-descriptionForeground); }
  .chip:hover { background: var(--hover); color: var(--vscode-foreground); }
  .chip.active { background: var(--primary); color: var(--vscode-button-foreground); border-color: transparent; }
  .default-dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
  .filter-row { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
  .select { background: var(--vscode-dropdown-background); color: var(--vscode-dropdown-foreground); border: 1px solid var(--vscode-dropdown-border, var(--border)); border-radius: 4px; padding: 3px 6px; font: inherit; font-size: 12px; }
  .search { position: relative; display: flex; align-items: center; flex: 1 1 200px; min-width: 160px; }
  .search-icon { position: absolute; left: 8px; color: var(--vscode-descriptionForeground); display: inline-flex; }
  .search input { width: 100%; padding: 4px 26px 4px 28px; border-radius: 4px; border: 1px solid var(--vscode-input-border, var(--border)); background: var(--vscode-input-background); color: var(--vscode-input-foreground); font: inherit; font-size: 12px; }
  .search input:focus { outline: 1px solid var(--vscode-focusBorder); }
  .clear { position: absolute; right: 4px; background: none; border: none; cursor: pointer; color: var(--vscode-descriptionForeground); display: inline-flex; padding: 2px; }
  .btn { display: inline-flex; align-items: center; gap: 4px; border-radius: 6px; padding: 3px 8px; font-size: 12px; cursor: pointer; border: 1px solid transparent; background: var(--primary); color: var(--vscode-button-foreground); white-space: nowrap; }
  .btn-outline { background: none; border-color: var(--border); color: var(--vscode-foreground); }
  .btn-outline:hover { background: var(--hover); }
  .btn-icon { padding: 3px 4px; }
  .repo-chips { display: flex; flex-wrap: wrap; gap: 6px; }
  .menu { position: relative; display: inline-flex; }
  .menu > summary { list-style: none; }
  .menu > summary::-webkit-details-marker { display: none; }
  .menu-items { position: absolute; top: calc(100% + 4px); right: 0; z-index: 50; min-width: 180px; background: var(--vscode-menu-background, var(--vscode-dropdown-background, var(--bg))); color: var(--vscode-menu-foreground, var(--vscode-foreground)); border: 1px solid var(--border); border-radius: 6px; padding: 4px; box-shadow: 0 4px 16px rgba(0,0,0,.3); display: flex; flex-direction: column; }
  .repo-menu .menu-items { left: 0; right: auto; max-height: 240px; overflow: auto; }
  .menu-items button, .menu-check { display: flex; align-items: center; gap: 6px; padding: 5px 8px; border: none; background: none; text-align: left; border-radius: 4px; cursor: pointer; font-size: 12px; white-space: nowrap; }
  .menu-items button:hover, .menu-check:hover { background: var(--hover); }
  .menu-check input { appearance: none; -webkit-appearance: none; flex: none; width: 14px; height: 14px; margin: 0; display: inline-grid; place-content: center; border: 1px solid var(--vscode-checkbox-border, var(--border)); background: var(--vscode-checkbox-background, transparent); cursor: pointer; }
  .menu-check input[type="checkbox"] { border-radius: 3px; }
  .menu-check input[type="radio"] { border-radius: 50%; }
  .menu-check input:checked { background: var(--primary); border-color: var(--primary); }
  .menu-check input[type="checkbox"]:checked::after { content: ''; width: 4px; height: 8px; margin-top: -2px; border: solid var(--vscode-button-foreground, #fff); border-width: 0 2px 2px 0; transform: rotate(45deg); }
  .menu-check input[type="radio"]:checked::after { content: ''; width: 6px; height: 6px; border-radius: 50%; background: var(--vscode-button-foreground, #fff); }
  .menu-check input:focus-visible { outline: 1px solid var(--vscode-focusBorder); outline-offset: 1px; }
  .menu-check input:disabled { opacity: .5; cursor: default; }
  .card .menu-check { padding: 2px 4px; }
  .menu-group { padding: 4px 8px; font-size: 10px; letter-spacing: .08em; text-transform: uppercase; color: var(--vscode-descriptionForeground); }
  .list { flex: 1 1 auto; min-height: 0; overflow: auto; position: relative; }
  .grid { min-width: max-content; }
  .row { display: grid; grid-template-columns: var(--grid); gap: 12px; padding: 10px 12px; min-height: 48px; align-items: stretch; border-bottom: 1px solid color-mix(in srgb, var(--border) 40%, transparent); background: var(--bg); cursor: pointer; }
  .row:hover { background: var(--hover); }
  .row:focus-visible { outline: 2px solid var(--vscode-focusBorder); outline-offset: -2px; }
  .row.head { position: sticky; top: 0; z-index: 30; min-height: 0; padding: 6px 12px; font-size: 11px; font-weight: 600; letter-spacing: .06em; text-transform: uppercase; color: var(--vscode-descriptionForeground); cursor: default; }
  .row.head:hover { background: var(--bg); }
  .row.head .c-id, .row.head .c-title { z-index: 30; }
  .cell { min-width: 0; display: flex; align-items: center; }
  .c-id, .c-title { position: sticky; z-index: 20; background: inherit; }
  .c-id { left: 0; }
  .c-title { left: var(--title-left, 84px); flex-direction: column; align-items: stretch; justify-content: center; }
  .c-id::before, .c-title::before { content: ''; position: absolute; top: 0; bottom: 0; width: 12px; background: inherit; }
  .c-id::before { left: -12px; }
  .c-title::before { left: -12px; }
  .c-title::after { content: ''; position: absolute; top: 0; bottom: 0; right: -12px; width: 12px; background: inherit; }
  .id-pill { display: inline-flex; align-items: center; gap: 4px; white-space: nowrap; border-radius: 6px; border: 1px solid color-mix(in srgb, var(--border) 40%, transparent); padding: 2px 6px; color: var(--vscode-descriptionForeground); }
  .id-pill svg { width: 12px; height: 12px; }
  .type-icon { width: 14px; height: 14px; flex: none; }
  .tone-emerald { color: var(--emerald); } .tone-danger { color: var(--danger); } .tone-amber { color: var(--amber); } .tone-primary { color: var(--primary); } .tone-muted { color: var(--vscode-descriptionForeground); }
  .title-row { display: flex; align-items: center; gap: 8px; min-width: 0; }
  .title { margin: 0; font-size: 13px; font-weight: 500; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--vscode-foreground); }
  .meta { margin-top: 2px; display: flex; flex-wrap: wrap; align-items: center; gap: 2px 10px; font-size: 12px; color: var(--vscode-descriptionForeground); }
  .meta-icon { display: inline-flex; align-items: center; gap: 4px; }
  .meta-icon svg { width: 12px; height: 12px; }
  .repo-badge { display: inline-flex; align-items: center; gap: 4px; font-size: 11px; color: var(--vscode-descriptionForeground); flex: none; }
  .repo-dot { width: 6px; height: 6px; border-radius: 50%; background: var(--vscode-charts-blue, var(--primary)); }
  .label-chip, .jira-chip { border-radius: 999px; border: 1px solid color-mix(in srgb, var(--border) 40%, transparent); background: color-mix(in srgb, var(--track) 30%, transparent); padding: 0 6px; font-size: 10px; color: var(--vscode-descriptionForeground); white-space: nowrap; }
  .jira-chip { cursor: pointer; color: var(--vscode-textLink-foreground); }
  .pill { display: inline-flex; align-items: center; gap: 4px; border-radius: 999px; border: 1px solid var(--border); padding: 1px 8px; font-size: 10px; font-weight: 500; white-space: nowrap; }
  .pill svg { width: 10px; height: 10px; }
  .pill-emerald { border-color: color-mix(in srgb, var(--emerald) 40%, transparent); background: color-mix(in srgb, var(--emerald) 10%, transparent); color: var(--emerald); }
  .pill-primary { border-color: color-mix(in srgb, var(--primary) 40%, transparent); background: color-mix(in srgb, var(--primary) 10%, transparent); color: var(--vscode-foreground); }
  .pill-danger { border-color: color-mix(in srgb, var(--danger) 40%, transparent); background: color-mix(in srgb, var(--danger) 10%, transparent); color: var(--danger); }
  .pill-amber { border-color: color-mix(in srgb, var(--amber) 40%, transparent); background: color-mix(in srgb, var(--amber) 10%, transparent); color: var(--amber); }
  .pill-muted { color: var(--vscode-descriptionForeground); }
  .pill-btn { cursor: pointer; }
  .cell-text { display: inline-flex; align-items: center; gap: 5px; font-size: 12px; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .link-btn { background: none; border: none; padding: 0; cursor: pointer; }
  .reviewers { display: inline-flex; align-items: center; gap: 5px; min-width: 0; }
  .reviewer-name { font-size: 12px; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .avatar { width: 20px; height: 20px; border-radius: 50%; display: inline-flex; align-items: center; justify-content: center; font-size: 9px; font-weight: 600; background: var(--track); border: 2px solid transparent; flex: none; }
  .ring-emerald { border-color: var(--emerald); } .ring-danger { border-color: var(--danger); } .ring-muted { border-color: var(--border); }
  .more { font-size: 11px; color: var(--vscode-descriptionForeground); }
  .split { display: inline-flex; align-items: center; gap: 0; }
  .split > .btn, .split > .menu > summary { border-radius: 0; margin-left: -1px; }
  .split > :first-child { border-radius: 6px 0 0 6px; margin-left: 0; }
  .split > :last-child > summary, .split > :last-child.btn { border-radius: 0 6px 6px 0; }
  .c-actions { justify-content: flex-start; }
  .skeleton { cursor: default; }
  .shimmer { display: block; height: 12px; width: 60%; border-radius: 4px; background: linear-gradient(90deg, var(--track) 25%, color-mix(in srgb, var(--track) 50%, transparent) 50%, var(--track) 75%); background-size: 200% 100%; animation: shimmer 1.2s infinite; }
  .shimmer.wide { width: 85%; }
  @keyframes shimmer { from { background-position: 200% 0; } to { background-position: -200% 0; } }
  .spin svg { animation: spin 1s linear infinite; }
  @keyframes spin { to { transform: rotate(360deg); } }
  .error-row { cursor: default; color: var(--danger); }
  .empty { padding: 48px 16px; text-align: center; display: flex; flex-direction: column; gap: 4px; align-items: center; }
  .empty-title { font-weight: 600; }
  .pagination { flex: none; display: flex; align-items: center; justify-content: center; gap: 4px; border-top: 1px solid color-mix(in srgb, var(--border) 50%, transparent); padding: 8px 12px; }
  .page-btn { display: inline-flex; align-items: center; gap: 2px; min-width: 72px; justify-content: center; border-radius: 6px; padding: 4px 8px; background: none; border: none; color: var(--vscode-descriptionForeground); cursor: pointer; }
  .page-btn:hover, .page-num:hover { background: var(--hover); color: var(--vscode-foreground); }
  .page-btn:disabled, .page-num:disabled { opacity: .4; pointer-events: none; }
  .page-num { width: 32px; height: 32px; border-radius: 6px; border: none; background: none; color: var(--vscode-descriptionForeground); cursor: pointer; display: inline-flex; align-items: center; justify-content: center; }
  .page-num.active { background: var(--primary); color: var(--vscode-button-foreground); font-weight: 500; }
  .page-ellipsis { width: 32px; display: inline-flex; justify-content: center; color: var(--vscode-descriptionForeground); }
  .settings { flex: none; border-top: 1px solid var(--border); padding: 0 12px 8px; max-height: 45vh; overflow: auto; }
  .section-header { padding: 8px 0; font-size: 11px; font-weight: 600; letter-spacing: .08em; text-transform: uppercase; color: var(--vscode-descriptionForeground); cursor: pointer; list-style: none; }
  .section-header::-webkit-details-marker { display: none; }
  .cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 12px; margin-bottom: 8px; }
  .card { border: 1px solid var(--border); border-radius: 6px; padding: 8px; display: flex; flex-direction: column; gap: 6px; min-width: 0; }
  .card-head { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
  .card-head svg { width: 16px; height: 16px; }
  .card-title { font-weight: 600; }
  .card-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 12px; }
  .card-row.repos { gap: 4px; }
  .segmented { display: inline-flex; border: 1px solid var(--border); border-radius: 6px; overflow: hidden; }
  .seg { background: none; border: none; padding: 3px 8px; cursor: pointer; color: var(--vscode-descriptionForeground); display: inline-flex; }
  .seg.active { background: color-mix(in srgb, var(--track) 70%, transparent); color: var(--vscode-foreground); }
  .board { flex: 1; min-height: 0; display: flex; gap: 12px; overflow-x: auto; overflow-y: hidden; padding: 4px 12px 12px; align-items: stretch; }
  .board-col { flex: 1 1 272px; min-width: 240px; max-width: 420px; display: flex; flex-direction: column; min-height: 0; border: 1px solid color-mix(in srgb, var(--border) 50%, transparent); border-radius: 8px; background: color-mix(in srgb, var(--track) 20%, transparent); transition: border-color .12s, box-shadow .12s; }
  .board-col.drag-over { border-color: var(--vscode-focusBorder); box-shadow: 0 0 0 1px var(--vscode-focusBorder) inset; }
  .board-col.collapsed { flex: 0 0 44px; min-width: 44px; }
  .board-col.collapsed .board-body, .board-col.collapsed .board-label, .board-col.collapsed .count { display: none; }
  .board-col.collapsed .collapse svg { transform: rotate(-90deg); }
  .board-head { display: flex; align-items: center; gap: 6px; height: 36px; padding: 0 8px; border-bottom: 1px solid color-mix(in srgb, var(--border) 50%, transparent); font-size: 12px; font-weight: 600; }
  .board-body { flex: 1; min-height: 0; overflow-y: auto; padding: 8px; display: flex; flex-direction: column; gap: 8px; }
  .board-empty { font-size: 12px; text-align: center; padding: 16px 0; border: 1px dashed var(--border); border-radius: 6px; }
  .board-card { cursor: grab; background: var(--bg); padding: 8px; gap: 4px; }
  .board-card:active { cursor: grabbing; }
  .board-card.dragging { opacity: .5; }
  .board-card.busy { cursor: wait; opacity: .7; }
  .board-card:focus-visible { outline: 2px solid var(--vscode-focusBorder); outline-offset: -2px; }
  .card-top { display: flex; align-items: center; gap: 6px; min-width: 0; }
  .card-top .cell-text { flex: 0 1 auto; min-width: 0; }
  .card-top .id-pill { flex: none; }
  .card-title { font-size: 13px; font-weight: 500; line-height: 1.3; display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; }
  .card-foot { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; font-size: 11px; }
  .card-menu .menu-items { right: 0; min-width: 200px; }
  .cell-btn { display: inline-flex; align-items: center; gap: 4px; background: none; border: none; padding: 0; cursor: pointer; min-width: 0; }
  .cell-btn svg:last-child { width: 12px; height: 12px; color: var(--vscode-descriptionForeground); }
  .pill-menu { display: inline-flex; align-items: center; gap: 2px; cursor: pointer; list-style: none; }
  .pill-menu::-webkit-details-marker { display: none; }
  .menu-sep { height: 1px; background: var(--border); margin: 4px 0; }
  .row-error { margin-top: 4px; display: flex; align-items: center; gap: 6px; font-size: 12px; color: var(--danger); }
  .row-error .btn { padding: 0 6px; font-size: 11px; }
  .pill-sky { border-color: color-mix(in srgb, var(--vscode-charts-blue, #60a5fa) 40%, transparent); background: color-mix(in srgb, var(--vscode-charts-blue, #60a5fa) 10%, transparent); color: var(--vscode-charts-blue, #60a5fa); }
  .btn-primary { background: var(--primary); color: var(--vscode-button-foreground); border-color: transparent; }
  .btn-primary:disabled { opacity: .5; cursor: default; }
  .detail { flex: 1; min-height: 0; overflow: auto; padding: 8px 12px 16px; display: flex; flex-direction: column; gap: 10px; }
  .detail-head { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; font-size: 12px; }
  .detail-key { font-size: 12px; }
  .detail-title { margin: 0; font-size: 20px; font-weight: 600; line-height: 1.25; color: var(--vscode-foreground); }
  .meta-bar { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  .meta-bar .menu-items { left: 0; right: auto; min-width: 220px; max-height: 280px; overflow: auto; }
  .meta-bar .menu-items button.active { font-weight: 600; }
  .menu-note { display: block; padding: 6px 8px; font-size: 12px; }
  .detail-body { display: grid; grid-template-columns: minmax(0, 1fr) 200px; gap: 12px; align-items: start; }
  .detail-main { display: flex; flex-direction: column; gap: 12px; min-width: 0; }
  .rail { display: flex; flex-direction: column; gap: 6px; }
  .rail .btn { justify-content: flex-start; }
  .count { border-radius: 999px; background: var(--track); padding: 0 7px; font-size: 11px; }
  .prose { font-size: 13px; line-height: 1.55; overflow-wrap: anywhere; }
  .prose p { margin: 0 0 8px; } .prose p:last-child { margin-bottom: 0; }
  .prose pre, .prose code { font-family: var(--vscode-editor-font-family, monospace); font-size: 12px; }
  .prose pre { background: color-mix(in srgb, var(--track) 40%, transparent); padding: 8px; border-radius: 6px; overflow: auto; }
  .prose blockquote { margin: 0 0 8px; padding-left: 10px; border-left: 3px solid var(--border); color: var(--vscode-descriptionForeground); }
  .prose table { border-collapse: collapse; } .prose th, .prose td { border: 1px solid var(--border); padding: 3px 6px; font-size: 12px; }
  .prose img { max-width: 100%; }
  .prose a { color: var(--vscode-textLink-foreground); }
  .tabs { display: flex; gap: 12px; border-bottom: 1px solid color-mix(in srgb, var(--border) 60%, transparent); }
  .tab { background: none; border: none; border-bottom: 2px solid transparent; margin-bottom: -1px; padding: 6px 8px; cursor: pointer; color: var(--vscode-descriptionForeground); display: inline-flex; align-items: center; gap: 4px; }
  .tab.active { color: var(--vscode-foreground); border-bottom-color: var(--vscode-foreground); }
  .tab:hover { color: var(--vscode-foreground); }
  .timeline-row { display: flex; align-items: center; gap: 8px; padding: 4px 4px; font-size: 12px; color: var(--vscode-descriptionForeground); }
  .timeline-row b { font-weight: 600; color: var(--vscode-foreground); }
  .timeline-row a { color: var(--vscode-textLink-foreground); cursor: pointer; }
  .check-row { display: flex; align-items: center; gap: 8px; padding: 5px 0; border-bottom: 1px solid color-mix(in srgb, var(--border) 30%, transparent); font-size: 12px; }
  .check-row:last-child { border-bottom: none; }
  .bubble { border: 1px solid color-mix(in srgb, var(--border) 50%, transparent); border-radius: 6px; background: color-mix(in srgb, var(--track) 20%, transparent); margin-bottom: 8px; }
  .bubble-head { display: flex; align-items: center; gap: 8px; border-bottom: 1px solid color-mix(in srgb, var(--border) 40%, transparent); padding: 6px 10px; font-size: 12px; }
  .bubble-author { font-weight: 600; }
  .bubble .prose { padding: 8px 10px; }
  .composer, .transition-form { display: flex; flex-direction: column; gap: 6px; margin-top: 8px; }
  .composer textarea, .transition-form textarea, .field select { width: 100%; box-sizing: border-box; font: inherit; color: var(--vscode-input-foreground); background: var(--vscode-input-background); border: 1px solid var(--vscode-input-border, var(--border)); border-radius: 6px; padding: 6px 8px; resize: vertical; }
  .composer textarea:focus, .transition-form textarea:focus { outline: 1px solid var(--vscode-focusBorder); }
  .field { display: flex; flex-direction: column; gap: 4px; font-size: 12px; }
  @media (max-width: 720px) { .detail-body { grid-template-columns: 1fr; } .rail { flex-direction: row; flex-wrap: wrap; } }
  @media (max-width: 480px) { .chrome { padding: 8px; } .row { padding: 10px 8px; } .c-title { left: calc(var(--title-left, 84px) - 4px); } .page-btn { min-width: 0; } }
`;

const SCRIPT = `
  const vscode = acquireVsCodeApi();
  const post = (msg) => vscode.postMessage(msg);
  const $ = (sel, root) => (root || document).querySelector(sel);
  document.addEventListener('click', (e) => {
    const t = e.target.closest('[data-act], [data-url], [data-source], [data-preset], [data-page], .row, .board-card');
    if (!t) return;
    if (t.matches('.menu > summary') || t.closest('.menu > summary')) return;
    if (t.closest('[data-issue-state], [data-merge], [data-auto-merge]')) return;
    if (t.dataset.act) {
      e.stopPropagation();
      if (t.dataset.act === 'start') return post({ type: 'start', id: t.dataset.id });
      if (t.dataset.act === 'back') return post({ type: 'back' });
      if (t.dataset.act === 'detail') return post({ type: 'detail', id: t.dataset.id });
      if (t.dataset.act === 'assign') return post({ type: 'assign', id: t.dataset.id });
      if (t.dataset.act === 'retry') return post({ type: 'retry', id: t.dataset.id });
      if (t.dataset.act === 'clearError') return post({ type: 'clearError', id: t.dataset.id });
      if (t.dataset.act === 'copy') return post({ type: 'copy', text: t.dataset.text, label: t.dataset.label });
      if (t.dataset.act === 'open') return post({ type: 'open', url: t.dataset.url });
      if (t.dataset.act === 'toggleSettings') { const d = $('#settings'); d.open = !d.open; post({ type: 'settingsExpanded', expanded: d.open }); return; }
      return post({ type: t.dataset.act, source: t.dataset.source });
    }
    if (t.dataset.preset) return post({ type: 'preset', id: t.dataset.preset });
    if (t.dataset.page) return post({ type: 'page', page: Number(t.dataset.page) });
    if (t.classList.contains('source-btn')) return post({ type: 'source', source: t.dataset.source });
    if (t.dataset.url !== undefined && !t.classList.contains('row')) { e.stopPropagation(); return post({ type: 'open', url: t.dataset.url }); }
    if ((t.classList.contains('row') || t.classList.contains('board-card')) && t.dataset.id) return post({ type: 'detail', id: t.dataset.id });
  });
  document.addEventListener('contextmenu', (e) => {
    const chip = e.target.closest('[data-preset]');
    if (!chip) return;
    e.preventDefault();
    post({ type: 'setDefault', id: chip.dataset.preset });
  });
  document.addEventListener('keydown', (e) => {
    const row = e.target.closest('.row[data-id], .board-card[data-id]');
    if (row && e.target === row && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); post({ type: 'detail', id: row.dataset.id }); }
    if (e.key === 'Escape') document.querySelectorAll('details.menu[open]').forEach((d) => { d.open = false; });
  });
  document.addEventListener('click', (e) => {
    document.querySelectorAll('details.menu[open]').forEach((d) => { if (!d.contains(e.target)) d.open = false; });
  }, true);
  const query = $('#query');
  if (query) {
    query.addEventListener('keydown', (e) => { if (e.key === 'Enter') post({ type: 'query', query: query.value }); });
    query.addEventListener('input', () => $('#clear').classList.toggle('hidden', !query.value));
    $('#clear').addEventListener('click', () => { query.value = ''; post({ type: 'query', query: '' }); });
  }
  document.querySelectorAll('[data-filter]').forEach((sel) => sel.addEventListener('change', () => post({ type: 'filter', key: sel.dataset.filter, value: sel.value })));
  document.querySelectorAll('input[data-repo]').forEach((cb) => cb.addEventListener('change', (e) => {
    e.stopPropagation();
    const boxes = [...document.querySelectorAll('input[data-repo]')].filter((b) => b.dataset.repo !== '*');
    if (cb.dataset.repo === '*') boxes.forEach((b) => { b.checked = cb.checked; });
    post({ type: 'repos', repos: boxes.filter((b) => b.checked).map((b) => b.dataset.repo) });
  }));
  document.querySelectorAll('input[data-visible]').forEach((cb) => cb.addEventListener('change', () => post({ type: 'toggleSourceVisible', source: cb.dataset.visible, visible: cb.checked })));
  document.querySelectorAll('input[data-default]').forEach((r) => r.addEventListener('change', () => post({ type: 'defaultSource', source: r.dataset.default })));
  // Lazy hydration: un-hydrated PR rows ask for checks/merge/review data when scrolled near (160px) or hovered.
  const pendingHydrate = new Set();
  let hydrateTimer = null;
  const requestHydrate = (row) => {
    if (!row || !row.dataset.hydrate) return;
    pendingHydrate.add(row.dataset.id);
    row.removeAttribute('data-hydrate');
    clearTimeout(hydrateTimer);
    hydrateTimer = setTimeout(() => { post({ type: 'hydrate', ids: [...pendingHydrate] }); pendingHydrate.clear(); }, 50);
  };
  if ('IntersectionObserver' in window) {
    const io = new IntersectionObserver((entries) => entries.forEach((en) => { if (en.isIntersecting) { requestHydrate(en.target); io.unobserve(en.target); } }), { rootMargin: '160px 0px' });
    document.querySelectorAll('.row[data-hydrate]').forEach((r) => io.observe(r));
  } else {
    document.querySelectorAll('.row[data-hydrate]').forEach(requestHydrate);
  }
  document.addEventListener('mouseover', (e) => { const r = e.target.closest('.row[data-hydrate]'); if (r) requestHydrate(r); });
  window.addEventListener('message', (e) => {
    const msg = e.data || {};
    if (msg.type !== 'rows') return;
    for (const r of msg.rows || []) {
      const row = document.querySelector('.row[data-id="' + CSS.escape(r.id) + '"]');
      if (!row) continue;
      for (const [name, html] of Object.entries(r.cells || {})) { const cell = row.querySelector('.cell[data-cell="' + name + '"]'); if (cell) cell.innerHTML = html; }
      if (r.hydrated) row.removeAttribute('data-hydrate');
    }
  });
  document.addEventListener('click', (e) => {
    const t = e.target.closest('[data-issue-state], [data-merge], [data-auto-merge]');
    if (!t) return;
    e.stopPropagation();
    document.querySelectorAll('details.menu[open]').forEach((d) => { d.open = false; });
    if (t.dataset.issueState) return post({ type: 'issueState', id: t.dataset.id, state: t.dataset.issueState, reason: t.dataset.reason || null });
    if (t.dataset.merge) return post({ type: 'merge', id: t.dataset.id, method: t.dataset.merge });
    if (t.dataset.autoMerge) return post({ type: 'autoMerge', id: t.dataset.id, enable: t.dataset.autoMerge === 'enable' });
  });
  document.querySelectorAll('[data-view-mode]').forEach((b) => b.addEventListener('click', () => post({ type: 'viewMode', mode: b.dataset.viewMode })));
  const board = $('.board');
  if (board) {
    const st = vscode.getState() || {};
    const collapsed = st.collapsed || {};
    board.querySelectorAll('.board-col').forEach((col) => { if (collapsed[col.dataset.col]) col.classList.add('collapsed'); });
    board.querySelectorAll('[data-collapse]').forEach((b) => b.addEventListener('click', (e) => {
      e.stopPropagation();
      const col = b.closest('.board-col');
      col.classList.toggle('collapsed');
      const s2 = vscode.getState() || {};
      vscode.setState({ ...s2, collapsed: { ...(s2.collapsed || {}), [col.dataset.col]: col.classList.contains('collapsed') } });
    }));
    board.addEventListener('dragstart', (e) => {
      const card = e.target.closest('.board-card[draggable="true"]');
      if (!card) return e.preventDefault();
      e.dataTransfer.setData('text/plain', card.dataset.id);
      e.dataTransfer.effectAllowed = 'move';
      card.classList.add('dragging');
    });
    board.addEventListener('dragend', (e) => { const c = e.target.closest('.board-card'); if (c) c.classList.remove('dragging'); board.querySelectorAll('.drag-over').forEach((x) => x.classList.remove('drag-over')); });
    board.addEventListener('dragover', (e) => { const col = e.target.closest('.board-col'); if (!col) return; e.preventDefault(); e.dataTransfer.dropEffect = 'move'; board.querySelectorAll('.drag-over').forEach((x) => { if (x !== col) x.classList.remove('drag-over'); }); col.classList.add('drag-over'); });
    board.addEventListener('dragleave', (e) => { const col = e.target.closest('.board-col'); if (col && !col.contains(e.relatedTarget)) col.classList.remove('drag-over'); });
    board.addEventListener('drop', (e) => {
      const col = e.target.closest('.board-col');
      if (!col) return;
      e.preventDefault();
      col.classList.remove('drag-over');
      const id = e.dataTransfer.getData('text/plain');
      const from = board.querySelector('.board-card[data-id="' + CSS.escape(id) + '"]');
      const fromCol = from && from.closest('.board-col');
      if (!id || !fromCol || fromCol === col) return;
      post({ type: 'move', id, from: fromCol.dataset.col, to: col.dataset.col });
    });
    board.querySelectorAll('[data-move-to]').forEach((b) => b.addEventListener('click', (e) => { e.stopPropagation(); document.querySelectorAll('details.menu[open]').forEach((d) => { d.open = false; }); const card = b.closest('.board-card'); post({ type: 'move', id: b.dataset.id, from: card.closest('.board-col').dataset.col, to: b.dataset.moveTo }); }));
    board.addEventListener('scroll', () => vscode.setState({ ...(vscode.getState() || {}), boardScroll: board.scrollLeft }));
    if ((vscode.getState() || {}).boardScroll) board.scrollLeft = (vscode.getState() || {}).boardScroll;
  }
  const list = $('.list');
  const state = vscode.getState() || {};
  if (list && state.scroll) { list.scrollTop = state.scroll; }
  if (list) list.addEventListener('scroll', () => vscode.setState({ ...(vscode.getState() || {}), scroll: list.scrollTop }));
  const detail = $('.detail[data-detail-id]');
  if (detail) {
    const id = detail.dataset.detailId;
    const body = $('#comment-body');
    const send = $('#comment-send');
    if (body) {
      body.value = (state.drafts || {})[id] || '';
      send.disabled = !body.value.trim();
      body.addEventListener('input', () => { send.disabled = !body.value.trim(); vscode.setState({ ...(vscode.getState() || {}), drafts: { ...((vscode.getState() || {}).drafts || {}), [id]: body.value } }); });
      body.addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); $('#composer').requestSubmit(); } });
      $('#composer').addEventListener('submit', (e) => {
        e.preventDefault();
        const text = body.value.trim();
        if (!text) return;
        vscode.setState({ ...(vscode.getState() || {}), drafts: { ...((vscode.getState() || {}).drafts || {}), [id]: '' } });
        send.disabled = true;
        post({ type: 'comment', id, body: text });
      });
    }
    detail.querySelectorAll('[data-tab]').forEach((b) => b.addEventListener('click', (e) => { e.stopPropagation(); if (b.dataset.url) return post({ type: 'open', url: b.dataset.url }); post({ type: 'tab', id, tab: b.dataset.tab }); }));
    detail.querySelectorAll('[data-priority]').forEach((b) => b.addEventListener('click', (e) => { e.stopPropagation(); post({ type: 'priority', id, priorityId: b.dataset.priority }); }));
    const form = $('#transition-form');
    let chosen = null;
    detail.querySelectorAll('[data-transition]').forEach((b) => b.addEventListener('click', (e) => {
      e.stopPropagation();
      document.querySelectorAll('details.menu[open]').forEach((d) => { d.open = false; });
      if (b.dataset.req === 'none') return post({ type: 'transition', id, transitionId: b.dataset.transition });
      if (b.dataset.req !== 'form') return;
      chosen = JSON.parse(b.dataset.form);
      $('#transition-title').textContent = chosen.name;
      const res = $('#resolution');
      res.innerHTML = '';
      if (chosen.resolution) {
        chosen.resolution.options.forEach((o) => { const opt = document.createElement('option'); opt.value = o.id; opt.textContent = o.label; res.appendChild(opt); });
      }
      $('#resolution-field').classList.toggle('hidden', !chosen.resolution);
      $('#transition-comment-label').textContent = chosen.commentRequired ? 'Comment (required)' : 'Comment';
      $('#transition-comment').required = !!chosen.commentRequired;
      form.classList.remove('hidden');
      ($('#resolution-field').classList.contains('hidden') ? $('#transition-comment') : res).focus();
    }));
    if (form) {
      form.addEventListener('submit', (e) => {
        e.preventDefault();
        if (!chosen) return;
        post({ type: 'transition', id, transitionId: chosen.id, resolutionId: chosen.resolution ? $('#resolution').value : null, comment: $('#transition-comment').value.trim() });
        form.classList.add('hidden');
      });
      $('#transition-cancel').addEventListener('click', () => { form.classList.add('hidden'); chosen = null; });
    }
  }
`;

/** Full webview HTML for the Tasks page. `model` is built by the extension's TasksViewProvider. */
function renderTasksHtml(model, opts) {
  const { nonce = '', cspSource = '' } = opts || {};
  const kind = gridKind(model);
  const grid = GRIDS[kind];
  const titleLeft = kind === 'jira' ? '168px' : '84px';
  let body;
  if (model.loading) body = skeletonRows(kind);
  else if (!model.items.length && !(model.errors || []).length) body = emptyState(model);
  else body = model.items.map((i) => row(i, model, kind)).join('') + errorRows(model);
  const bodyIsEmpty = !model.loading && !model.items.length && !(model.errors || []).length;
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src https: data:; style-src 'unsafe-inline' ${cspSource}; script-src 'nonce-${nonce}';">
<style>${CSS}</style>
</head>
<body style="--grid:${grid};--title-left:${titleLeft}">
  <div class="chrome">
    ${sourceBar(model)}
    ${listBanner(model)}
    ${modeControls(model)}
    ${filters(model)}
  </div>
  ${model.view === 'detail' ? detailView(model, model.now) : model.viewMode === 'board' ? `${boardView(model)}
  ${pagination(model)}` : `<div class="list"><div class="grid${bodyIsEmpty ? ' grid-empty' : ''}">${bodyIsEmpty ? '' : headerRow(kind)}${body}</div></div>
  ${pagination(model)}`}
  ${settingsSection(model)}
  <script nonce="${nonce}">${SCRIPT}</script>
</body>
</html>`;
}

/** Left-nav launcher: source rows with status dots and open buttons; the page itself lives in a window or the Panel. */
function renderLauncherHtml(model, opts) {
  const { nonce = '', cspSource = '' } = opts || {};
  const s = model.settings || {};
  const net = (s.tailscale && s.tailscale.status) || null;
  const statusDot = (st) => (!st ? 'muted' : st.ok ? 'emerald' : 'danger');
  const sources = (model.sources || []).map((src) => {
    const c = s[src.id] || {};
    return `<button class="src${src.id === model.source ? ' active' : ''}" data-open-source="${esc(src.id)}" title="Open Tasks on ${esc(SOURCE_LABELS[src.id] || src.id)} in a new window"><span class="src-icon">${ICONS[src.id] || ''}</span><span class="src-name">${esc(SOURCE_LABELS[src.id] || src.id)}</span><span class="dot dot-${statusDot(c.status)}" title="${esc(c.status ? (c.status.ok ? `Connected${c.status.login ? ` as ${c.status.login}` : ''}` : c.status.message || 'Disconnected') : 'Checking…')}"></span>${c.status && !c.status.ok ? `<span class="mini" role="button" tabindex="0" data-act="signIn" data-source="${esc(src.id)}">${src.id === 'github' ? 'Sign in' : 'Token'}</span>` : ''}</button>`;
  });
  const where = model.openWhere === 'window' ? 'a separate window' : model.openWhere === 'editor' ? 'an editor tab' : model.openWhere === 'panel' ? 'the Panel' : null;
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src https: data:; style-src 'unsafe-inline' ${cspSource}; script-src 'nonce-${nonce}';">
<style>
  :root { --track: color-mix(in srgb, var(--vscode-foreground) 14%, transparent); --border: color-mix(in srgb, var(--vscode-widget-border, var(--vscode-foreground)) 60%, transparent); --hover: var(--vscode-list-hoverBackground); --emerald: #34d399; --danger: var(--vscode-editorError-foreground, #f87171); --primary: var(--vscode-button-background); }
  * { box-sizing: border-box; }
  body { font-family: var(--vscode-font-family); font-size: 13px; color: var(--vscode-foreground); background: var(--vscode-sideBar-background); margin: 0; padding: 8px 10px; display: flex; flex-direction: column; gap: 10px; }
  svg { width: 14px; height: 14px; flex: none; }
  .muted { color: var(--vscode-descriptionForeground); font-size: 12px; }
  .section { font-size: 11px; font-weight: 600; letter-spacing: .08em; text-transform: uppercase; color: var(--vscode-descriptionForeground); }
  .src { display: flex; align-items: center; gap: 8px; width: 100%; border: 1px solid transparent; border-radius: 6px; padding: 6px 8px; background: none; color: inherit; font: inherit; cursor: pointer; text-align: left; }
  .src:hover { background: var(--hover); }
  .src.active { border-color: color-mix(in srgb, var(--vscode-foreground) 30%, transparent); background: color-mix(in srgb, var(--track) 50%, transparent); }
  .src-icon { width: 24px; height: 24px; display: inline-flex; align-items: center; justify-content: center; border-radius: 5px; background: color-mix(in srgb, var(--track) 60%, transparent); }
  .src-icon svg { width: 14px; height: 14px; }
  .src-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .dot { width: 8px; height: 8px; border-radius: 50%; flex: none; }
  .dot-emerald { background: var(--emerald); } .dot-danger { background: var(--danger); } .dot-muted { background: var(--track); }
  .mini { font: inherit; font-size: 11px; border: 1px solid var(--border); border-radius: 999px; padding: 0 7px; background: none; color: inherit; cursor: pointer; }
  .btn { display: inline-flex; align-items: center; justify-content: center; gap: 6px; width: 100%; border-radius: 6px; padding: 5px 10px; cursor: pointer; border: 1px solid var(--border); background: none; color: var(--vscode-foreground); font: inherit; }
  .btn.primary { background: var(--primary); color: var(--vscode-button-foreground); border-color: transparent; }
  .btn:hover { filter: brightness(1.1); }
  .src:disabled { opacity: .6; cursor: default; }
</style>
</head>
<body>
  <div class="section">Sources</div>
  <div>${sources.join('')}</div>
  <button class="btn primary" data-act="openWindow">${ICONS.externalLink}New Window</button>
  <button class="btn" data-act="openPanel">${ICONS.layout}Panel</button>
  ${where ? `<div class="muted">Tasks is open in ${esc(where)}.</div>` : ''}
  <div class="section">Network</div>
  <button class="src" data-act="exitNode"${net && net.ok ? '' : ' disabled'} title="Change exit node"><span class="src-icon">${ICONS.globe}</span><span class="src-name">${esc(!net ? 'Tailscale: checking…' : !net.ok ? `Tailscale: ${net.message || 'disconnected'}` : net.exitNode ? `Exit node: ${net.exitNode.hostName}` : 'Direct (no exit node)')}</span><span class="dot dot-${!net ? 'muted' : !net.ok ? 'danger' : net.exitNode ? 'emerald' : 'muted'}"></span></button>
  <script nonce="${nonce}">
    const vscode = acquireVsCodeApi();
    document.addEventListener('click', (e) => {
      const t = e.target.closest('[data-act], [data-open-source]');
      if (!t) return;
      e.stopPropagation();
      if (t.dataset.act === 'signIn') return vscode.postMessage({ type: 'signIn', source: t.dataset.source });
      if (t.dataset.act) return vscode.postMessage({ type: t.dataset.act });
      vscode.postMessage({ type: 'openWindow', source: t.dataset.openSource });
    });
  </script>
</body>
</html>`;
}

/** What a sidebar/Panel view shows while the editor tab or floating window owns the Tasks page. */
function renderElsewhereHtml(opts) {
  const { floating = false, viewType = '', nonce = '', cspSource = '' } = opts || {};
  const where = floating ? 'a separate window' : 'an editor tab';
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline' ${cspSource}; script-src 'nonce-${nonce}';">
<style>
  body { font-family: var(--vscode-font-family); font-size: 13px; color: var(--vscode-descriptionForeground); background: var(--vscode-sideBar-background); margin: 0; height: 100vh; display: flex; align-items: center; justify-content: center; }
  .box { display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 16px; text-align: center; }
  .row { display: flex; gap: 8px; flex-wrap: wrap; justify-content: center; }
  button { font: inherit; display: inline-flex; align-items: center; gap: 6px; border-radius: 6px; padding: 4px 10px; cursor: pointer; border: 1px solid var(--vscode-widget-border, var(--vscode-descriptionForeground)); background: none; color: var(--vscode-foreground); }
  button.primary { background: var(--vscode-button-background); color: var(--vscode-button-foreground); border-color: transparent; }
  svg { width: 14px; height: 14px; }
</style>
</head>
<body>
  <div class="box" role="status">
    <div>Tasks is open in ${where}.</div>
    <div class="row"><button class="primary" id="back">${ICONS.chevronLeft}Bring back here</button><button id="show">${ICONS.externalLink}Show it</button></div>
  </div>
  <script nonce="${nonce}">
    const vscode = acquireVsCodeApi();
    document.getElementById('back').addEventListener('click', () => vscode.postMessage({ type: 'bringBack', viewType: ${JSON.stringify(viewType)} }));
    document.getElementById('show').addEventListener('click', () => vscode.postMessage({ type: 'revealElsewhere' }));
  </script>
</body>
</html>`;
}

module.exports = { renderTasksHtml, renderElsewhereHtml, renderLauncherHtml, detailView, boardView, rowCells, usesPrGrid, gridKind, ISSUE_GRID, PR_GRID, JIRA_GRID, SKELETON_ROWS };
