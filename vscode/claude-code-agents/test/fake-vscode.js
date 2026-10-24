'use strict';
// A minimal `vscode` module for node:test: installs itself in the require cache so extension.js loads.
// Records commands and panels; exposes `make()` to build fresh providers with fake webview views.
const Module = require('node:module');

function emitter() {
  const listeners = [];
  const event = (fn) => {
    listeners.push(fn);
    return { dispose: () => listeners.splice(listeners.indexOf(fn), 1) };
  };
  event.fire = (arg) => listeners.slice().forEach((fn) => fn(arg));
  return event;
}

function fakeWebview() {
  return { html: '', options: {}, cspSource: 'vscode-webview:', onDidReceiveMessage: emitter(), postMessage: () => Promise.resolve(true) };
}

const state = { commands: [], panels: [], messages: [], config: {} };

/** A fake integrated terminal: records sendText calls, tracked on `vscode.window.terminals`. */
function fakeTerminal(options) {
  const term = {
    name: options && options.name,
    cwd: options && options.cwd,
    exitStatus: undefined,
    sent: [],
    sendText(text) {
      term.sent.push(text);
    },
    show() {},
    dispose() {
      term.exitStatus = { code: 0 };
    },
  };
  vscode.window.terminals.push(term);
  return term;
}

function fakePanel(viewType, title) {
  const panel = {
    viewType,
    title,
    active: true,
    visible: true,
    webview: fakeWebview(),
    onDidDispose: emitter(),
    onDidChangeViewState: emitter(),
    reveal() {
      state.commands.push(['panel.reveal', viewType]);
    },
    dispose() {
      panel.visible = false;
      panel.onDidDispose.fire();
    },
  };
  state.panels.push(panel);
  return panel;
}

const vscode = {
  ViewColumn: { Active: -1 },
  ConfigurationTarget: { Global: 1 },
  Uri: { parse: (s) => ({ toString: () => s }), file: (p) => ({ fsPath: p, toString: () => p }) },
  MarkdownString: class {
    constructor(v) {
      this.value = v;
    }
  },
  StatusBarAlignment: { Left: 1, Right: 2 },
  window: {
    createWebviewPanel: (viewType, title) => fakePanel(viewType, title),
    createOutputChannel: () => ({ appendLine() {}, show() {}, dispose() {} }),
    createStatusBarItem: () => ({ show() {}, hide() {}, dispose() {}, text: '', tooltip: '' }),
    showErrorMessage: (m) => state.messages.push(['error', m]),
    showWarningMessage: (m) => state.messages.push(['warn', m]),
    showInformationMessage: (m, ...items) => {
      state.messages.push(['info', m, ...items]);
      return Promise.resolve(undefined);
    },
    setStatusBarMessage: () => {},
    registerWebviewViewProvider: () => ({ dispose() {} }),
    createQuickPick: () => ({ show() {}, dispose() {}, onDidAccept: emitter(), onDidHide: emitter(), onDidChangeValue: emitter(), items: [] }),
    showQuickPick: async () => undefined,
    showInputBox: async () => undefined,
    terminals: [],
    createTerminal: (options) => fakeTerminal(options),
  },
  commands: {
    executeCommand: async (...args) => {
      state.commands.push(args);
      return undefined;
    },
    registerCommand: () => ({ dispose() {} }),
  },
  workspace: {
    workspaceFolders: [],
    getConfiguration: () => ({
      get: (key, def) => (key in state.config ? state.config[key] : def),
      update: async (key, value) => {
        state.config[key] = value;
      },
    }),
  },
  env: { openExternal: async (uri) => state.commands.push(['openExternal', String(uri)]), clipboard: { writeText: async () => {} } },
  extensions: { getExtension: () => undefined },
};

const realResolve = Module._resolveFilename;
Module._resolveFilename = function (request, ...rest) {
  if (request === 'vscode') return 'vscode';
  return realResolve.call(this, request, ...rest);
};
require.cache.vscode = { id: 'vscode', filename: 'vscode', loaded: true, exports: vscode };

/** A webview view the provider can resolve: `visible` toggles via `show()`/`hide()` and fires the visibility event. */
function fakeView(viewType, visible = true) {
  const view = { viewType, visible, webview: fakeWebview(), onDidDispose: emitter(), onDidChangeVisibility: emitter() };
  view.show = () => {
    view.visible = true;
    view.onDidChangeVisibility.fire();
  };
  view.hide = () => {
    view.visible = false;
    view.onDidChangeVisibility.fire();
  };
  return view;
}

function fakeContext() {
  const store = new Map();
  const globalStorageUri = { fsPath: require('node:fs').mkdtempSync(require('node:path').join(require('node:os').tmpdir(), 'claude-code-agents-test-')) };
  return {
    subscriptions: [],
    globalStorageUri,
    workspaceState: { get: (k) => store.get(k), update: async (k, v) => store.set(k, v) },
    globalState: { get: (k) => store.get(k), update: async (k, v) => store.set(k, v) },
  };
}

function reset() {
  state.commands.length = 0;
  state.panels.length = 0;
  state.messages.length = 0;
  vscode.window.terminals.length = 0;
  vscode.workspace.workspaceFolders = [];
  for (const k of Object.keys(state.config)) delete state.config[k];
}

module.exports = { vscode, state, fakeView, fakeContext, reset };
