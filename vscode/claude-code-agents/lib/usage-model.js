'use strict';
// Pure usage-popover model builder — no vscode dependency, so node:test can exercise it directly.
const { formatCount } = require('./format');

const PROVIDER_META = {
  anthropic: { name: 'Claude', icon: 'sparkle' },
  google: { name: 'Antigravity', icon: 'rocket' },
  github: { name: 'GitHub Copilot', icon: 'github' },
  openai: { name: 'ChatGPT', icon: 'comment-discussion' },
};

/** Agent CLI id → display name for the Agents section (`usage --format json`'s `agents[]`). */
const AGENT_META = {
  claude: 'Claude',
  codex: 'Codex',
  opencode: 'OpenCode',
  copilot: 'Copilot CLI',
};

/** "5h" | "wk" | "mo", falling back to the raw window string for anything unrecognized. */
function windowLabelFor(window) {
  if (window === '5h') return '5h';
  if (window === 'weekly') return 'wk';
  if (window === 'monthly') return 'mo';
  return window || '';
}

/** ms → "47m" | "3h 54m" | "6d 7h" | "now", floored to whole units. */
function formatResetDuration(ms) {
  const totalMinutes = Math.floor((ms || 0) / 60000);
  if (totalMinutes <= 0) return 'now';
  if (totalMinutes < 60) return `${totalMinutes}m`;
  const totalHours = Math.floor(totalMinutes / 60);
  const remMinutes = totalMinutes % 60;
  if (totalHours < 24) return `${totalHours}h ${remMinutes}m`;
  const totalDays = Math.floor(totalHours / 24);
  const remHours = totalHours % 24;
  return `${totalDays}d ${remHours}h`;
}

/** "Resets in 3h 54m" | "Resets now". */
function formatResetCountdown(ms) {
  const duration = formatResetDuration(ms);
  return duration === 'now' ? 'Resets now' : `Resets in ${duration}`;
}

function toMs(v) {
  if (!v) return null;
  const t = Date.parse(v);
  return Number.isNaN(t) ? null : t;
}

function buildProvider(id, rows) {
  const meta = PROVIDER_META[id] || { name: id, icon: 'circle-outline' };
  let plan = null;
  let note = null;
  const windows = [];
  for (const row of rows) {
    if (plan == null && row.plan) plan = row.plan;
    if (!row.window) {
      if (row.note) note = row.note;
      continue;
    }
    const label =
      row.group === 'Fable'
        ? 'Fable'
        : row.group
        ? `${row.group} · ${windowLabelFor(row.window)}`
        : windowLabelFor(row.window);
    windows.push({
      key: row.group ? `${row.window}:${row.group}` : row.window,
      label,
      group: row.group || null,
      usedPct: typeof row.used_percent === 'number' ? row.used_percent : 0,
      leftPct: typeof row.left_percent === 'number' ? row.left_percent : 0,
      resetsAtMs: toMs(row.resets_at) != null ? toMs(row.resets_at) : toMs(row.resets),
      state: row.state || 'ok',
    });
  }
  let resetsAtMs = null;
  for (const w of windows) {
    if (w.resetsAtMs != null && (resetsAtMs == null || w.resetsAtMs < resetsAtMs)) resetsAtMs = w.resetsAtMs;
  }
  const resetLabel = resetsAtMs != null ? formatResetCountdown(resetsAtMs - Date.now()) : null;
  return { id, name: meta.name, icon: meta.icon, plan, resetsAtMs, resetLabel, note, windows };
}

/** One `agents[]` model entry → {provider, model, turns, tokens, costUsd}. */
function buildAgentModelRow(row) {
  return {
    provider: row.provider || null,
    model: row.model || null,
    turns: row.turns || 0,
    tokens: row.tokens || 0,
    costUsd: typeof row.cost_usd === 'number' ? row.cost_usd : null,
  };
}

/** One `agents[]` entry (older `usage` output lacks the key entirely; callers must treat it as absent). */
function buildAgent(row) {
  const id = row.agent;
  const models = (row.models || []).map(buildAgentModelRow);
  const top = models[0];
  return {
    id,
    name: AGENT_META[id] || id,
    sessions: row.sessions || 0,
    turns: row.turns || 0,
    tokens: row.tokens || 0,
    costUsd: typeof row.cost_usd === 'number' ? row.cost_usd : null,
    topModel: top ? `${top.provider}/${top.model}` : null,
    models,
    note: row.note || null,
  };
}

