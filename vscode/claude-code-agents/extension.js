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
const tasksModel = require('./lib/tasks-model');
const bitbucketClient = require('./lib/bitbucket-client');

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

/** tasks-model node -> vscode.TreeItem; a `url` opens via claudeCodeTasks.open, `action: 'setupToken'` runs the setup command. */
function toTasksTreeItem(node) {
  const collapsible = node.children && node.children.length
    ? vscode.TreeItemCollapsibleState.Collapsed
    : vscode.TreeItemCollapsibleState.None;
  const item = new vscode.TreeItem(node.label, collapsible);
  if (node.description) item.description = node.description;
  if (node.tooltip) {
    const tooltip = new vscode.MarkdownString(node.tooltip);
    tooltip.isTrusted = false;
    item.tooltip = tooltip;
  }
  if (node.icon) item.iconPath = new vscode.ThemeIcon(node.icon);
  if (node.action === 'setupToken') {
    item.command = { command: 'claudeCodeTasks.setupToken', title: 'Set Bitbucket token' };
  } else if (node.url) {
    item.command = { command: 'claudeCodeTasks.open', title: 'Open', arguments: [node.url] };
    item.contextValue = 'task';
  }
  return item;
}

/** Backs one Tasks tree view from a node list set by TasksController.load(). */
class TasksTreeProvider {
  constructor() {
    this._onDidChangeTreeData = new vscode.EventEmitter();
    this.onDidChangeTreeData = this._onDidChangeTreeData.event;
    this.nodes = [];
  }

  setNodes(nodes) {
    this.nodes = nodes;
    this._onDidChangeTreeData.fire();
  }

  getTreeItem(element) {
    return toTasksTreeItem(element);
  }

  getChildren(element) {
    return element ? element.children || [] : this.nodes;
  }
}

/** Loads the three Tasks trees + the branch status bar segment via the bundled Bitbucket Data Center client. */
class TasksController {
  constructor(statusBarItem) {
    this.statusBarItem = statusBarItem;
    this.branchProvider = new TasksTreeProvider();
    this.mineProvider = new TasksTreeProvider();
    this.reviewingProvider = new TasksTreeProvider();
    this.branchPr = null;
    this.tokenPromise = null;
  }

  errorNode(message) {
    if (looksLikeMissingToken(message)) {
      return { label: message, description: '', tooltip: '', url: null, icon: 'warning', children: [], action: 'setupToken' };
    }
    return { label: message, description: '', tooltip: '', url: null, icon: 'error', children: [] };
  }

  notDataCenterNode(host) {
    return { label: `Not a Bitbucket Data Center repo: ${host}`, description: '', tooltip: '', url: null, icon: 'info', children: [] };
  }

  /** Resolves BITBUCKET_TOKEN once per extension lifetime; `forceRefresh` re-resolves after a 401/403. */
  getToken(binary, forceRefresh) {
    if (forceRefresh) this.tokenPromise = null;
    if (!this.tokenPromise) {
      this.tokenPromise = bitbucketClient.resolveToken({ binary }).catch((err) => {
        this.tokenPromise = null;
        throw err;
      });
    }
    return this.tokenPromise;
  }

  /** Runs `op(client)` and retries once with a freshly resolved token on a 401/403. */
  async withClient(binary, baseUrl, op) {
    const token = await this.getToken(binary);
    try {
      return await op(bitbucketClient.createClient({ baseUrl, token }));
    } catch (err) {
      if (err.status !== 401 && err.status !== 403) throw err;
      const freshToken = await this.getToken(binary, true);
      return op(bitbucketClient.createClient({ baseUrl, token: freshToken }));
    }
  }

  async load() {
    const cfg = config();
    const binary = cfg.get('binary') || 'claude-code';
    const baseUrl = cfg.get('bitbucketUrl') || 'https://bitbucket.biscrum.com';
    const jiraBaseUrl = cfg.get('tasksJiraBaseUrl') || '';
    const dir = workspaceDir();
    const [branch, remoteUrl] = await Promise.all([currentBranch(dir), originRemoteUrl(dir)]);
    await Promise.all([
      this.loadBranch(binary, baseUrl, remoteUrl, branch, jiraBaseUrl),
      this.loadList(binary, baseUrl, 'AUTHOR', this.mineProvider),
      this.loadList(binary, baseUrl, 'REVIEWER', this.reviewingProvider),
    ]);
  }

