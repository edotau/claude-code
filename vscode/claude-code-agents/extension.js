'use strict';
const vscode = require('vscode');
const { execFile } = require('node:child_process');
const {
  AGENTS,
  dispatchStartArgv,
  usageArgv,
  profileArgv,
  vscodeServeArgv,
  attachCommandLine,
} = require('./lib/argv');
const { buildUsageModel, statusBarText, statusBarTooltip } = require('./lib/usage-model');
const { renderUsageHtml } = require('./lib/usage-view');
const bitbucketClient = require('./lib/bitbucket-client');
const taskQuery = require('./lib/task-query');
const tasksGithub = require('./lib/tasks-github');
const tasksBitbucket = require('./lib/tasks-bitbucket');
const jiraClient = require('./lib/jira-client');
const tasksJira = require('./lib/tasks-jira');
const { MutationQueue } = require('./lib/tasks-mutations');
const tasksBoard = require('./lib/tasks-board');
const tailscale = require('./lib/tailscale');
const tasksState = require('./lib/tasks-state');
const { renderTasksHtml, renderElsewhereHtml, renderLauncherHtml, rowCells, gridKind } = require('./lib/tasks-view');

// Source order for the source bar and the settings writes; every handler reads this one list.
const TASK_SOURCES = ['jira', 'github', 'bitbucket'];
const GITHUB_PR_EXTENSION_ID = 'github.vscode-pull-request-github';

// dashboardTerminal reuses one "Agents Dashboard" terminal for every command the dashboard launches.
function dashboardTerminal() {
  const name = 'Agents Dashboard';
  const existing = vscode.window.terminals.find((t) => t.name === name && t.exitStatus === undefined);
  return existing || vscode.window.createTerminal({ name });
}

function config() {
  return vscode.workspace.getConfiguration('claudeCodeAgents');
}

function runBinary(binary, argv, cwd) {
  return new Promise((resolve, reject) => {
    execFile(binary, argv.slice(1), { cwd, maxBuffer: 10 * 1024 * 1024 }, (err, stdout, stderr) => {
      if (err) {
        if (err.code === 'ENOENT') return reject(new Error(`binary not found: ${binary}`));
        return reject(new Error(stderr && stderr.trim() ? stderr.trim() : err.message));
      }
      resolve(stdout);
    });
  });
}

/** 32-char alnum string for the webview CSP script nonce. */
function nonce() {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
  let text = '';
  for (let i = 0; i < 32; i++) text += alphabet.charAt(Math.floor(Math.random() * alphabet.length));
  return text;
}

/** Drives the status bar segment + the Usage WebviewView from `usage`/`profile --format json`. */
class UsageViewProvider {
  constructor(statusBarItem) {
    this.statusBarItem = statusBarItem;
    this.view = undefined;
    this.panel = undefined;
    this.usageJson = null;
    this.profileJson = null;
    this.detailed = true;
    this.lastErrorMessage = null;
  }

  /** Wires scripts + the shared message handler onto a webview (sidebar view or editor panel). */
  wireWebview(webview) {
    webview.options = { enableScripts: true };
    webview.onDidReceiveMessage((msg) => this.handleMessage(msg));
  }

  resolveWebviewView(webviewView) {
    this.view = webviewView;
    this.wireWebview(webviewView.webview);
    this.render();
  }

  /** Opens (or reveals) the single Usage editor-tab panel; same model and message handling as the sidebar view. */
  openInEditor() {
    if (this.panel) {
      this.panel.reveal(vscode.ViewColumn.Active);
      return;
    }
    const panel = vscode.window.createWebviewPanel('claudeCodeAgents.usageEditor', 'Agents Dashboard', vscode.ViewColumn.Active, {
      enableScripts: true,
      retainContextWhenHidden: true,
    });
    this.panel = panel;
    this.wireWebview(panel.webview);
    panel.onDidDispose(() => {
      if (this.panel === panel) {
        this.panel = undefined;
        this.panelFloating = false;
      }
    });
    this.render();
  }

  async handleMessage(msg) {
    if (!msg) return;
    if (msg.type === 'refresh') return this.load();
    if (msg.type === 'toggle') {
      this.detailed = !!msg.detailed;
      return this.render();
    }
    if (msg.type === 'show') {
      const show = msg.show === 'left' ? 'left' : 'used';
      // Persisted as the usageShow setting so the status bar and every window follow the same perspective.
      await config().update('usageShow', show, vscode.ConfigurationTarget.Global);
      return this.render();
    }
  }

  async load() {
    const cfg = config();
    const binary = cfg.get('binary') || 'claude-code';
    const days = cfg.get('usageDays', 30) || 30;
    try {
      const [usageOut, profileOut] = await Promise.all([
        runBinary(binary, usageArgv(binary, { days })),
        runBinary(binary, profileArgv(binary)),
      ]);
      this.usageJson = JSON.parse(usageOut);
      this.profileJson = JSON.parse(profileOut);
      this.lastErrorMessage = null;
    } catch (err) {
      this.usageJson = null;
      this.profileJson = null;
      this.statusBarItem.text = '$(sparkle) usage: error';
      this.statusBarItem.tooltip = err.message;
      if (this.lastErrorMessage !== err.message) {
        this.lastErrorMessage = err.message;
        vscode.window.showErrorMessage(`Claude Code Agents usage: ${err.message}`);
      }
      return;
    }
    this.render();
  }

  /** Re-renders from the cached JSON with a fresh `now` — ticks reset countdowns without refetching. */
  tick() {
    if (this.usageJson) this.render();
  }

  render() {
    if (!this.usageJson) return;
    const cfg = config();
    const show = cfg.get('usageShow', 'used') || 'used';
    const days = cfg.get('usageDays', 30) || 30;
    const showAgents = cfg.get('usageStatusBarAgents', true);
    const model = buildUsageModel(this.usageJson, this.profileJson, Date.now());
    this.statusBarItem.text = statusBarText(model, show, { agents: showAgents });
    const tooltip = new vscode.MarkdownString(statusBarTooltip(model, show));
    tooltip.isTrusted = false;
    this.statusBarItem.tooltip = tooltip;
    for (const surface of [this.view, this.panel]) {
      if (!surface) continue;
      surface.webview.html = renderUsageHtml(model, {
        detailed: this.detailed,
        show,
        days,
        nonce: nonce(),
        cspSource: surface.webview.cspSource,
      });
    }
  }
}

/** Prompts for host/agent/dir/task, starts the dispatch, and offers to copy the attach command. */
async function runDispatch() {
  const cfg = config();
  const binary = cfg.get('binary') || 'claude-code';
  const configuredHosts = cfg.get('hosts', []) || [];
  const hostPick = await vscode.window.showQuickPick([...configuredHosts, 'Enter custom host…'], {
    placeHolder: 'Dispatch host',
  });
  if (!hostPick) return;
  let host = hostPick;
  if (hostPick === 'Enter custom host…') {
    host = await vscode.window.showInputBox({ prompt: 'SSH host' });
    if (!host) return;
  }
  const agent = await vscode.window.showQuickPick(AGENTS, { placeHolder: 'Agent CLI' });
  if (!agent) return;
  const dir = await vscode.window.showInputBox({ prompt: 'Remote working directory (optional)' });
  const task = await vscode.window.showInputBox({ prompt: 'Task' });
  if (!task) return;
  try {
    const argv = dispatchStartArgv(binary, { host, agent, dir: dir || undefined, task });
    await runBinary(binary, argv);
    const line = attachCommandLine(host, `cc-${agent}-*`);
    const choice = await vscode.window.showInformationMessage(`Dispatched on ${host}. ${line}`, 'Copy Command');
    if (choice === 'Copy Command') {
      await vscode.env.clipboard.writeText(line);
    }
  } catch (err) {
    vscode.window.showErrorMessage(`Claude Code Agents dispatch failed: ${err.message}`);
  }
}

/** True when a `claude-code bitbucket` error message is a missing-credential error, not a real failure. */
function looksLikeMissingToken(message) {
  return /no token|token not (set|found)|missing.*token/i.test(message || '');
}

function workspaceDir() {
  const folders = vscode.workspace.workspaceFolders;
  return folders && folders.length ? folders[0].uri.fsPath : undefined;
}

/** Current checked-out branch name, or null outside a git repo / on error. */
function currentBranch(cwd) {
  return new Promise((resolve) => {
    if (!cwd) return resolve(null);
    execFile('git', ['rev-parse', '--abbrev-ref', 'HEAD'], { cwd }, (err, stdout) => {
      resolve(err ? null : stdout.trim());
    });
  });
}

