'use strict';
// JSON -> plain tree-node model. Pure (no vscode import) so node:test exercises it directly;
// extension.js adapts these plain nodes into vscode.TreeItem.
const path = require('node:path');
const { formatAge, formatTokens, formatActivity, formatLeg } = require('./format');

function iconForStatus(status) {
  if (status === 'running' || status === 'active') return 'sync~spin';
  if (status === 'finished') return 'check';
  if (status === 'stale' || status === 'idle') return 'warning';
  return null;
}

// Rebuilds the nesting the API flattens via `depth`: each item becomes the child of the nearest
// preceding item with a strictly smaller depth.
function nestByDepth(items) {
  const root = [];
  const stack = [];
  for (const item of items || []) {
    const depth = item.depth || 1;
    const node = Object.assign({}, item, { depth, children: [] });
    while (stack.length && stack[stack.length - 1].depth >= depth) stack.pop();
    if (stack.length) stack[stack.length - 1].node.children.push(node);
    else root.push(node);
    stack.push({ depth, node });
  }
  return root;
}

/** A vendor-dispatch leg as an indented, non-expandable child row under its relay subagent. */
function legNode(agentId, leg) {
  return {
    kind: 'leg',
    id: `${agentId}:leg:${leg.id}`,
    label: formatLeg(leg),
    description: '',
    tooltip: leg.fallbackReason || undefined,
    icon: leg.fallback ? 'warning' : 'link',
    children: [],
  };
}

// sessionId threads through to each subagent node so its view-detail command knows which
// session to search (`claude-code web view <id> --session <sessionId>`).
function subagentNode(item, now, sessionId) {
  const children = (item.children || []).map((c) => subagentNode(c, now, sessionId));
  const legChildren = (item.legs || []).map((l) => legNode(item.id, l));
  const activity = formatActivity(item.activity);
  return {
    kind: 'subagent',
    id: item.id,
    agentId: item.id,
    sessionId,
    label: `${item.type}: ${item.description}`,
    description: `${item.status} · ${formatTokens(item.tokens)} · ${formatAge(item.updated, now)}${activity ? ' · ' + activity : ''}`,
    tooltip: item.lastTool ? `last tool: ${item.lastTool}` : undefined,
    icon: iconForStatus(item.status),
    children: [...legChildren, ...children],
  };
}

function phaseNode(workflowId, phase, now, sessionId) {
  const nested = nestByDepth(phase.agents || []).map((a) => subagentNode(a, now, sessionId));
  return {
    kind: 'phase',
    id: `${workflowId}:${phase.name}`,
    label: phase.name,
    description: '',
    tooltip: undefined,
    icon: null,
    children: nested,
  };
}

function workflowNode(wf, now, sessionId) {
  return {
    kind: 'workflow',
    id: wf.id,
    label: wf.name,
    description: `${wf.status} · ${formatAge(wf.updated, now)}`,
    tooltip: undefined,
    icon: iconForStatus(wf.status),
    children: (wf.phases || []).map((p) => phaseNode(wf.id, p, now, sessionId)),
  };
}

function sessionNode(session, now) {
  const subChildren = nestByDepth(session.subagents || []).map((s) => subagentNode(s, now, session.id));
  const wfChildren = (session.workflows || []).map((w) => workflowNode(w, now, session.id));
  return {
    kind: 'session',
    id: session.id,
    label: session.title || session.id,
    description: `${session.status} · ${formatAge(session.updated, now)}`,
    tooltip: session.cwd,
    icon: iconForStatus(session.status),
    children: [...subChildren, ...wfChildren],
  };
}

/** Groups sessions by repo (basename of cwd) and builds the full display-node tree. */
function buildAgentTree(data, opts) {
  const now = (opts && opts.now) || Date.now();
  const sessions = (data && data.sessions) || [];
  const byRepo = new Map();
  for (const session of sessions) {
    const cwd = session.cwd || '';
    const repoName = (cwd && path.basename(cwd)) || session.project || 'unknown';
    if (!byRepo.has(repoName)) byRepo.set(repoName, { cwd, sessions: [] });
    byRepo.get(repoName).sessions.push(session);
  }
  const repos = [];
  for (const [repoName, info] of byRepo) {
    repos.push({
      kind: 'repo',
      id: repoName,
      label: repoName,
      description: '',
      tooltip: info.cwd,
      icon: null,
      children: info.sessions.map((s) => sessionNode(s, now)),
    });
  }
  return repos;
}

/** Counts active sessions + running subagents/workflow agents, for the view's "N running" badge. */
function countRunning(data) {
  let n = 0;
  const walk = (list) => {
    for (const item of list || []) {
      if (item.status === 'running') n++;
      walk(item.children);
    }
  };
  for (const session of (data && data.sessions) || []) {
    if (session.status === 'active') n++;
    walk(session.subagents);
    for (const wf of session.workflows || []) {
      for (const phase of wf.phases || []) walk(phase.agents);
    }
  }
  return n;
}

/** A single placeholder node shown when there is nothing else to render. */
function emptyStateNode() {
  return {
    kind: 'empty',
    id: 'claude-code-agents:empty',
    label: 'No active agents — toggle Show all',
    description: '',
    tooltip: undefined,
    icon: 'info',
    children: [],
  };
}

/** buildAgentTree's repo nodes, or a single empty-state placeholder when there are none. */
function buildAgentRootNodes(data, opts) {
  const repos = buildAgentTree(data, opts);
  return repos.length ? repos : [emptyStateNode()];
}

/** hostResults: [{host, sessions?: dispatch.Session[], error?: string}] -> display nodes. */
function buildDispatchNodes(hostResults) {
  return (hostResults || []).map((hr) => {
    if (hr.error) {
      return {
        kind: 'host',
        id: `dispatch:${hr.host}`,
        label: hr.host,
        description: 'error',
        tooltip: hr.error,
        icon: 'warning',
        children: [
          { kind: 'error', id: `dispatch:${hr.host}:error`, label: hr.error, description: '', tooltip: hr.error, icon: 'warning', children: [] },
        ],
      };
    }
    const sessions = hr.sessions || [];
    return {
      kind: 'host',
      id: `dispatch:${hr.host}`,
      label: hr.host,
      description: `${sessions.length} session(s)`,
      tooltip: undefined,
      icon: null,
      children: sessions.map((s) => ({
        kind: 'dispatch-session',
        id: `dispatch:${hr.host}:${s.Name}`,
        host: hr.host,
        session: s.Name,
        label: s.Name,
        description: `${s.Attached ? 'attached' : 'detached'}${s.CurrentCmd ? ' · ' + s.CurrentCmd : ''}`,
        tooltip: s.Created ? `created ${s.Created}` : undefined,
        icon: s.Attached ? 'radio-tower' : 'circle-outline',
        children: [],
      })),
    };
  });
}

/** Parses `agents tree --format json` stdout into {sessions} or {error}; never throws. */
function parseTreeOutput(stdout) {
  let data;
  try {
    data = JSON.parse(stdout);
  } catch (err) {
    return { error: `malformed agent tree JSON: ${err.message}` };
  }
  if (!data || typeof data !== 'object' || !Array.isArray(data.sessions)) {
    return { error: 'malformed agent tree output: missing "sessions" array' };
  }
  return { sessions: data.sessions };
}

module.exports = {
  buildAgentTree,
  buildAgentRootNodes,
  emptyStateNode,
  countRunning,
  buildDispatchNodes,
  nestByDepth,
  iconForStatus,
  parseTreeOutput,
};