  async loadBranch(binary, baseUrl, remoteUrl, branch, jiraBaseUrl) {
    let host;
    try {
      host = new URL(baseUrl).hostname.toLowerCase();
    } catch {
      host = '';
    }
    const ref = remoteUrl ? bitbucketClient.parseRepoRef(remoteUrl, host) : null;
    if (!ref) {
      this.branchPr = null;
      this.branchProvider.setNodes([this.notDataCenterNode(remoteUrl ? remoteHostFor(remoteUrl) : 'no git remote')]);
      if (this.statusBarItem) this.statusBarItem.hide();
      return;
    }
    try {
      const pr = await this.withClient(binary, baseUrl, (client) => client.pullRequestForBranch(ref, branch));
      const jiraKey = tasksModel.detectJiraIssueKey({ branch, title: pr && pr.title });
      this.branchPr = pr;
      this.branchProvider.setNodes(tasksModel.buildBranchNodes(pr ? [pr] : [], { branch, jiraKey, jiraBaseUrl }));
      this.updateStatusBar({ pr, jiraKey });
    } catch (err) {
      this.branchPr = null;
      this.branchProvider.setNodes([this.errorNode(err.message)]);
      if (this.statusBarItem) this.statusBarItem.hide();
    }
  }

  async loadList(binary, baseUrl, role, provider) {
    try {
      const rows = await this.withClient(binary, baseUrl, async (client) => {
        const raw = await client.dashboard(role);
        return client.listWithBuilds(raw);
      });
      provider.setNodes(tasksModel.buildPrListNodes(rows));
    } catch (err) {
      provider.setNodes([this.errorNode(err.message)]);
    }
  }

  updateStatusBar(branchModel) {
    if (!this.statusBarItem) return;
    if (!config().get('tasksStatusBar', true)) {
      this.statusBarItem.hide();
      return;
    }
    const text = tasksModel.statusBarText(branchModel);
    if (!text) {
      this.statusBarItem.hide();
      return;
    }
    this.statusBarItem.text = text;
    this.statusBarItem.show();
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
    item.command = 'claudeCodeTasks.branch.focus';
    return item;
  }

  let tasksStatusBarItem = createTasksStatusBarItem();
  let tasksStatusBarDisposable = { dispose: () => tasksStatusBarItem.dispose() };
  context.subscriptions.push(tasksStatusBarDisposable);

  const tasksController = new TasksController(tasksStatusBarItem);

  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (
        !e.affectsConfiguration('claudeCodeAgents.usageStatusBarAlignment') &&
        !e.affectsConfiguration('claudeCodeAgents.usageStatusBarPriority')
      ) {
        return;
      }
      tasksStatusBarDisposable.dispose();
      tasksStatusBarItem = createTasksStatusBarItem();
      tasksStatusBarDisposable = { dispose: () => tasksStatusBarItem.dispose() };
      context.subscriptions.push(tasksStatusBarDisposable);
      tasksController.statusBarItem = tasksStatusBarItem;
      tasksController.updateStatusBar({ pr: tasksController.branchPr, jiraKey: null });
    })
  );

  const branchView = vscode.window.createTreeView('claudeCodeTasks.branch', { treeDataProvider: tasksController.branchProvider });
  const mineView = vscode.window.createTreeView('claudeCodeTasks.mine', { treeDataProvider: tasksController.mineProvider });
  const reviewingView = vscode.window.createTreeView('claudeCodeTasks.reviewing', { treeDataProvider: tasksController.reviewingProvider });
  context.subscriptions.push(branchView, mineView, reviewingView);

  context.subscriptions.push(
    vscode.commands.registerCommand('claudeCodeTasks.refresh', () => tasksController.load()),
    vscode.commands.registerCommand('claudeCodeTasks.open', (url) => {
      if (url) vscode.env.openExternal(vscode.Uri.parse(url));
    }),
    vscode.commands.registerCommand('claudeCodeTasks.setupToken', () => {
      const binary = config().get('binary') || 'claude-code';
      const terminal = dashboardTerminal();
      terminal.sendText(`${binary} secrets set BITBUCKET_TOKEN`);
      terminal.show();
    }),
    vscode.commands.registerCommand('claudeCodeTasks.startAgent', async (node) => {
      if (!node || !node.url) return;
      await vscode.env.clipboard.writeText(`Work on ${node.label} (${node.url})`);
      const binary = config().get('binary') || 'claude-code';
      const terminal = dashboardTerminal();
      terminal.sendText(`${binary} claude`);
      terminal.show();
      vscode.window.showInformationMessage('Task prompt copied — paste it into Claude.');
    })
  );

  tasksController.load();
  const tasksSeconds = Math.max(15, config().get('tasksRefreshSeconds', 120) || 120);
  const tasksTimer = setInterval(() => {
    if (branchView.visible || mineView.visible || reviewingView.visible) tasksController.load();
  }, tasksSeconds * 1000);
  context.subscriptions.push({ dispose: () => clearInterval(tasksTimer) });
}

function deactivate() {}

module.exports = { activate, deactivate, UsageViewProvider, TasksController, TasksTreeProvider };