/** `origin`'s remote URL, or null outside a git repo / with no origin. */
function originRemoteUrl(cwd) {
  return new Promise((resolve) => {
    if (!cwd) return resolve(null);
    execFile('git', ['remote', 'get-url', 'origin'], { cwd }, (err, stdout) => {
      resolve(err ? null : stdout.trim());
    });
  });
}

/** The host a remote URL (ssh/scp-like/https) points at, for display when it isn't a Data Center match. */
function remoteHostFor(remoteUrl) {
  const trimmed = (remoteUrl || '').trim();
  const scpLike = trimmed.match(/^(?:[^@/]+@)?([^:/]+):(?!\/\/)([^\s]+)$/);
  if (scpLike && !trimmed.includes('://')) return scpLike[1];
  try {
    return new URL(trimmed).hostname;
  } catch {
    return trimmed;
  }
}

/** `{owner, name, label, selected}` GitHub repos: the `tasksGitHubRepos` setting plus every workspace folder's origin. */
async function discoverGitHubRepos(selectedLabels) {
  const slugs = (config().get('tasksGitHubRepos', []) || []).map(tasksGithub.parseRepoSlug).filter(Boolean);
  for (const folder of vscode.workspace.workspaceFolders || []) {
    const repo = tasksGithub.repoFromRemote(await originRemoteUrl(folder.uri.fsPath));
    if (repo && !slugs.some((s) => s.owner === repo.owner && s.name === repo.name)) slugs.push(repo);
  }
  return slugs.map((r) => {
    const label = `${r.owner}/${r.name}`;
    return { owner: r.owner, name: r.name, label, selected: !selectedLabels || selectedLabels.includes(label) };
  });
}

/** Data Center repo refs: the `tasksBitbucketRepos` setting (`PROJECT/repo`) plus workspace folders on the DC host. */
async function discoverBitbucketRepos(host) {
  const refs = [];
  for (const entry of config().get('tasksBitbucketRepos', []) || []) {
    const m = /^([^/]+)\/([^/]+)$/.exec(String(entry).trim());
    if (m) refs.push({ host, project: m[1].toUpperCase(), repo: m[2] });
  }
  for (const folder of vscode.workspace.workspaceFolders || []) {
    const ref = await bitbucketClient.repoRefFromDir(folder.uri.fsPath, host);
    if (ref && !refs.some((r) => r.project === ref.project && r.repo === ref.repo)) refs.push(ref);
  }
  return refs;
}

/** Status bar text for the branch PR row (either source): `#N ✓ · approved/total`, or '' with no PR. */
function branchStatusText(row) {
  if (!row) return '';
  const parts = [`#${row.number}`];
  const c = row.checks;
  if (c && c.state === 'success') parts.push('✓');
  else if (c && c.state === 'failed') parts.push('✗');
  else if (c && c.state === 'in_progress') parts.push('…');
  const reviewers = row.reviewers || [];
  if (reviewers.length) parts.push(`· ${reviewers.filter((r) => r.status === 'APPROVED').length}/${reviewers.length}`);
  return `$(git-pull-request) ${parts.join(' ')}`;
}

/** Drives the Tasks webview (sidebar view + optional editor panel) and the branch-PR status bar segment. */
class TasksViewProvider {
  constructor(statusBarItem, context) {
    this.statusBarItem = statusBarItem;
    this.context = context;
    this.views = new Set(); // the sidebar and Panel webview views, whichever VS Code has resolved
    this.panel = undefined;
    this.source = null;
    this.queries = {};
    this.activePreset = {};
    this.page = 1;
    this.result = null;
    this.initialLoad = true;
    this.refreshing = false;
    this.loadingPage = null;
    this.hardError = null;
    this.settingsExpanded = false;
    this.status = { github: null, bitbucket: null, jira: null, tailscale: null };
    this.githubRepos = [];
    this.bitbucketRepos = [];
    this.jiraProjects = [];
    this.cache = new tasksState.PageCache({ ttlMs: Math.max(15, config().get('tasksRefreshSeconds', 120) || 120) * 1000 });
    this.tokenPromise = null;
    this.jiraTokenPromise = null;
    this.loadNonce = 0;
    this.view = 'list';
    this.detail = null; // { id, key, loading, error, data, comments, transitions, priorities, branchName, prompt }
    this.priorities = null;
    this.viewModes = (context && context.workspaceState.get('tasks.viewModes')) || {}; // source → list|board
    this.mergeMethods = {}; // repo label → {merge, squash, rebase}
    this.assignable = {}; // repo label → logins
    this.hydrating = new Set();
    this.mutations = new MutationQueue({
      getItem: (id) => this.findItem(id),
      setItem: (id, row) => this.patchItem(id, row),
      refetch: (id) => this.refetchItem(id),
      onChange: () => this.render(),
    });
  }

  /** Replaces the row in the list (and the open detail) with `row`; keeps detail-only fields the row lacks. */
  patchItem(id, row) {
    if (this.result && this.result.items) this.result = { ...this.result, items: this.result.items.map((i) => (i.id === id ? { ...i, ...row } : i)) };
    if (this.detail && this.detail.id === id && this.detail.data) this.detail = { ...this.detail, data: { ...this.detail.data, ...row } };
  }

  /** Fresh row after a write: Jira re-reads the issue (and comments/transitions when its detail is open); GitHub `gh … view`. */
  async refetchItem(id) {
    const item = this.findItem(id);
    if (!item) return null;
    if (item.source === 'github') {
      this.cache.invalidate();
      const open = this.detail && this.detail.id === id;
      const [row, conv] = await Promise.all([
        item.type === 'pr' ? tasksGithub.prDetail({ repo: item.repo, number: item.number }) : tasksGithub.issueDetail({ repo: item.repo, number: item.number }),
        open ? tasksGithub.conversation({ repo: item.repo, number: item.number }).catch(() => null) : null,
      ]);
      if (open && conv) this.detail = { ...this.detail, conversation: conv };
      return row;
    }
    if (item.source !== 'jira') return null;
    const cfg = this.cfg();
    const open = this.detail && this.detail.id === id;
    const data = await this.withJiraClient(async (client) => {
      const [issue, comments, transitions] = await Promise.all([client.issue(item.key), open ? client.comments(item.key) : null, open ? client.transitions(item.key) : null]);
      return tasksJira.mapDetail({ issue, comments: comments || [], transitions: transitions || [] }, { baseUrl: cfg.jiraBaseUrl });
    });
    if (open) this.detail = { ...this.detail, comments: data.comments, transitions: data.transitions, branchName: tasksJira.branchName(data), prompt: tasksJira.prompt(data) };
    this.cache.invalidate();
    return data;
  }

  /** Opens the inline detail: Jira loads issue + comments + transitions (+ priorities once); other sources show the row. */
  async openDetail(id) {
    const item = this.findItem(id);
    if (!item) return;
    this.view = 'detail';
    if (item.source === 'github') return this.openGitHubDetail(item);
    if (item.source !== 'jira') {
      this.detail = { id, key: item.number, data: item };
      return this.render();
    }
    this.detail = { id, key: item.key, loading: true };
    this.render();
    try {
      const cfg = this.cfg();
      const data = await this.withJiraClient(async (client) => {
        const [issue, comments, transitions, priorities] = await Promise.all([client.issue(item.key), client.comments(item.key), client.transitions(item.key), this.priorities ? null : client.priorities().catch(() => [])]);
        if (priorities) this.priorities = priorities.map((p) => ({ id: String(p.id), name: p.name, iconUrl: p.iconUrl || null }));
        return tasksJira.mapDetail({ issue, comments, transitions }, { baseUrl: cfg.jiraBaseUrl });
      });
      if (!this.detail || this.detail.id !== id) return;
      this.detail = { id, key: item.key, data, comments: data.comments, transitions: data.transitions, branchName: tasksJira.branchName(data), prompt: tasksJira.prompt(data) };
      this.patchItem(id, data);
    } catch (err) {
      if (this.detail && this.detail.id === id) this.detail = { id, key: item.key, error: err.message };
    }
    this.render();
  }

