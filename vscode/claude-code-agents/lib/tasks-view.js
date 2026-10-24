'use strict';
// Webview HTML for the Tasks page (Orca's Task page, GitHub + Bitbucket DC) — no vscode dependency, so node:test
// can render it. Same nonce/CSP shape as usage-view.js; the page re-renders whole on every model change.
const { ICONS } = require('../media/icons');
const { getPageNumbers, formatRelativeTime } = require('./tasks-state');

const SKELETON_ROWS = 12;
const ISSUE_GRID = '72px minmax(320px,1fr) 84px 100px 92px 122px';
const PR_GRID = '72px minmax(360px,2fr) 132px 128px 132px 92px 158px';
const SOURCE_LABELS = { github: 'GitHub', bitbucket: 'Bitbucket' };

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function attr(obj) {
  return JSON.stringify(obj).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;');
}

/** The PR grid when the query scope is pr, Bitbucket is active, or every loaded row is a PR; else the issue grid. */
function usesPrGrid(model) {
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

function idPill(item) {
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
  if (item.jiraKey) meta.push(`<a class="jira-chip" data-url="${esc(item.jiraUrl || '')}" title="Open ${esc(item.jiraKey)} in Jira">${esc(item.jiraKey)}</a>`);
  for (const label of (item.labels || []).slice(0, 3)) meta.push(`<span class="label-chip">${esc(label)}</span>`);
  return `<div class="title-row"><h3 class="title">${esc(item.title)}</h3>${badge}${repoBadge}</div><div class="meta">${meta.join('')}</div>`;
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
  if (!list.length) return `<span class="muted cell-text">${item.type === 'pr' ? 'No reviewers' : ''}</span>`;
  const primary = list.find((r) => r.status === 'APPROVED') || list.find((r) => r.status === 'CHANGES_REQUESTED') || list[0];
  const roster = list.map((r) => `${r.name} — ${r.status.toLowerCase().replace('_', ' ')}`).join('\n');
  const more = list.length > 1 ? `<span class="more">+${list.length - 1}</span>` : '';
  return `<span class="reviewers" title="${esc(roster)}"><span class="avatar ring-${reviewerTone(primary.status)}">${esc(initials(primary.name))}</span><span class="reviewer-name">${esc(primary.name)}</span>${more}</span>`;
}

function checksCell(item) {
  const c = item.checks;
  if (!c || c.state === 'none') return `<span class="cell-text muted">${ICONS.minus}${esc(c ? c.label : 'No checks')}</span>`;
  const icon = c.state === 'success' ? ICONS.checkCircle2 : c.state === 'failed' ? ICONS.alertCircle : ICONS.clock3;
  const tone = c.state === 'success' ? 'emerald' : c.state === 'failed' ? 'danger' : 'amber';
  const url = c.url || item.url;
  return `<button class="cell-text tone-${tone} link-btn" data-url="${esc(url)}" title="Open checks">${icon}${esc(c.label)}</button>`;
}

function mergeCell(item) {
  const m = item.merge;
  if (!m) return '';
  const tone = m.state === 'ready' ? 'emerald' : m.state === 'merged' ? 'primary' : m.state === 'conflict' ? 'danger' : m.state === 'blocked' ? 'amber' : 'muted';
  const title = item.source === 'bitbucket' ? 'Open the pull request in Bitbucket' : `Open the merge box in browser (${m.label})`;
  return `<button class="pill pill-${tone} pill-btn link-btn" data-url="${esc(item.url)}" title="${esc(title)}">${esc(m.label)}${ICONS.externalLink}</button>`;
}

function statusCell(item) {
  const closed = item.state === 'closed';
  const reason = item.stateReason ? ` (${String(item.stateReason).replace('_', ' ')})` : '';
  return pill(closed ? 'primary' : 'emerald', closed ? 'Closed' : 'Open', `${closed ? 'Closed' : 'Open'}${reason}`);
}

function assigneesCell(item) {
  const list = item.assignees || [];
  if (!list.length) return '<span class="muted cell-text">—</span>';
  const more = list.length > 1 ? `<span class="more">+${list.length - 1}</span>` : '';
  return `<span class="reviewers" title="${esc(list.join('\n'))}"><span class="avatar ring-muted">${esc(initials(list[0]))}</span>${more}</span>`;
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

function row(item, model, prGrid) {
  const cells = [`<div class="cell c-id">${idPill(item)}</div>`, `<div class="cell c-title">${titleCell(item, model)}</div>`];
  if (prGrid) {
    cells.push(`<div class="cell">${reviewersCell(item)}</div>`, `<div class="cell">${checksCell(item)}</div>`, `<div class="cell">${mergeCell(item)}</div>`);
  } else {
    cells.push(`<div class="cell">${assigneesCell(item)}</div>`, `<div class="cell">${statusCell(item)}</div>`);
  }
  cells.push(`<div class="cell">${updatedCell(item, model.now)}</div>`, `<div class="cell c-actions">${actionsCell(item)}</div>`);
  return `<div class="row" role="button" tabindex="0" data-id="${esc(item.id)}" data-url="${esc(item.url)}">${cells.join('')}</div>`;
}

function headerRow(prGrid) {
  const cols = prGrid ? ['ID', 'Title', 'Reviewers', 'Checks', 'Merge', 'Updated', 'Actions'] : ['ID', 'Title', 'Assignees', 'Status', 'Updated', 'Actions'];
  return `<div class="row head">${cols.map((c, i) => `<div class="cell${i === 0 ? ' c-id' : i === 1 ? ' c-title' : ''}">${c}</div>`).join('')}</div>`;
}

function skeletonRows(prGrid) {
  const n = prGrid ? 7 : 6;
  const one = `<div class="row skeleton" aria-hidden="true">${Array.from({ length: n }, (_, i) => `<div class="cell${i === 0 ? ' c-id' : i === 1 ? ' c-title' : ''}"><span class="shimmer${i === 1 ? ' wide' : ''}"></span></div>`).join('')}</div>`;
  return one.repeat(SKELETON_ROWS);
}

function errorRows(model) {
  return (model.errors || []).map((e) => `<div class="row error-row"><div class="cell c-id">${ICONS.alertCircle}</div><div class="cell c-title"><span class="title">${esc(e.repo)} · ${esc(e.kind)}</span><div class="meta">${esc(e.message)}</div></div><div class="cell c-actions"><button class="btn btn-outline" data-act="refresh">${ICONS.refreshCw}Retry</button></div></div>`).join('');
}

function emptyState(model) {
  const title = model.source === 'bitbucket' ? 'No pull requests' : 'Nothing matches';
  const desc = model.source === 'bitbucket' ? 'No Bitbucket pull requests match this filter.' : model.repoCount ? 'Try another preset or clear the query.' : 'Add a GitHub repo in Task sources or open a workspace with a GitHub remote.';
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
  const stateOpts = [['open', 'Open'], ['closed', model.source === 'bitbucket' ? 'Declined' : 'Closed'], ['merged', 'Merged'], ['all', 'All']];
  const state = `<select class="select" data-filter="state" aria-label="State">${stateOpts.map(([v, l]) => `<option value="${v}"${(model.state || 'open') === v ? ' selected' : ''}>${l}</option>`).join('')}</select>`;
  const newIssue = model.source === 'github' && (model.repos || []).some((r) => r.selected) ? `<button class="btn btn-outline" data-act="newIssue" title="New issue">${ICONS.plus}New issue</button>` : '';
  return `<div class="filters"><div class="chips">${chips}</div><div class="filter-row">${scope}${state}<label class="search"><span class="search-icon">${ICONS.search}</span><input id="query" type="text" value="${esc(model.query || '')}" placeholder="Search ${model.source === 'bitbucket' ? 'pull requests' : 'issues and pull requests'}…" spellcheck="false" autocomplete="off"><button class="clear${model.query ? '' : ' hidden'}" id="clear" aria-label="Clear">${ICONS.x}</button></label>${newIssue}<button class="icon-btn${model.loading || model.loadingPage != null ? ' spin' : ''}" data-act="refresh" title="Refresh">${ICONS.refreshCw}</button></div></div>`;
}

function statusPill(status) {
  if (!status) return pill('muted', 'Checking…');
  if (status.ok) return pill('emerald', `Connected as ${status.login || status.user || 'user'}`);
  return pill('danger', status.message || status.reason || 'Not connected', status.message);
}

/** Task sources cards: status pill, Show-in-Tasks toggle (locked on the last visible one), default radio, repo list. */
function settingsSection(model) {
  const s = model.settings || {};
  const visibleCount = model.sources.length;
  const card = (id, cfg) => {
    const c = cfg || {};
    const lockVisible = c.visible && visibleCount <= 1;
    const repos = (c.repos || []).length ? c.repos.map((r) => `<span class="label-chip">${esc(r)}</span>`).join('') : '<span class="muted">workspace remotes only</span>';
    const signIn = c.status && !c.status.ok ? `<button class="btn btn-outline" data-act="signIn" data-source="${id}">${id === 'github' ? 'gh auth login' : 'Set token'}</button>` : '';
    return `<div class="card"><div class="card-head">${ICONS[id]}<span class="card-title">${esc(SOURCE_LABELS[id])}</span>${statusPill(c.status)}${signIn}</div>
      <div class="card-row"><label class="menu-check"><input type="checkbox" data-visible="${id}"${c.visible ? ' checked' : ''}${lockVisible ? ' disabled title="The last visible source stays on"' : ''}>Show in Tasks</label><label class="menu-check"><input type="radio" name="defaultSource" data-default="${id}"${s.defaultSource === id ? ' checked' : ''}>Default</label></div>
      <div class="card-row repos">${repos}</div>${c.baseUrl ? `<div class="card-row muted truncate">${esc(c.baseUrl)}</div>` : ''}</div>`;
  };
  return `<details class="settings" id="settings"${s.expanded ? ' open' : ''}><summary class="section-header">TASK SOURCES</summary><div class="cards">${card('github', s.github)}${card('bitbucket', s.bitbucket)}</div><div class="card-row"><button class="btn btn-outline" data-act="openSettings">${ICONS.settings}Edit repos and defaults in Settings</button><button class="btn btn-outline" data-act="doctor">${ICONS.refreshCw}Re-check connections</button></div></details>`;
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
  .menu-items { position: absolute; top: calc(100% + 4px); right: 0; z-index: 50; min-width: 180px; background: var(--vscode-menu-background, var(--vscode-dropdown-background)); color: var(--vscode-menu-foreground, var(--vscode-foreground)); border: 1px solid var(--border); border-radius: 6px; padding: 4px; box-shadow: 0 4px 16px rgba(0,0,0,.3); display: flex; flex-direction: column; }
  .repo-menu .menu-items { left: 0; right: auto; max-height: 240px; overflow: auto; }
  .menu-items button, .menu-check { display: flex; align-items: center; gap: 6px; padding: 5px 8px; border: none; background: none; text-align: left; border-radius: 4px; cursor: pointer; font-size: 12px; white-space: nowrap; }
  .menu-items button:hover, .menu-check:hover { background: var(--hover); }
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
  .c-title { left: 84px; flex-direction: column; align-items: stretch; justify-content: center; }
  .c-id::before, .c-title::before { content: ''; position: absolute; top: 0; bottom: 0; width: 12px; background: inherit; }
  .c-id::before { left: -12px; }
  .c-title::before { left: -12px; }
  .c-title::after { content: ''; position: absolute; top: 0; bottom: 0; right: -12px; width: 12px; background: inherit; }
  .id-pill { display: inline-flex; align-items: center; gap: 4px; border-radius: 6px; border: 1px solid color-mix(in srgb, var(--border) 40%, transparent); padding: 2px 6px; color: var(--vscode-descriptionForeground); }
  .id-pill svg { width: 12px; height: 12px; }
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
  @media (max-width: 480px) { .chrome { padding: 8px; } .row { padding: 10px 8px; } .c-title { left: 80px; } .page-btn { min-width: 0; } }
`;

const SCRIPT = `
  const vscode = acquireVsCodeApi();
  const post = (msg) => vscode.postMessage(msg);
  const $ = (sel, root) => (root || document).querySelector(sel);
  document.addEventListener('click', (e) => {
    const t = e.target.closest('[data-act], [data-url], [data-source], [data-preset], [data-page], .row');
    if (!t) return;
    if (t.matches('.menu > summary') || t.closest('.menu > summary')) return;
    if (t.dataset.act) {
      e.stopPropagation();
      if (t.dataset.act === 'start') return post({ type: 'start', id: t.dataset.id });
      if (t.dataset.act === 'open') return post({ type: 'open', url: t.dataset.url });
      if (t.dataset.act === 'toggleSettings') { const d = $('#settings'); d.open = !d.open; post({ type: 'settingsExpanded', expanded: d.open }); return; }
      return post({ type: t.dataset.act, source: t.dataset.source });
    }
    if (t.dataset.preset) return post({ type: 'preset', id: t.dataset.preset });
    if (t.dataset.page) return post({ type: 'page', page: Number(t.dataset.page) });
    if (t.classList.contains('source-btn')) return post({ type: 'source', source: t.dataset.source });
    if (t.dataset.url !== undefined && !t.classList.contains('row')) { e.stopPropagation(); return post({ type: 'open', url: t.dataset.url }); }
    if (t.classList.contains('row') && t.dataset.id) return post({ type: 'detail', id: t.dataset.id });
  });
  document.addEventListener('contextmenu', (e) => {
    const chip = e.target.closest('[data-preset]');
    if (!chip) return;
    e.preventDefault();
    post({ type: 'setDefault', id: chip.dataset.preset });
  });
  document.addEventListener('keydown', (e) => {
    const row = e.target.closest('.row[data-id]');
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
  const list = $('.list');
  if (list && window.__scroll) list.scrollTop = window.__scroll;
`;

/** Full webview HTML for the Tasks page. `model` is built by the extension's TasksViewProvider. */
function renderTasksHtml(model, opts) {
  const { nonce = '', cspSource = '' } = opts || {};
  const prGrid = usesPrGrid(model);
  const grid = prGrid ? PR_GRID : ISSUE_GRID;
  let body;
  if (model.loading) body = skeletonRows(prGrid);
  else if (!model.items.length && !(model.errors || []).length) body = emptyState(model);
  else body = model.items.map((i) => row(i, model, prGrid)).join('') + errorRows(model);
  const bodyIsEmpty = !model.loading && !model.items.length && !(model.errors || []).length;
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline' ${cspSource}; script-src 'nonce-${nonce}';">
<style>${CSS}</style>
</head>
<body style="--grid:${grid}">
  <div class="chrome">
    ${sourceBar(model)}
    ${listBanner(model)}
    ${modeControls(model)}
    ${filters(model)}
  </div>
  <div class="list"><div class="grid${bodyIsEmpty ? ' grid-empty' : ''}">${bodyIsEmpty ? '' : headerRow(prGrid)}${body}</div></div>
  ${pagination(model)}
  ${settingsSection(model)}
  <script nonce="${nonce}">${SCRIPT}</script>
</body>
</html>`;
}

module.exports = { renderTasksHtml, usesPrGrid, ISSUE_GRID, PR_GRID, SKELETON_ROWS };
