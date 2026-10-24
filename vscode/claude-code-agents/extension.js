'use strict';
const vscode = require('vscode');
const { execFile, spawn } = require('node:child_process');
const {
  AGENTS,
  treeArgv,
  dispatchListArgv,
  dispatchStartArgv,
  viewArgv,
  viewFollowArgv,
  usageArgv,
  profileArgv,
  attachShellArgs,
  attachCommandLine,
} = require('./lib/argv');
const { buildAgentRootNodes, countRunning, buildDispatchNodes, parseTreeOutput } = require('./lib/tree-model');
const { formatSnapshotText } = require('./lib/format');
const { buildUsageModel, statusBarText, statusBarTooltip } = require('./lib/usage-model');
const { renderUsageHtml } = require('./lib/usage-view');

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

/** Converts a plain model node (lib/tree-model.js) into a vscode.TreeItem. */
function toTreeItem(node) {
  const collapsibleState =
    node.children && node.children.length
      ? vscode.TreeItemCollapsibleState.Collapsed
      : vscode.TreeItemCollapsibleState.None;
  const item = new vscode.TreeItem(node.label, collapsibleState);
  item.id = node.id;
  item.description = node.description || undefined;
  item.tooltip = node.tooltip || node.label;
  item.contextValue = node.kind;
  if (node.icon) item.iconPath = new vscode.ThemeIcon(node.icon);
  if (node.kind === 'subagent') {
    item.command = { command: 'claudeCodeAgents.viewAgent', title: 'View Agent', arguments: [node] };
  }
  return item;
}

class AgentsTreeProvider {
  constructor() {
    this._onDidChangeTreeData = new vscode.EventEmitter();
    this.onDidChangeTreeData = this._onDidChangeTreeData.event;
    this.showAll = false;
    this.rootNodes = [];
    this.running = 0;
  }

  refresh() {
    this._onDidChangeTreeData.fire(undefined);
    return this.load();
  }

  async load() {
    const cfg = config();
    const binary = cfg.get('binary') || 'claude-code';
    const days = cfg.get('days', 1);
    const hosts = cfg.get('hosts', []) || [];
    const nodes = [];
    try {
      const argv = treeArgv(binary, { days, all: this.showAll });
      const stdout = await runBinary(binary, argv);
      const parsed = parseTreeOutput(stdout);
      if (parsed.error) throw new Error(parsed.error);
      nodes.push(...buildAgentRootNodes(parsed));
      this.running = countRunning(parsed);
    } catch (err) {
      nodes.push(errorNode('claude-code-agents:tree-error', err.message));
      vscode.window.showErrorMessage(`Claude Code Agents: ${err.message}`);
      this.running = 0;
    }
    if (hosts.length) {
      const hostResults = await Promise.all(
        hosts.map(async (host) => {
          try {
            const argv = dispatchListArgv(binary, host);
            const stdout = await runBinary(binary, argv);
            return { host, sessions: JSON.parse(stdout) };
          } catch (err) {
            return { host, error: err.message };
          }
        })
      );
      nodes.push({
        kind: 'dispatches-root',
        id: 'claude-code-agents:dispatches',
        label: 'Dispatches',
        description: '',
        tooltip: undefined,
        icon: 'server',
        children: buildDispatchNodes(hostResults),
      });
    }
    this.rootNodes = nodes;
    this._onDidChangeTreeData.fire(undefined);
  }

  getTreeItem(node) {
    return toTreeItem(node);
  }

  getChildren(node) {
    if (!node) return this.rootNodes;
    return node.children || [];
  }
}