  /** GitHub detail: the row shows at once, the conversation (html media type) loads, PR cells hydrate if still light. */
  async openGitHubDetail(item) {
    const id = item.id;
    const prompt = `Work on ${item.type === 'pr' ? 'PR' : 'issue'} #${item.number} "${item.title}" (${item.url})`;
    this.detail = { id, key: item.number, data: item, tab: 'conversation', prompt };
    this.render();
    try {
      const [conv] = await Promise.all([
        tasksGithub.conversation({ repo: item.repo, number: item.number }),
        item.type === 'pr' && item.hydrated === false ? this.hydrate([id]) : null,
      ]);
      if (!this.detail || this.detail.id !== id) return;
      this.detail = { ...this.detail, data: this.findItem(id) || item, conversation: conv };
    } catch (err) {
      if (this.detail && this.detail.id === id) this.detail = { ...this.detail, error: tasksGithub.classifyGhError(err) };
    }
    this.render();
  }

  async loadChecks(id) {
    const item = this.findItem(id);
    if (!item || !item.headSha || (this.detail && this.detail.checks)) return;
    try {
      const checks = await tasksGithub.checkRuns({ repo: item.repo, sha: item.headSha });
      if (this.detail && this.detail.id === id) this.detail = { ...this.detail, checks };
    } catch (err) {
      if (this.detail && this.detail.id === id) this.detail = { ...this.detail, checks: { total: 0, runs: [] }, mutationError: tasksGithub.classifyGhError(err) };
    }
    this.render();
  }

  /** Assignee QuickPick: Unassigned · Assign to me · live search over `/user/assignable/search`. */
  async pickAssignee(id) {
    const item = this.findItem(id);
    if (!item || item.source !== 'jira') return;
    const me = this.status.jira && this.status.jira.ok ? this.status.jira : null;
    const fixed = [{ label: '$(circle-slash) Unassigned', name: null, displayName: '' }, ...(me && me.name ? [{ label: `$(account) Assign to me (${me.login})`, name: me.name, displayName: me.login }] : [])];
    const qp = vscode.window.createQuickPick();
    qp.placeholder = `Assign ${item.key} to…`;
    qp.matchOnDescription = true;
    qp.items = fixed;
    let timer;
    qp.onDidChangeValue((value) => {
      clearTimeout(timer);
      if (!value.trim()) return (qp.items = fixed);
      timer = setTimeout(async () => {
        qp.busy = true;
        try {
          const users = await this.withJiraClient((client) => client.assignable(item.key, value.trim()));
          qp.items = [...fixed, ...users.map((u) => ({ label: u.displayName, description: u.name, name: u.name, displayName: u.displayName }))];
        } catch (err) {
          qp.items = [...fixed, { label: `$(error) ${err.message}`, name: undefined }];
        } finally {
          qp.busy = false;
        }
      }, 250);
    });
    const picked = await new Promise((resolve) => {
      qp.onDidAccept(() => resolve(qp.selectedItems[0]));
      qp.onDidHide(() => resolve(undefined));
      qp.show();
    });
    qp.dispose();
    if (!picked || picked.name === undefined) return;
    const assignee = picked.name ? { name: picked.name, displayName: picked.displayName || picked.name, avatarUrl: null } : null;
    await this.runMutation(id, { assignee, assignees: assignee ? [assignee.displayName] : [] }, () => this.withJiraClient((client) => client.assign(item.key, picked.name)));
  }

  /** Posts patched cells for `ids` to every surface instead of re-rendering the page (keeps menus, selection, scroll). */
  postRows(ids) {
    const model = this.model();
    const kind = gridKind(model);
    const rows = ids.map((id) => this.findItem(id)).filter(Boolean).map((item) => ({ id: item.id, hydrated: item.hydrated !== false, cells: rowCells(item, model, kind) }));
    for (const surface of [...this.views, this.panel]) {
      if (surface) surface.webview.postMessage({ type: 'rows', rows });
    }
  }

  /** Lazy per-row `gh pr view` for checks / merge / reviews (≤ 4 in flight), plus the repo's merge methods once. */
  async hydrate(ids) {
    const todo = ids.map((id) => this.findItem(id)).filter((i) => i && i.source === 'github' && i.type === 'pr' && i.hydrated === false && !this.hydrating.has(i.id));
    const queue = [...todo];
    const worker = async () => {
      for (let item = queue.shift(); item; item = queue.shift()) {
        this.hydrating.add(item.id);
        try {
          const [detail] = await Promise.all([tasksGithub.prDetail({ repo: item.repo, number: item.number }), this.ensureMergeMethods(item.repo)]);
          this.patchItem(item.id, detail);
        } catch (err) {
          this.patchItem(item.id, { hydrated: true, checks: { state: 'none', label: 'Unavailable' }, merge: { state: 'unknown', label: 'Unknown' } });
          this.mutations.errors.set(item.id, { message: tasksGithub.classifyGhError(err), retry: () => { this.patchItem(item.id, { hydrated: false }); return this.hydrate([item.id]); } });
        } finally {
          this.hydrating.delete(item.id);
        }
        this.postRows([item.id]);
      }
    };
    await Promise.all(Array.from({ length: Math.min(4, queue.length) }, worker));
  }

  async ensureMergeMethods(repo) {
    if (this.mergeMethods[repo.label]) return this.mergeMethods[repo.label];
    this.mergeMethods[repo.label] = await tasksGithub.mergeMethods({ repo }).catch(() => ({ merge: true, squash: true, rebase: true }));
    return this.mergeMethods[repo.label];
  }

  async ensureAssignable(repo) {
    if (!this.assignable[repo.label]) this.assignable[repo.label] = await tasksGithub.listAssignable({ repo });
    return this.assignable[repo.label];
  }

  /** Multi-select QuickPick over the repo's assignable users; `current` pre-checked; returns `{add, remove}` or null. */
  async pickLogins(item, current, placeholder) {
    let logins;
    try {
      logins = await this.ensureAssignable(item.repo);
    } catch (err) {
      vscode.window.showErrorMessage(`Claude Code Tasks: ${tasksGithub.classifyGhError(err)}`);
      return null;
    }
    const all = [...new Set([...current, ...logins])];
    const items = all.map((login) => ({ label: login, picked: current.includes(login) }));
    const picked = await vscode.window.showQuickPick(items, { canPickMany: true, placeHolder: placeholder, matchOnDescription: true });
    if (!picked) return null;
    const next = picked.map((p) => p.label);
    return { add: next.filter((l) => !current.includes(l)), remove: current.filter((l) => !next.includes(l)), next };
  }

  async changeAssignees(id) {
    const item = this.findItem(id);
    if (!item || item.source !== 'github') return;
    const r = await this.pickLogins(item, item.assignees || [], `Assignees for #${item.number}`);
    if (!r || (!r.add.length && !r.remove.length)) return;
    return this.runMutation(id, { assignees: r.next }, () => tasksGithub.editIssue({ repo: item.repo, number: item.number, addAssignees: r.add, removeAssignees: r.remove }));
  }

  async changeReviewers(id) {
    const item = this.findItem(id);
    if (!item || item.source !== 'github' || item.type !== 'pr') return;
    const current = (item.reviewers || []).filter((x) => x.status === 'PENDING').map((x) => x.name);
    const r = await this.pickLogins(item, current, `Request reviewers for #${item.number} (up to 15)`);
    if (!r || (!r.add.length && !r.remove.length)) return;
    if (r.next.length > 15) return vscode.window.showWarningMessage('You can request up to 15 reviewers');
    const reviewers = [...(item.reviewers || []).filter((x) => x.status !== 'PENDING' || r.next.includes(x.name)), ...r.add.map((name) => ({ name, status: 'PENDING' }))];
    return this.runMutation(id, { reviewers }, () => tasksGithub.editPrReviewers({ repo: item.repo, number: item.number, add: r.add, remove: r.remove }));
  }

