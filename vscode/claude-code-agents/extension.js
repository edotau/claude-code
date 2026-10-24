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

// dashboardTerminal reuses one "Agents Dashboard" terminal for every command the dashboard launches.
function dashboardTerminal() {
  const name = 'Agents Dashboard';
  const existing = vscode.window.terminals.find((t) => t.name === name && t.exitStatus === undefined);
  return existing || vscode.window.createTerminal({ name });
}

// detailsChannel shows `claude-code usage` text output read-only, without spawning a shell.
let detailsChannel = null;
function showUsageDetails(binary, days) {
  if (!detailsChannel) detailsChannel = vscode.window.createOutputChannel('Agents Dashboard');
  execFile(binary, ['usage', '--days', String(days)], { maxBuffer: 10 * 1024 * 1024 }, (err, stdout, stderr) => {
    detailsChannel.clear();
    detailsChannel.appendLine(err ? `${binary} usage failed: ${stderr || err.message}` : stdout);
    detailsChannel.show(true);
  });
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
    const binary = config().get('binary') || 'claude-code';
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
    if (msg.type === 'details') {
      showUsageDetails(binary, config().get('usageDays', 30) || 30);
      return;
    }
    if (msg.type === 'accounts') {
      const terminal = dashboardTerminal();
      terminal.sendText(`${binary} profile`);
      terminal.show();
      return;
    }
    // 'provider' clicks are reserved for a future per-provider drill-down; no-op today
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

function activate(context) {
  context.subscriptions.push(vscode.commands.registerCommand('claudeCodeAgents.dispatch', () => runDispatch()));

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
}

function deactivate() {}

module.exports = { activate, deactivate, UsageViewProvider };