/** Flat `usage --format json` + `profile --format json` → the grouped model the status bar/webview render. */
function buildUsageModel(usageJson, profileJson, nowMs) {
  void profileJson; // reserved for future model-slot-name lookups; not needed by the current fields
  void nowMs; // reset labels are computed at render time (see statusBarText/Tooltip) so they tick live
  const billing = (usageJson && usageJson.billing) || {};
  const mode = billing.mode === 'api' ? 'api' : 'subscription';
  const billingProvider = billing.provider || null;
  const rows = (usageJson && usageJson.providers) || [];
  const order = [];
  const byId = new Map();
  for (const row of rows) {
    const id = row.provider;
    if (!id) continue;
    if (!byId.has(id)) {
      order.push(id);
      byId.set(id, []);
    }
    byId.get(id).push(row);
  }
  const ids = order.includes('anthropic') ? ['anthropic', ...order.filter((id) => id !== 'anthropic')] : order;
  const providers = ids.map((id) => buildProvider(id, byId.get(id)));
  const api = ((usageJson && usageJson.api) || []).map((r) => ({
    provider: r.provider,
    model: r.model,
    today: r.today || 0,
    last30d: r.last_30d || 0,
    allTime: r.all_time || 0,
  }));
  const agents = Array.isArray(usageJson && usageJson.agents) ? usageJson.agents.map(buildAgent) : [];
  return { mode, billingProvider, providers, api, agents };
}

/** 10-cell ascii bar, filled proportionally to pct (0-100). */
function bar(pct, width) {
  const w = width || 10;
  const filled = Math.max(0, Math.min(w, Math.round(((pct || 0) / 100) * w)));
  return '▰'.repeat(filled) + '▱'.repeat(w - filled);
}

/** ` · codex 25k · opencode 321k` — agents with tokens>0, excluding claude (the quota chips cover it); '' when none. */
function agentsStatusText(model) {
  const agents = (model.agents || []).filter((a) => a.id !== 'claude' && a.tokens > 0);
  if (!agents.length) return '';
  return ' · ' + agents.map((a) => `${a.id} ${formatCount(a.tokens)}`).join(' · ');
}

/** Status bar segment text; `show` picks used_percent vs left_percent (default 'used'); `opts.agents` appends per-agent totals. */
function statusBarText(model, show, opts) {
  const pctField = show === 'left' ? 'leftPct' : 'usedPct';
  const suffix = (opts && opts.agents) ? agentsStatusText(model) : '';
  if (model.mode === 'api') {
    const todayTotal = (model.api || []).reduce((sum, r) => sum + (r.today || 0), 0);
    return `$(key) ${model.billingProvider || 'api'} ${formatCount(todayTotal)} tok today${suffix}`;
  }
  const anthropic = model.providers.find((p) => p.id === 'anthropic');
  if (!anthropic || anthropic.windows.length === 0) {
    const note = (anthropic && anthropic.note) || 'no data';
    return `$(sparkle) Claude: ${note}${suffix}`;
  }
  const nowMs = Date.now();
  const segments = anthropic.windows.map((w) => {
    const pct = Math.round(w[pctField]);
    if (w.label === 'Fable') return `${pct}% Fable`;
    const duration = w.resetsAtMs != null ? formatResetDuration(w.resetsAtMs - nowMs) : null;
    return duration ? `${pct}% ${duration}` : `${pct}%`;
  });
  return `$(sparkle) ${segments.join(' · ')}${suffix}`;
}

/** "3 sessions" / "1 session". */
function sessionsLabel(n) {
  return `${n} session${n === 1 ? '' : 's'}`;
}

/** One tooltip line per agent: "**Codex** openai/gpt-6-astra · 25.3k tokens · 3 sessions · $0.33"; a note-only agent shows just the note. */
function agentTooltipLine(a) {
  if (!a.tokens && !a.sessions) return `**${a.name}** ${a.note || 'no data'}`;
  const parts = [`${formatCount(a.tokens)} tokens`, sessionsLabel(a.sessions)];
  if (a.costUsd != null) parts.push(`$${a.costUsd.toFixed(2)}`);
  return a.topModel ? `**${a.name}** ${a.topModel} · ${parts.join(' · ')}` : `**${a.name}** ${parts.join(' · ')}`;
}

/** Markdown tooltip: one line per window, "<Provider> <label> <bar> <pct>% <used|left> · resets in …". */
function statusBarTooltip(model, show) {
  const pctField = show === 'left' ? 'leftPct' : 'usedPct';
  const nowMs = Date.now();
  const lines = [];
  if (model.mode === 'api') {
    lines.push(`**${model.billingProvider || 'API'} usage**`);
    for (const r of model.api || []) {
      lines.push(`${r.provider} ${r.model}: ${formatCount(r.today)} today`);
    }
    for (const a of model.agents || []) lines.push(agentTooltipLine(a));
    return lines.join('  \n');
  }
  for (const p of model.providers) {
    if (!p.windows.length) {
      lines.push(`**${p.name}**: ${p.note || 'no data'}`);
      continue;
    }
    for (const w of p.windows) {
      const pct = Math.round(w[pctField]);
      const resetMsg = w.resetsAtMs != null ? formatResetCountdown(w.resetsAtMs - nowMs).toLowerCase() : 'resets unknown';
      lines.push(`${p.name} ${w.label} ${bar(pct)} ${pct}% ${show === 'left' ? 'left' : 'used'} · ${resetMsg}`);
    }
  }
  for (const a of model.agents || []) lines.push(agentTooltipLine(a));
  return lines.join('  \n');
}

module.exports = {
  sessionsLabel,
  windowLabelFor,
  formatResetDuration,
  formatResetCountdown,
  buildUsageModel,
  bar,
  agentsStatusText,
  statusBarText,
  statusBarTooltip,
};
