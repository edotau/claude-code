'use strict';
const vscode = require('vscode');
const { execFile } = require('node:child_process');
const { AGENTS, treeArgv, dispatchListArgv, dispatchStartArgv, viewArgv, attachShellArgs, attachCommandLine } = require('./lib/argv');
const { buildAgentRootNodes, countRunning, buildDispatchNodes, parseTreeOutput } = require('./lib/tree-model');
const { formatSnapshotText } = require('./lib/format');

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

/** The click-through detail pane: `web view <id> --session S --format json`, dumped as plain text. */
async function viewAgent(binary, node, outputChannel) {
  if (!node || node.kind !== 'subagent') return;
  try {
    const argv = viewArgv(binary, node.agentId, node.sessionId);
    const stdout = await runBinary(binary, argv);
    outputChannel.clear();
    outputChannel.appendLine(formatSnapshotText(JSON.parse(stdout)));
    outputChannel.show(true);
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
}

function deactivate() {}

module.exports = { activate, deactivate, AgentsTreeProvider, toTreeItem };