function errorNode(id, message) {
  return { kind: 'error', id, label: message, description: '', tooltip: message, icon: 'error', children: [] };
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
    const panel = vscode.window.createWebviewPanel('claudeCodeAgents.usageEditor', 'Agent Dashboard', vscode.ViewColumn.Active, {
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
    const binary = config().get('binary') || 'claude-code';
    if (!msg) return;
    if (msg.type === 'refresh') return this.load();
    if (msg.type === 'toggle') {
      this.detailed = !!msg.detailed;
      return this.render();
    }
    if (msg.type === 'details') {
      const terminal = vscode.window.createTerminal({ name: 'Agent Dashboard' });
      terminal.sendText(`${binary} usage`);
      terminal.show();
      return;
    }
    if (msg.type === 'accounts') {
      const terminal = vscode.window.createTerminal({ name: 'Claude Code Profile' });
      terminal.sendText(`${binary} profile`);
      terminal.show();
    }
    // 'provider' clicks are reserved for a future per-provider drill-down; no-op today
  }

  async load() {
    const cfg = config();
    const binary = cfg.get('binary') || 'claude-code';
    const days = cfg.get('usageDays', 1) || 1;
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
    const days = cfg.get('usageDays', 1) || 1;
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

function attachTerminal(host, session) {
  const terminal = vscode.window.createTerminal({
    name: `${host}: ${session}`,
    shellPath: 'ssh',
    shellArgs: attachShellArgs(host, session),
    location: vscode.TerminalLocation.Editor,
  });
  terminal.show();
}

async function runDispatch(context, provider) {
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
    const choice = await vscode.window.showInformationMessage(`Dispatched on ${host}. ${line}`, 'Attach', 'Copy Command');
    if (choice === 'Copy Command') {
      await vscode.env.clipboard.writeText(line);
    } else if (choice === 'Attach') {
      provider.refresh();
    }
  } catch (err) {
    vscode.window.showErrorMessage(`Claude Code Agents dispatch failed: ${err.message}`);
  }
}

// followChild is the one live `web view --follow` child process backing the output channel, killed
// whenever a new agent is viewed or the extension deactivates — never more than one at a time.
let followChild = null;

function stopFollow() {
  if (followChild) {
    followChild.kill();
    followChild = null;
  }
}

/** Streams `web view <id> --session S --follow`'s text/plain lines into outputChannel until the agent finishes. */
function startFollow(binary, agentId, sessionId, outputChannel) {
  const argv = viewFollowArgv(binary, agentId, sessionId);
  const child = spawn(binary, argv.slice(1));
  followChild = child;
  child.stdout.on('data', (chunk) => outputChannel.append(chunk.toString()));
  child.on('error', () => { followChild = null; });
  child.on('close', () => { followChild = null; });
}

/** The click-through detail pane: a `web view --format json` snapshot, then (if running) its live follow stream. */
async function viewAgent(binary, node, outputChannel) {
  if (!node || node.kind !== 'subagent') return;
  stopFollow();
  try {
    const argv = viewArgv(binary, node.agentId, node.sessionId);
    const stdout = await runBinary(binary, argv);
    const snap = JSON.parse(stdout);
    outputChannel.clear();
    outputChannel.appendLine(formatSnapshotText(snap));
    outputChannel.show(true);
    if (snap.status === 'running') startFollow(binary, node.agentId, node.sessionId, outputChannel);
  } catch (err) {
    vscode.window.showErrorMessage(`Claude Code Agents view failed: ${err.message}`);
  }
}

function activate(context) {
  const provider = new AgentsTreeProvider();
  const view = vscode.window.createTreeView('claudeCodeAgents.tree', { treeDataProvider: provider });
  context.subscriptions.push(view);
  const detailChannel = vscode.window.createOutputChannel('Claude Code Agent');
  context.subscriptions.push(detailChannel);

  const updateBadge = () => {
    view.badge = provider.running > 0 ? { value: provider.running, tooltip: `${provider.running} running` } : undefined;
  };
  provider.onDidChangeTreeData(updateBadge);

  context.subscriptions.push(
    vscode.commands.registerCommand('claudeCodeAgents.refresh', () => provider.refresh()),
    vscode.commands.registerCommand('claudeCodeAgents.toggleShowAll', () => {
      provider.showAll = !provider.showAll;
      vscode.window.showInformationMessage(`Claude Code Agents: show all ${provider.showAll ? 'on' : 'off'}`);
      provider.refresh();
    }),
    vscode.commands.registerCommand('claudeCodeAgents.dispatch', () => runDispatch(context, provider)),
    vscode.commands.registerCommand('claudeCodeAgents.viewAgent', (node) =>
      viewAgent(config().get('binary') || 'claude-code', node, detailChannel)
    ),
    vscode.commands.registerCommand('claudeCodeAgents.attach', (node) => {
      if (!node || node.kind !== 'dispatch-session') return;
      attachTerminal(node.host, node.session);
    }),
    vscode.commands.registerCommand('claudeCodeAgents.copyAttachCommand', (node) => {
      if (!node || node.kind !== 'dispatch-session') return;
      vscode.env.clipboard.writeText(attachCommandLine(node.host, node.session));
    })
  );

  provider.load();
  const seconds = config().get('refreshSeconds', 5) || 5;
  const timer = setInterval(() => provider.load(), Math.max(1, seconds) * 1000);
  context.subscriptions.push({ dispose: () => clearInterval(timer) });

  function createUsageStatusBarItem() {
    const cfg = config();
    const alignment =
      cfg.get('usageStatusBarAlignment', 'left') === 'right'
        ? vscode.StatusBarAlignment.Right
        : vscode.StatusBarAlignment.Left;
    const priority = cfg.get('usageStatusBarPriority', 1000000) || 1000000;
    const item = vscode.window.createStatusBarItem(alignment, priority);
    item.name = 'Agent Dashboard';
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

  usageProvider.load();
  const usageSeconds = Math.max(15, config().get('usageRefreshSeconds', 60) || 60);
  const usageTimer = setInterval(() => usageProvider.load(), usageSeconds * 1000);
  context.subscriptions.push({ dispose: () => clearInterval(usageTimer) });
  const usageTickTimer = setInterval(() => usageProvider.tick(), 60 * 1000);
  context.subscriptions.push({ dispose: () => clearInterval(usageTickTimer) });
}

function deactivate() {
  stopFollow();
}

module.exports = { activate, deactivate, AgentsTreeProvider, toTreeItem, UsageViewProvider };