  async setIssueState(id, state, reason) {
    const item = this.findItem(id);
    if (!item || item.source !== 'github' || item.type !== 'issue') return;
    if (state === 'open') return this.runMutation(id, { state: 'open', stateReason: null }, () => tasksGithub.reopenIssue({ repo: item.repo, number: item.number }));
    let duplicateOf;
    if (reason === 'duplicate') {
      const raw = await vscode.window.showInputBox({ prompt: `Close #${item.number} as a duplicate of…`, placeHolder: 'Issue number', validateInput: (v) => (/^#?\d+$/.test(v.trim()) ? null : 'Enter an issue number') });
      if (!raw) return;
      duplicateOf = raw.trim().replace(/^#/, '');
    }
    return this.runMutation(id, { state: 'closed', stateReason: duplicateOf ? 'duplicate' : reason }, () => tasksGithub.closeIssue({ repo: item.repo, number: item.number, reason, duplicateOf }));
  }

  /** Orca's MergeCell confirm: "<Method> PR #N?" / "This will update the pull request on GitHub." */
  async mergePullRequest(id, method) {
    const item = this.findItem(id);
    if (!item || item.source !== 'github' || item.type !== 'pr') return;
    const labels = { merge: 'Merge', squash: 'Squash and merge', rebase: 'Rebase and merge' };
    const label = labels[method] || 'Merge';
    const ok = await vscode.window.showWarningMessage(`${label} PR #${item.number}?`, { modal: true, detail: 'This will update the pull request on GitHub.' }, label);
    if (ok !== label) return;
    return this.runMutation(id, { state: 'merged', merge: { state: 'merged', label: 'Merged' } }, () => tasksGithub.mergePr({ repo: item.repo, number: item.number, method }));
  }

  async setAutoMerge(id, enable) {
    const item = this.findItem(id);
    if (!item || item.source !== 'github' || item.type !== 'pr') return;
    const allowed = await this.ensureMergeMethods(item.repo);
    const method = ['squash', 'merge', 'rebase'].find((m) => allowed[m]) || 'squash';
    return this.runMutation(id, { autoMergeEnabled: !!enable }, () => tasksGithub.setAutoMerge({ repo: item.repo, number: item.number, method, enable: !!enable }));
  }

  /** Exit node QuickPick over the harness's peer list; "Direct" clears it. The switch is `tailscale set --exit-node=`. */
  async pickExitNode(clear) {
    const cfg = this.cfg();
    const net = this.status.tailscale;
    let target = '';
    if (!clear) {
      if (!net || !net.ok) return;
      const items = [
        { label: '$(circle-slash) Direct (no exit node)', description: net.exitNode ? '' : 'current', ip: '' },
        ...net.options.map((o) => ({ label: `$(globe) ${o.hostName}`, description: `${o.ip}${o.os ? ` · ${o.os}` : ''}${o.online ? '' : ' · offline'}${o.selected ? ' · current' : ''}`, ip: o.ip })),
      ];
      const picked = await vscode.window.showQuickPick(items, { placeHolder: 'Route this machine through an exit node' });
      if (!picked) return;
      target = picked.ip;
    }
    try {
      await tailscale.setExitNode(target, { tailscaleBinary: cfg.tailscaleBinary });
      vscode.window.setStatusBarMessage(target ? `Exit node set` : 'Exit node cleared', 3000);
    } catch (err) {
      vscode.window.showErrorMessage(`Tailscale: ${(err.stderr && String(err.stderr).trim().split('\n')[0]) || err.message}`);
    }
    this.status = { ...this.status, tailscale: await tailscale.status({ binary: cfg.binary, tailscaleBinary: cfg.tailscaleBinary }) };
    this.render();
  }

  viewMode() {
    return this.viewModes[this.source] || config().get('tasksViewMode', 'list') || 'list';
  }

  setViewMode(mode) {
    this.viewModes = { ...this.viewModes, [this.source]: mode === 'board' ? 'board' : 'list' };
    this.context.workspaceState.update('tasks.viewModes', this.viewModes);
    this.render();
  }

  rowError(id, message) {
    this.mutations.errors.set(id, { message, retry: () => Promise.resolve() });
    this.render();
  }

  /** A board drop = the matching in-panel action; unmapped drops are refused with an inline note (Orca's rule). */
  async moveRow(id, toKey) {
    const item = this.findItem(id);
    const col = (this.model().board || []).find((c) => c.key === toKey);
    if (!item || !col) return;
    if (item.source === 'jira') return this.moveJira(item, col);
    if (item.source === 'github' && item.type === 'issue') {
      if (col.key.endsWith('open')) return this.setIssueState(id, 'open');
      const reason = await vscode.window.showQuickPick([{ label: 'Close as completed', reason: 'completed' }, { label: 'Close as not planned', reason: 'not_planned' }, { label: 'Close as duplicate…', reason: 'duplicate' }], { placeHolder: `Close #${item.number}` });
      return reason ? this.setIssueState(id, 'closed', reason.reason) : undefined;
    }
    if (item.source === 'github') {
      const target = col.key.replace(/^pr:/, '');
      if (target === 'merged') {
        const allowed = await this.ensureMergeMethods(item.repo);
        const method = await vscode.window.showQuickPick(['squash', 'merge', 'rebase'].filter((m) => allowed[m]).map((m) => ({ label: m, method: m })), { placeHolder: `Merge method for #${item.number}` });
        return method ? this.mergePullRequest(id, method.method) : undefined;
      }
      if (target === 'closed') return this.runMutation(id, { state: 'closed', merge: { state: 'closed', label: 'Closed' } }, () => tasksGithub.setPrState({ repo: item.repo, number: item.number, state: 'closed' }));
      if (target === 'open' && item.state === 'closed') return this.runMutation(id, { state: 'open' }, () => tasksGithub.setPrState({ repo: item.repo, number: item.number, state: 'open' }));
      if (target === 'open' && item.state === 'draft') return this.runMutation(id, { state: 'open' }, () => tasksGithub.setPrReady({ repo: item.repo, number: item.number }));
      if (target === 'draft' && item.state === 'open') return this.runMutation(id, { state: 'draft' }, () => tasksGithub.setPrReady({ repo: item.repo, number: item.number, undo: true }));
      return this.rowError(id, `No action for "${col.label}" — submit a review from the PR page`);
    }
    return this.rowError(id, 'Bitbucket is read-only in this panel');
  }

  /** Jira drop: the transition(s) landing in the column — 0 refuses, 1 applies, many ask; required fields collected first. */
  async moveJira(item, col) {
    let transitions;
    try {
      transitions = (await this.withJiraClient((client) => client.transitions(item.key))).map(tasksJira.mapTransition);
    } catch (err) {
      return this.rowError(item.id, err.message);
    }
    let matches = tasksBoard.jiraTransitionsTo(transitions, col);
    if (!matches.length && col.category) matches = transitions.filter((t) => t.to.category === col.category);
    if (!matches.length) return this.rowError(item.id, `No transition to "${col.label}" from ${item.status ? item.status.name : 'this status'}`);
    let t = matches[0];
    if (matches.length > 1) {
      const pick = await vscode.window.showQuickPick(matches.map((m) => ({ label: m.name, description: `→ ${m.to.name}`, t: m })), { placeHolder: `Transition ${item.key} to ${col.label}` });
      if (!pick) return;
      t = pick.t;
    }
    const req = t.requirement || { kind: 'none' };
    if (req.kind === 'unsupported') return this.rowError(item.id, `"${t.name}" needs ${req.fields.map((f) => f.name || f.key).join(', ')} — open in Jira`);
    let fields;
    let comment = '';
    if (req.kind === 'form') {
      if (req.resolution) {
        const r = await vscode.window.showQuickPick((req.resolution.allowedValues || []).map((v) => ({ label: v.name || v.value || v.id, id: v.id || v.value || v.name })), { placeHolder: 'Resolution' });
        if (!r) return;
        fields = { resolution: { id: String(r.id) } };
      }
      comment = (await vscode.window.showInputBox({ prompt: req.commentRequired ? 'Comment (required)' : 'Comment (optional)', ignoreFocusOut: true })) || '';
      if (req.commentRequired && !comment.trim()) return;
    }
    if (this.detail && this.detail.id === item.id) this.detail = { ...this.detail, transitions };
    return this.runMutation(item.id, { status: { name: t.to.name, category: t.to.category }, state: t.to.category === 'done' ? 'closed' : 'open' }, () => this.withJiraClient((client) => client.transition(item.key, t.id, comment, fields)));
  }

  /** Optimistic write through the queue; the error lands on the detail banner / row instead of a toast. */
  async runMutation(id, patch, write) {
    try {
      await this.mutations.enqueue({ id, patch, write });
      if (this.detail && this.detail.id === id) this.detail = { ...this.detail, mutationError: null };
    } catch (err) {
      if (this.detail && this.detail.id === id) this.detail = { ...this.detail, mutationError: err.message };
    }
    this.render();
  }

  cfg() {
    const cfg = config();
    const visible = (cfg.get('tasksVisibleSources', TASK_SOURCES) || []).filter((s) => TASK_SOURCES.includes(s));
    return {
      binary: cfg.get('binary') || 'claude-code',
      baseUrl: cfg.get('bitbucketUrl') || 'https://bitbucket.biscrum.com',
      jiraBaseUrl: cfg.get('tasksJiraBaseUrl') || '',
      visible: visible.length ? visible : TASK_SOURCES,
      defaultSource: cfg.get('tasksDefaultSource', 'jira') || 'jira',
      defaultQuery: {
        github: cfg.get('tasksDefaultQuery.github') || tasksState.DEFAULT_QUERY.github,
        bitbucket: cfg.get('tasksDefaultQuery.bitbucket') || tasksState.DEFAULT_QUERY.bitbucket,
        jira: cfg.get('tasksDefaultQuery.jira') || tasksState.DEFAULT_QUERY.jira,
      },
      tailscaleBinary: cfg.get('tailscaleBinary') || 'tailscale',
      jiraProjects: (cfg.get('tasksJiraProjects', []) || []).map((k) => String(k).trim().toUpperCase()).filter(Boolean),
      pageSize: Math.max(5, cfg.get('tasksPageSize', 36) || 36),
    };
  }

  dcHost() {
    try {
      return new URL(this.cfg().baseUrl).hostname.toLowerCase();
    } catch {
      return '';
    }
  }

  wireWebview(webview) {
    webview.options = { enableScripts: true };
    webview.onDidReceiveMessage((msg) => this.handleMessage(msg).catch((err) => vscode.window.showErrorMessage(`Claude Code Tasks: ${err.message}`)));
  }

  resolveWebviewView(webviewView) {
    if (webviewView.viewType === 'claudeCodeTasks.page') {
      // The left-nav view is a launcher: too narrow for the page, so it lists the sources and opens Tasks in a window.
      this.sidebar = webviewView;
      webviewView.onDidDispose(() => {
        if (this.sidebar === webviewView) this.sidebar = undefined;
      });
      this.wireWebview(webviewView.webview);
      webviewView.onDidChangeVisibility(() => {
        if (webviewView.visible) this.openFromSidebar();
      });
      this.render();
      this.openFromSidebar();
      return;
    }
    this.views.add(webviewView);
    webviewView.onDidDispose(() => this.views.delete(webviewView));
    this.wireWebview(webviewView.webview);
    this.render();
  }

  /** Left-nav click: reveal Tasks where it already lives, else pop it out into a new window. */
  openFromSidebar(source) {
    if (source && TASK_SOURCES.includes(source) && source !== this.source) this.handleMessage({ type: 'source', source });
    if (this.panel) return this.panel.reveal();
    if ([...this.views].some((v) => v.visible)) return;
    return this.popOut();
  }

  openWhere() {
    if (this.panel) return this.panelFloating ? 'window' : 'editor';
    if ([...this.views].some((v) => v.visible)) return 'panel';
    return null;
  }

  /** Opens (or reveals) the single Tasks editor-tab panel; same model and message handling as the sidebar view. */
  openInEditor() {
    if (this.panel) {
      this.panel.reveal(vscode.ViewColumn.Active);
      return;
    }
    // One place at a time: the views that were showing Tasks close, and come back when this tab/window closes.
    this.restoreTo = [...this.views].filter((v) => v.visible).map((v) => v.viewType);
    const panel = vscode.window.createWebviewPanel('claudeCodeTasks.editor', 'Tasks', vscode.ViewColumn.Active, { enableScripts: true, retainContextWhenHidden: true });
    this.panel = panel;
    this.wireWebview(panel.webview);
    panel.onDidDispose(() => {
      if (this.panel !== panel) return;
      this.panel = undefined;
      this.panelFloating = false;
      const back = this.restoreTo || [];
      this.restoreTo = [];
      this.render();
      for (const viewType of back) vscode.commands.executeCommand(`${viewType}.focus`);
    });
    this.render();
    for (const viewType of this.restoreTo) {
      vscode.commands.executeCommand(viewType === 'claudeCodeTasks.panel' ? 'workbench.action.closePanel' : 'workbench.action.closeSidebar');
    }
  }

  /** "Bring back here": closes the editor tab / floating window; its dispose handler re-focuses the view. */
  bringBack(viewType) {
    if (!this.panel) return;
    if (viewType && !(this.restoreTo || []).includes(viewType)) this.restoreTo = [...(this.restoreTo || []), viewType];
    this.panel.dispose();
  }

  visible() {
    return [...this.views].some((v) => v.visible) || !!(this.panel && this.panel.visible);
  }

  /** Pops the page out: the editor tab, then VS Code's "move editor into new window" once that tab is the active editor. */
  async popOut() {
    if (this.panel && this.panelFloating) return this.panel.reveal(); // already in its own window: don't move an empty group
    this.openInEditor();
    const panel = this.panel;
    if (!panel.active) {
      await new Promise((resolve) => {
        const sub = panel.onDidChangeViewState((e) => {
          if (!e.webviewPanel.active) return;
          sub.dispose();
          resolve();
        });
        setTimeout(() => {
          sub.dispose();
          resolve();
        }, 1500);
      });
    }
    if (!panel.active) return; // never move whatever else is active
    await vscode.commands.executeCommand('workbench.action.moveEditorToNewWindow');
    this.panelFloating = true;
    this.render();
  }

  /** Focuses the surface `tasksOpenIn` names: the Panel view, the sidebar view, or the editor tab. */
  async show() {
    const where = config().get('tasksOpenIn', 'panel') || 'panel';
    if (where === 'editor') return this.openInEditor();
    if (where === 'sidebar') {
      await vscode.commands.executeCommand('workbench.view.extension.claudeCodeTasks');
      return vscode.commands.executeCommand('claudeCodeTasks.page.focus');
    }
    await vscode.commands.executeCommand('workbench.view.extension.claudeCodeTasksPanel');
    return vscode.commands.executeCommand('claudeCodeTasks.panel.focus');
  }

  currentQuery() {
    return this.queries[this.source] != null ? this.queries[this.source] : this.cfg().defaultQuery[this.source];
  }

  /** Repos, credentials and the first page; called once on activation and again from "Re-check connections". */
  async init() {
    const cfg = this.cfg();
    const stored = this.context.workspaceState.get('tasks.githubRepos');
    [this.githubRepos, this.bitbucketRepos] = await Promise.all([discoverGitHubRepos(stored), discoverBitbucketRepos(this.dcHost())]);
    this.jiraProjects = cfg.jiraProjects;
    if (!this.source || !cfg.visible.includes(this.source)) this.source = cfg.visible.includes(cfg.defaultSource) ? cfg.defaultSource : cfg.visible[0];
    await this.doctor();
    await Promise.all([this.load({ force: true }), this.loadBranch()]);
  }

  async doctor() {
    const cfg = this.cfg();
    const noToken = (err) => ({ ok: false, message: looksLikeMissingToken(err.message) ? 'No token' : err.message });
    const [github, bitbucket, jira, net] = await Promise.all([
      tasksGithub.authStatus(),
      this.withClient((client) => client.doctor()).catch(noToken),
      cfg.jiraBaseUrl ? this.withJiraClient((client) => client.myself()).catch(noToken) : Promise.resolve({ ok: false, message: 'No Jira URL' }),
      tailscale.status({ binary: cfg.binary, tailscaleBinary: cfg.tailscaleBinary }),
    ]);
    const failed = (r) => ({ ok: false, message: r.message || (r.reason === 'rejected' ? 'Token rejected' : 'Unreachable') });
    this.status = {
      github,
      bitbucket: bitbucket.ok ? { ok: true, login: decodeURIComponent(bitbucket.user.username) } : failed(bitbucket),
      jira: jira.ok ? { ok: true, login: jira.user.displayName || jira.user.name, name: jira.user.name } : failed(jira),
      tailscale: net,
    };
    this.render();
  }

  /** Resolves BITBUCKET_TOKEN once; `forceRefresh` re-resolves after a 401/403 (same cache the trees used). */
  getToken(forceRefresh) {
    if (forceRefresh) this.tokenPromise = null;
    if (!this.tokenPromise) {
      this.tokenPromise = bitbucketClient.resolveToken({ binary: this.cfg().binary }).catch((err) => {
        this.tokenPromise = null;
        throw err;
      });
    }
    return this.tokenPromise;
  }

  /** Resolves JIRA_TOKEN once (vault → env → token files); `forceRefresh` re-resolves after a 401/403. */
  getJiraToken(forceRefresh) {
    if (forceRefresh) this.jiraTokenPromise = null;
    if (!this.jiraTokenPromise) {
      this.jiraTokenPromise = jiraClient.resolveToken({ binary: this.cfg().binary }).catch((err) => {
        this.jiraTokenPromise = null;
        throw err;
      });
    }
    return this.jiraTokenPromise;
  }

  async withJiraClient(op) {
    const baseUrl = this.cfg().jiraBaseUrl;
    const token = await this.getJiraToken();
    try {
      return await op(jiraClient.createClient({ baseUrl, token }));
    } catch (err) {
      if (err.status !== 401 && err.status !== 403) throw err;
      return op(jiraClient.createClient({ baseUrl, token: await this.getJiraToken(true) }));
    }
  }

  async withClient(op) {
    const { baseUrl } = this.cfg();
    const token = await this.getToken();
    try {
      return await op(bitbucketClient.createClient({ baseUrl, token }));
    } catch (err) {
      if (err.status !== 401 && err.status !== 403) throw err;
      return op(bitbucketClient.createClient({ baseUrl, token: await this.getToken(true) }));
    }
  }

  fetchPage(source, query, page) {
    const cfg = this.cfg();
    const parsed = taskQuery.parseTaskQuery(taskQuery.stripRepoQualifiers(query));
    if (source === 'github') {
      const repos = this.githubRepos.filter((r) => r.selected);
      if (!repos.length) return Promise.resolve({ items: [], errors: [], totalPages: 1, failedRepos: 0 });
      return tasksGithub.listWorkItems({ repos, query: parsed, page, limit: cfg.pageSize });
    }
    if (source === 'jira') {
      return this.withJiraClient((client) => tasksJira.list({ client, query: parsed, projects: this.jiraProjects, page, limit: cfg.pageSize, baseUrl: cfg.jiraBaseUrl }));
    }
    return this.withClient((client) => tasksBitbucket.list({ client, query: parsed, repos: this.bitbucketRepos, page, limit: cfg.pageSize, jiraBaseUrl: cfg.jiraBaseUrl }));
  }

  /** Loads the current page through the cache; `force` bypasses it (manual refresh, timer) and keeps rows on screen. */
  async load(opts) {
    const { force = false, page = this.page } = opts || {};
    const source = this.source;
    const query = this.currentQuery();
    const nonce = ++this.loadNonce;
    const scopes = { github: () => this.githubRepos.filter((r) => r.selected).map((r) => r.label), bitbucket: () => this.bitbucketRepos.map((r) => `${r.project}/${r.repo}`), jira: () => this.jiraProjects };
    const key = { source, repos: (scopes[source] || scopes.bitbucket)(), query, page };
    const cached = force ? null : this.cache.get(key);
    if (cached) {
      this.page = page;
      this.result = cached;
      this.hardError = null;
      this.initialLoad = false;
      this.render();
      return;
    }
    if (page !== this.page) this.loadingPage = page;
    else this.refreshing = true;
    this.render();
    try {
      const result = await this.cache.load(key, () => this.fetchPage(source, query, page), { force });
      if (nonce !== this.loadNonce) return;
      this.page = page;
      this.result = result;
      this.hardError = null;
    } catch (err) {
      if (nonce !== this.loadNonce) return;
      this.hardError = source === 'github' ? tasksGithub.classifyGhError(err) : err.message;
      if (!this.result) this.result = { items: [], errors: [], totalPages: 1, failedRepos: 0 };
    } finally {
      if (nonce === this.loadNonce) {
        this.initialLoad = false;
        this.refreshing = false;
        this.loadingPage = null;
        this.render();
      }
    }
  }

  /** The current branch's PR from whichever source the first workspace folder's remote belongs to → status bar. */
  async loadBranch() {
    const dir = workspaceDir();
    const [branch, remoteUrl] = await Promise.all([currentBranch(dir), originRemoteUrl(dir)]);
    let row = null;
    try {
      const ref = remoteUrl ? bitbucketClient.parseRepoRef(remoteUrl, this.dcHost()) : null;
      const gh = remoteUrl ? tasksGithub.repoFromRemote(remoteUrl) : null;
      if (ref && branch) {
        const pr = await this.withClient((client) => client.pullRequestForBranch(ref, branch));
        row = pr ? tasksBitbucket.mapRow(pr, { jiraBaseUrl: this.cfg().jiraBaseUrl }) : null;
      } else if (gh && branch) {
        const parsed = taskQuery.parseTaskQuery(`is:pr head:${branch}`);
        const res = await tasksGithub.listWorkItems({ repos: [gh], query: parsed, page: 1, limit: 5 });
        row = res.items.find((i) => i.branch === branch && i.state !== 'closed') || res.items.find((i) => i.branch === branch) || null;
      }
    } catch {
      row = null;
    }
    this.branchPr = row;
    this.updateStatusBar();
  }

  updateStatusBar() {
    if (!this.statusBarItem) return;
    const text = config().get('tasksStatusBar', true) ? branchStatusText(this.branchPr) : '';
    if (!text) {
      this.statusBarItem.hide();
      return;
    }
    this.statusBarItem.text = text;
    this.statusBarItem.tooltip = this.branchPr.title;
    this.statusBarItem.show();
  }

  banner() {
    const s = this.status[this.source];
    if (!s || s.ok) return null;
    if (this.source === 'github') return { kind: 'warn', text: `GitHub: ${s.message}`, action: { act: 'signIn', label: 'Sign in' } };
    if (this.source === 'jira') return { kind: 'warn', text: `Jira: ${s.message}`, action: { act: 'signIn', label: 'Set token' } };
    return { kind: 'warn', text: `Bitbucket: ${s.message}`, action: { act: 'signIn', label: 'Set token' } };
  }

  model() {
    const cfg = this.cfg();
    const query = this.currentQuery();
    const parsed = taskQuery.parseTaskQuery(query);
    const presets = tasksState.presetsFor(this.source);
    const result = this.result || { items: [], errors: [], totalPages: 1, failedRepos: 0 };
    const repos =
      this.source === 'github'
        ? this.githubRepos.map((r) => ({ label: r.label, selected: r.selected }))
        : this.source === 'jira'
          ? this.jiraProjects.map((k) => ({ label: k, selected: true }))
          : this.bitbucketRepos.map((r) => ({ label: `${r.project}/${r.repo}`, selected: true }));
    const defaultPreset = presets.find((p) => p.query === cfg.defaultQuery[this.source]);
    const activePreset = presets.find((p) => p.query === query);
    return {
      sources: cfg.visible.map((id) => ({ id })),
      source: this.source,
      host: 'github.com',
      query,
      scope: parsed.scope,
      state: parsed.state || 'all',
      presets,
      activePreset: activePreset ? activePreset.id : null,
      defaultPreset: defaultPreset ? defaultPreset.id : null,
      repos,
      repoCount: repos.filter((r) => r.selected).length,
      banner: this.banner(),
      loading: this.initialLoad,
      refreshing: this.refreshing,
      loadingPage: this.loadingPage,
      items: result.items,
      errors: result.errors,
      failedRepos: result.failedRepos,
      hardError: this.hardError,
      page: this.page,
      totalPages: result.totalPages,
      now: Date.now(),
      view: this.view,
      rowErrors: Object.fromEntries([...this.mutations.errors].map(([id, e]) => [id, e.message])),
      viewMode: this.viewMode(),
      board: this.viewMode() === 'board' ? tasksBoard.groupRows(result.items, this.source, parsed.scope, { ensure: this.source === 'jira' ? [{ label: 'Done', category: 'done' }] : [] }) : null,
      pendingIds: result.items.filter((i) => this.mutations.isPending(i.id)).map((i) => i.id),
      mergeMethods: this.mergeMethods,
      detail: this.detail ? { ...this.detail, pending: this.mutations.isPending(this.detail.id), priorities: this.priorities || [] } : null,
      settings: {
        expanded: this.settingsExpanded,
        defaultSource: cfg.defaultSource,
        github: { status: this.status.github, visible: cfg.visible.includes('github'), repos: this.githubRepos.map((r) => r.label) },
        bitbucket: { status: this.status.bitbucket, visible: cfg.visible.includes('bitbucket'), repos: this.bitbucketRepos.map((r) => `${r.project}/${r.repo}`), baseUrl: cfg.baseUrl },
        jira: { status: this.status.jira, visible: cfg.visible.includes('jira'), repos: this.jiraProjects, baseUrl: cfg.jiraBaseUrl },
        tailscale: { status: this.status.tailscale },
      },
    };
  }

  render() {
    const model = this.model();
    if (this.panel) this.panel.webview.html = renderTasksHtml(model, { nonce: nonce(), cspSource: this.panel.webview.cspSource });
    if (this.sidebar) this.sidebar.webview.html = renderLauncherHtml({ ...model, openWhere: this.openWhere() }, { nonce: nonce(), cspSource: this.sidebar.webview.cspSource });
    for (const view of this.views) {
      // While the editor tab / window owns the page, views show a pointer to it instead of a second live copy.
      view.webview.html = this.panel ? renderElsewhereHtml({ floating: !!this.panelFloating, viewType: view.viewType, nonce: nonce(), cspSource: view.webview.cspSource }) : renderTasksHtml(model, { nonce: nonce(), cspSource: view.webview.cspSource });
    }
  }

  setQuery(query) {
    this.queries[this.source] = query;
    return this.load({ page: 1 });
  }

  findItem(id) {
    return ((this.result && this.result.items) || []).find((i) => i.id === id);
  }

  async handleMessage(msg) {
    if (!msg || !msg.type) return;
    const cfg = this.cfg();
    const terminal = () => dashboardTerminal();
    switch (msg.type) {
      case 'refresh':
        return Promise.all([this.load({ force: true }), this.loadBranch()]);
      case 'source':
        if (!cfg.visible.includes(msg.source)) return;
        this.source = msg.source;
        this.page = 1;
        this.result = null;
        this.hardError = null;
        this.initialLoad = true;
        return this.load({ page: 1 });
      case 'query':
        return this.setQuery(String(msg.query || '').trim());
      case 'preset': {
        const preset = tasksState.presetsFor(this.source).find((p) => p.id === msg.id);
        return preset ? this.setQuery(preset.query) : undefined;
      }
      case 'setDefault': {
        const preset = tasksState.presetsFor(this.source).find((p) => p.id === msg.id);
        if (!preset) return;
        await config().update(`tasksDefaultQuery.${this.source}`, preset.query, vscode.ConfigurationTarget.Global);
        return this.render();
      }
      case 'filter': {
        if (msg.key === 'state') return this.setQuery(taskQuery.withQualifier(this.currentQuery(), 'state', msg.value));
        if (msg.key === 'scope') {
          const parsed = taskQuery.parseTaskQuery(this.currentQuery());
          parsed.scope = ['all', 'issue', 'pr'].includes(msg.value) ? msg.value : 'all';
          if (parsed.scope !== 'pr') {
            parsed.draft = false;
            parsed.reviewRequested = null;
            parsed.reviewedBy = null;
            if (parsed.state === 'merged') parsed.state = 'open';
          }
          return this.setQuery(taskQuery.serializeTaskQuery(parsed));
        }
        return;
      }
      case 'page':
        if (Number.isFinite(msg.page) && msg.page >= 1 && msg.page <= ((this.result && this.result.totalPages) || 1)) return this.load({ page: msg.page });
        return;
      case 'repos': {
        const labels = Array.isArray(msg.repos) ? msg.repos : [];
        this.githubRepos = this.githubRepos.map((r) => ({ ...r, selected: labels.includes(r.label) }));
        await this.context.workspaceState.update('tasks.githubRepos', labels);
        return this.load({ page: 1 });
      }
      case 'open':
        if (msg.url) await vscode.env.openExternal(vscode.Uri.parse(msg.url));
        return;
      case 'detail':
        return this.openDetail(msg.id);
      case 'back':
        this.view = 'list';
        this.detail = null;
        return this.render();
      case 'comment': {
        const item = this.findItem(msg.id);
        if (!item || !msg.body) return;
        if (item.source === 'github') {
          return this.runMutation(msg.id, null, async () => {
            const entry = await tasksGithub.addComment({ repo: item.repo, number: item.number, body: msg.body });
            if (this.detail && this.detail.id === msg.id && this.detail.conversation) {
              const conv = this.detail.conversation;
              this.detail = { ...this.detail, conversation: { ...conv, entries: [...conv.entries, entry], commentCount: conv.commentCount + 1 } };
            }
            return entry;
          });
        }
        if (item.source !== 'jira') return;
        return this.runMutation(msg.id, null, () => this.withJiraClient((client) => client.addComment(item.key, msg.body)));
      }
      case 'tab': {
        if (!this.detail || this.detail.id !== msg.id) return;
        this.detail = { ...this.detail, tab: msg.tab === 'checks' ? 'checks' : 'conversation' };
        this.render();
        if (msg.tab === 'checks') return this.loadChecks(msg.id);
        return;
      }
      case 'transition': {
        const item = this.findItem(msg.id);
        const t = (this.detail && this.detail.transitions || []).find((x) => x.id === String(msg.transitionId));
        if (!item || !t) return;
        const fields = msg.resolutionId ? { resolution: { id: String(msg.resolutionId) } } : undefined;
        return this.runMutation(msg.id, { status: { name: t.to.name, category: t.to.category }, state: t.to.category === 'done' ? 'closed' : 'open' }, () => this.withJiraClient((client) => client.transition(item.key, t.id, msg.comment || '', fields)));
      }
      case 'priority': {
        const item = this.findItem(msg.id);
        const p = (this.priorities || []).find((x) => x.id === String(msg.priorityId));
        if (!item || !p) return;
        return this.runMutation(msg.id, { priority: { name: p.name, iconUrl: p.iconUrl } }, () => this.withJiraClient((client) => client.update(item.key, { fields: { priority: { id: p.id } } })));
      }
      case 'assign':
        return this.pickAssignee(msg.id);
      case 'hydrate':
        return this.hydrate(msg.ids || []);
      case 'openWindow':
        return this.openFromSidebar(msg.source);
      case 'openPanel':
        await vscode.commands.executeCommand('workbench.view.extension.claudeCodeTasksPanel');
        return vscode.commands.executeCommand('claudeCodeTasks.panel.focus');
      case 'revealElsewhere':
        if (this.panel) this.panel.reveal();
        return;
      case 'bringBack':
        return this.bringBack(msg.viewType);
      case 'viewMode':
        return this.setViewMode(msg.mode);
      case 'exitNode':
        return this.pickExitNode(false);
      case 'exitNodeOff':
        return this.pickExitNode(true);
      case 'move':
        return this.moveRow(msg.id, msg.to);
      case 'assignees':
        return this.changeAssignees(msg.id);
      case 'reviewers':
        return this.changeReviewers(msg.id);
      case 'issueState':
        return this.setIssueState(msg.id, msg.state, msg.reason);
      case 'merge':
        return this.mergePullRequest(msg.id, msg.method);
      case 'autoMerge':
        return this.setAutoMerge(msg.id, msg.enable);
      case 'retry': {
        const e = this.mutations.errorFor(msg.id);
        if (e) e.retry().catch(() => {});
        return;
      }
      case 'clearError':
        this.mutations.clearError(msg.id);
        if (this.detail && this.detail.id === msg.id) this.detail = { ...this.detail, mutationError: null };
        return this.render();
      case 'copy':
        await vscode.env.clipboard.writeText(msg.text || '');
        vscode.window.setStatusBarMessage(`${msg.label || 'Text'} copied`, 2000);
        return;
      case 'start': {
        const item = this.findItem(msg.id);
        if (!item) return;
        await vscode.env.clipboard.writeText(`Work on ${item.type === 'pr' ? 'PR' : 'issue'} #${item.number} "${item.title}" (${item.url})`);
        const t = terminal();
        t.sendText(`${cfg.binary} claude`);
        t.show();
        vscode.window.showInformationMessage('Task prompt copied — paste it into Claude.');
        return;
      }
      case 'newIssue': {
        const repo = this.githubRepos.find((r) => r.selected);
        if (repo) await vscode.env.openExternal(vscode.Uri.parse(`https://github.com/${repo.label}/issues/new`));
        return;
      }
      case 'signIn': {
        const t = terminal();
        t.sendText(msg.source === 'bitbucket' ? `${cfg.binary} secrets set BITBUCKET_TOKEN` : msg.source === 'jira' ? `${cfg.binary} secrets set JIRA_TOKEN` : 'gh auth login');
        t.show();
        this.tokenPromise = null;
        this.jiraTokenPromise = null;
        return;
      }
      case 'doctor':
        this.cache.invalidate();
        return this.init();
      case 'toggleSourceVisible': {
        const next = cfg.visible.filter((s) => s !== msg.source);
        if (msg.visible) next.push(msg.source);
        if (!next.length) return this.render(); // the last visible source stays on
        await config().update('tasksVisibleSources', TASK_SOURCES.filter((s) => next.includes(s)), vscode.ConfigurationTarget.Global);
        if (!next.includes(this.source)) return this.handleMessage({ type: 'source', source: next[0] });
        return this.render();
      }
      case 'defaultSource':
        if (TASK_SOURCES.includes(msg.source)) await config().update('tasksDefaultSource', msg.source, vscode.ConfigurationTarget.Global);
        return this.render();
      case 'settingsExpanded':
        this.settingsExpanded = !!msg.expanded;
        return;
      case 'openSettings':
        return vscode.commands.executeCommand('workbench.action.openSettings', 'claudeCodeAgents.tasks');
      default:
        return;
    }
  }
}

/** Installs the GitHub Pull Requests and Issues extension once, if missing and not disabled by setting. */
async function installGitHubPrExtension() {
  if (!config().get('tasksInstallGitHubExtension', true)) return;
  if (vscode.extensions.getExtension(GITHUB_PR_EXTENSION_ID)) return;
  try {
    vscode.window.showInformationMessage('Installing GitHub Pull Requests and Issues for the Tasks views');
    await vscode.commands.executeCommand('workbench.extensions.installExtension', GITHUB_PR_EXTENSION_ID);
  } catch {
    // Best-effort: offline, marketplace unreachable, or already installing — the Tasks views work without it.
  }
}

function activate(context) {
  context.subscriptions.push(vscode.commands.registerCommand('claudeCodeAgents.dispatch', () => runDispatch()));
  installGitHubPrExtension();

  function createUsageStatusBarItem() {
    const cfg = config();
    const alignment =
      cfg.get('usageStatusBarAlignment', 'left') === 'right'
        ? vscode.StatusBarAlignment.Right
        : vscode.StatusBarAlignment.Left;
    const priority = cfg.get('usageStatusBarPriority', 1000000) || 1000000;
    const item = vscode.window.createStatusBarItem(alignment, priority);
    item.name = 'Agents Dashboard';
    item.command = 'claudeCodeAgents.showUsage';
    item.text = '$(sparkle) usage';
    item.show();
    return item;
  }

  let statusBarItem = createUsageStatusBarItem();
  let statusBarDisposable = { dispose: () => statusBarItem.dispose() };
  context.subscriptions.push(statusBarDisposable);

  const usageProvider = new UsageViewProvider(statusBarItem);

  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (
        !e.affectsConfiguration('claudeCodeAgents.usageStatusBarAlignment') &&
        !e.affectsConfiguration('claudeCodeAgents.usageStatusBarPriority')
      ) {
        return;
      }
      statusBarDisposable.dispose();
      statusBarItem = createUsageStatusBarItem();
      statusBarDisposable = { dispose: () => statusBarItem.dispose() };
      context.subscriptions.push(statusBarDisposable);
      usageProvider.statusBarItem = statusBarItem;
      usageProvider.render();
    })
  );
  context.subscriptions.push(
    vscode.window.registerWebviewViewProvider('claudeCodeAgents.usage', usageProvider, {
      webviewOptions: { retainContextWhenHidden: true },
    })
  );

  context.subscriptions.push(
    vscode.commands.registerCommand('claudeCodeAgents.showUsage', async () => {
      if (config().get('usageOpenIn', 'sidebar') === 'editor') {
        usageProvider.openInEditor();
        return;
      }
      await vscode.commands.executeCommand('workbench.view.extension.claudeCodeAgents');
      await vscode.commands.executeCommand('claudeCodeAgents.usage.focus');
    }),
    vscode.commands.registerCommand('claudeCodeAgents.refreshUsage', () => usageProvider.load()),
    vscode.commands.registerCommand('claudeCodeAgents.openUsageEditor', () => usageProvider.openInEditor())
  );

  context.subscriptions.push(
    vscode.commands.registerCommand('claudeCodeAgents.servePhone', () => {
      const binary = config().get('binary') || 'claude-code';
      const terminal = dashboardTerminal();
      terminal.sendText(vscodeServeArgv(binary, {}).join(' '));
      terminal.show();
    }),
    vscode.commands.registerCommand('claudeCodeAgents.servePhoneStop', () => {
      const binary = config().get('binary') || 'claude-code';
      const terminal = dashboardTerminal();
      terminal.sendText(vscodeServeArgv(binary, { stop: true }).join(' '));
      terminal.show();
    })
  );

  usageProvider.load();
  const usageSeconds = Math.max(15, config().get('usageRefreshSeconds', 60) || 60);
  const usageTimer = setInterval(() => usageProvider.load(), usageSeconds * 1000);
  context.subscriptions.push({ dispose: () => clearInterval(usageTimer) });
  const usageTickTimer = setInterval(() => usageProvider.tick(), 60 * 1000);
  context.subscriptions.push({ dispose: () => clearInterval(usageTickTimer) });

  function createTasksStatusBarItem() {
    const cfg = config();
    const alignment =
      cfg.get('usageStatusBarAlignment', 'left') === 'right'
        ? vscode.StatusBarAlignment.Right
        : vscode.StatusBarAlignment.Left;
    const priority = (cfg.get('usageStatusBarPriority', 1000000) || 1000000) - 1;
    const item = vscode.window.createStatusBarItem(alignment, priority);
    item.name = 'Claude Code Tasks';
    item.command = 'claudeCodeTasks.show';
    return item;
  }

  let tasksStatusBarItem = createTasksStatusBarItem();
  let tasksStatusBarDisposable = { dispose: () => tasksStatusBarItem.dispose() };
  context.subscriptions.push(tasksStatusBarDisposable);

  const tasksProvider = new TasksViewProvider(tasksStatusBarItem, context);

  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration('claudeCodeAgents.usageStatusBarAlignment') || e.affectsConfiguration('claudeCodeAgents.usageStatusBarPriority')) {
        tasksStatusBarDisposable.dispose();
        tasksStatusBarItem = createTasksStatusBarItem();
        tasksStatusBarDisposable = { dispose: () => tasksStatusBarItem.dispose() };
        context.subscriptions.push(tasksStatusBarDisposable);
        tasksProvider.statusBarItem = tasksStatusBarItem;
        tasksProvider.updateStatusBar();
        return;
      }
      if (e.affectsConfiguration('claudeCodeAgents.tasksGitHubRepos') || e.affectsConfiguration('claudeCodeAgents.tasksBitbucketRepos') || e.affectsConfiguration('claudeCodeAgents.bitbucketUrl')) {
        tasksProvider.cache.invalidate();
        tasksProvider.init();
      } else if (e.affectsConfiguration('claudeCodeAgents.tasksStatusBar')) {
        tasksProvider.updateStatusBar();
      }
    })
  );
  context.subscriptions.push(
    vscode.window.registerWebviewViewProvider('claudeCodeTasks.page', tasksProvider, { webviewOptions: { retainContextWhenHidden: true } }),
    vscode.window.registerWebviewViewProvider('claudeCodeTasks.panel', tasksProvider, { webviewOptions: { retainContextWhenHidden: true } })
  );

  context.subscriptions.push(
    vscode.commands.registerCommand('claudeCodeTasks.refresh', () => tasksProvider.handleMessage({ type: 'refresh' })),
    vscode.commands.registerCommand('claudeCodeTasks.openInEditor', () => tasksProvider.openInEditor()),
    vscode.commands.registerCommand('claudeCodeTasks.show', () => tasksProvider.show()),
    vscode.commands.registerCommand('claudeCodeTasks.popOut', () => tasksProvider.popOut()),
    vscode.commands.registerCommand('claudeCodeTasks.recheck', () => tasksProvider.handleMessage({ type: 'doctor' })),
    vscode.commands.registerCommand('claudeCodeTasks.open', (url) => {
      if (url) vscode.env.openExternal(vscode.Uri.parse(url));
    }),
    vscode.commands.registerCommand('claudeCodeTasks.setupToken', () => tasksProvider.handleMessage({ type: 'signIn', source: 'bitbucket' })),
    vscode.commands.registerCommand('claudeCodeTasks.startAgent', (id) => tasksProvider.handleMessage({ type: 'start', id }))
  );

  tasksProvider.init();
  const tasksSeconds = Math.max(15, config().get('tasksRefreshSeconds', 120) || 120);
  const tasksTimer = setInterval(() => {
    if (tasksProvider.visible()) tasksProvider.handleMessage({ type: 'refresh' });
  }, tasksSeconds * 1000);
  context.subscriptions.push({ dispose: () => clearInterval(tasksTimer) });
}

function deactivate() {}

module.exports = { activate, deactivate, UsageViewProvider, TasksViewProvider, branchStatusText };
