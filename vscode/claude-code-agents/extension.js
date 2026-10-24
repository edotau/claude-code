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
const tasksState = require('./lib/tasks-state');
const { renderTasksHtml } = require('./lib/tasks-view');

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
      if (this.panel === panel) this.panel = undefined;
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
    this.status = { github: null, bitbucket: null };
    this.githubRepos = [];
    this.bitbucketRepos = [];
    this.cache = new tasksState.PageCache({ ttlMs: Math.max(15, config().get('tasksRefreshSeconds', 120) || 120) * 1000 });
    this.tokenPromise = null;
    this.loadNonce = 0;
  }

  cfg() {
    const cfg = config();
    const visible = (cfg.get('tasksVisibleSources', ['github', 'bitbucket']) || []).filter((s) => s === 'github' || s === 'bitbucket');
    return {
      binary: cfg.get('binary') || 'claude-code',
      baseUrl: cfg.get('bitbucketUrl') || 'https://bitbucket.biscrum.com',
      jiraBaseUrl: cfg.get('tasksJiraBaseUrl') || '',
      visible: visible.length ? visible : ['github', 'bitbucket'],
      defaultSource: cfg.get('tasksDefaultSource', 'github') || 'github',
      defaultQuery: { github: cfg.get('tasksDefaultQuery.github') || tasksState.DEFAULT_QUERY.github, bitbucket: cfg.get('tasksDefaultQuery.bitbucket') || tasksState.DEFAULT_QUERY.bitbucket },
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
    this.views.add(webviewView);
    webviewView.onDidDispose(() => this.views.delete(webviewView));
    this.wireWebview(webviewView.webview);
    this.render();
  }

  /** Opens (or reveals) the single Tasks editor-tab panel; same model and message handling as the sidebar view. */
  openInEditor() {
    if (this.panel) {
      this.panel.reveal(vscode.ViewColumn.Active);
      return;
    }
    const panel = vscode.window.createWebviewPanel('claudeCodeTasks.editor', 'Tasks', vscode.ViewColumn.Active, { enableScripts: true, retainContextWhenHidden: true });
    this.panel = panel;
    this.wireWebview(panel.webview);
    panel.onDidDispose(() => {
      if (this.panel === panel) this.panel = undefined;
    });
    this.render();
  }

  visible() {
    return [...this.views].some((v) => v.visible) || !!(this.panel && this.panel.visible);
  }

  /** Pops the page out: the editor tab, then VS Code's "move editor into new window" on it (a floating window). */
  async popOut() {
    this.openInEditor();
    await vscode.commands.executeCommand('workbench.action.moveEditorToNewWindow');
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
    if (!this.source || !cfg.visible.includes(this.source)) this.source = cfg.visible.includes(cfg.defaultSource) ? cfg.defaultSource : cfg.visible[0];
    await this.doctor();
    await Promise.all([this.load({ force: true }), this.loadBranch()]);
  }

  async doctor() {
    const cfg = this.cfg();
    const [github, bitbucket] = await Promise.all([
      tasksGithub.authStatus(),
      this.withClient((client) => client.doctor()).catch((err) => ({ ok: false, message: looksLikeMissingToken(err.message) ? 'No token' : err.message })),
    ]);
    this.status = { github, bitbucket: bitbucket.ok ? { ok: true, login: decodeURIComponent(bitbucket.user.username) } : { ok: false, message: bitbucket.message || (bitbucket.reason === 'rejected' ? 'Token rejected' : 'Unreachable') } };
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
    return this.withClient((client) => tasksBitbucket.list({ client, query: parsed, repos: this.bitbucketRepos, page, limit: cfg.pageSize, jiraBaseUrl: cfg.jiraBaseUrl }));
  }

  /** Loads the current page through the cache; `force` bypasses it (manual refresh, timer) and keeps rows on screen. */
  async load(opts) {
    const { force = false, page = this.page } = opts || {};
    const source = this.source;
    const query = this.currentQuery();
    const nonce = ++this.loadNonce;
    const key = { source, repos: source === 'github' ? this.githubRepos.filter((r) => r.selected).map((r) => r.label) : this.bitbucketRepos.map((r) => `${r.project}/${r.repo}`), query, page };
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
    return { kind: 'warn', text: `Bitbucket: ${s.message}`, action: { act: 'signIn', label: 'Set token' } };
  }

  model() {
    const cfg = this.cfg();
    const query = this.currentQuery();
    const parsed = taskQuery.parseTaskQuery(query);
    const presets = tasksState.presetsFor(this.source);
    const result = this.result || { items: [], errors: [], totalPages: 1, failedRepos: 0 };
    const repos = this.source === 'github' ? this.githubRepos.map((r) => ({ label: r.label, selected: r.selected })) : this.bitbucketRepos.map((r) => ({ label: `${r.project}/${r.repo}`, selected: true }));
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
      settings: {
        expanded: this.settingsExpanded,
        defaultSource: cfg.defaultSource,
        github: { status: this.status.github, visible: cfg.visible.includes('github'), repos: this.githubRepos.map((r) => r.label) },
        bitbucket: { status: this.status.bitbucket, visible: cfg.visible.includes('bitbucket'), repos: this.bitbucketRepos.map((r) => `${r.project}/${r.repo}`), baseUrl: cfg.baseUrl },
      },
    };
  }

  render() {
    const model = this.model();
    for (const surface of [...this.views, this.panel]) {
      if (!surface) continue;
      surface.webview.html = renderTasksHtml(model, { nonce: nonce(), cspSource: surface.webview.cspSource });
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
      case 'detail': {
        const item = this.findItem(msg.id);
        if (item && item.url) await vscode.env.openExternal(vscode.Uri.parse(item.url));
        return;
      }
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
        t.sendText(msg.source === 'bitbucket' ? `${cfg.binary} secrets set BITBUCKET_TOKEN` : 'gh auth login');
        t.show();
        this.tokenPromise = null;
        return;
      }
      case 'doctor':
        this.cache.invalidate();
        return this.init();
      case 'toggleSourceVisible': {
        const next = cfg.visible.filter((s) => s !== msg.source);
        if (msg.visible) next.push(msg.source);
        if (!next.length) return this.render(); // the last visible source stays on
        await config().update('tasksVisibleSources', ['github', 'bitbucket'].filter((s) => next.includes(s)), vscode.ConfigurationTarget.Global);
        if (!next.includes(this.source)) return this.handleMessage({ type: 'source', source: next[0] });
        return this.render();
      }
      case 'defaultSource':
        if (msg.source === 'github' || msg.source === 'bitbucket') await config().update('tasksDefaultSource', msg.source, vscode.ConfigurationTarget.Global);
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
