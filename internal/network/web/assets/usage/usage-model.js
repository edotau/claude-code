'use strict';
// Pure usage-popover model builder — no vscode dependency, so node:test can exercise it directly.
const { formatCount } = require('./format');
const { priceFor } = require('./pricing');

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

/** One provider card's rows, scoped to a single (provider, account) pair — the CLI's PROVIDER QUOTAS keys on
 * both, so a provider with two accounts (e.g. two `github` logins) renders as two cards, never merged. */
function buildProvider(id, account, rows) {
  const meta = PROVIDER_META[id] || { name: id, icon: 'circle-outline' };
  let plan = null;
  let note = null;
  let tokenExpiresAtMs = null;
  const windows = [];
  for (const row of rows) {
    if (tokenExpiresAtMs == null) tokenExpiresAtMs = toMs(row.token_expires_at);
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
      spend: row.provider === 'databricks' && row.unit === 'USD',
      unit: row.unit || null,
      window: row.window,
      usedCredits: typeof row.used_credits === 'number' ? row.used_credits : null,
      limitCredits: typeof row.limit_credits === 'number' ? row.limit_credits : null,
      resetsAtMs: toMs(row.resets_at) != null ? toMs(row.resets_at) : toMs(row.resets),
      state: row.state || 'ok',
    });
  }
  let resetsAtMs = null;
  for (const w of windows) {
    if (w.resetsAtMs != null && (resetsAtMs == null || w.resetsAtMs < resetsAtMs)) resetsAtMs = w.resetsAtMs;
  }
  const resetLabel = resetsAtMs != null ? formatResetCountdown(resetsAtMs - Date.now()) : null;
  return { id, account: account || null, name: meta.name, icon: meta.icon, plan, resetsAtMs, resetLabel, note, tokenExpiresAtMs, windows };
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

/** `row.mix` (snake_case, optional) -> camelCase {input,output,cacheRead,cacheWrite}, or null when absent. */
function buildAgentMix(mix) {
  if (!mix) return null;
  return {
    input: mix.input || 0,
    output: mix.output || 0,
    cacheRead: mix.cache_read || 0,
    cacheWrite: mix.cache_write || 0,
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
    mix: buildAgentMix(row.mix),
    daily: Array.isArray(row.daily)
      ? row.daily.filter((d) => d && d.date).map((d) => ({ date: d.date, tokens: d.tokens || 0, turns: d.turns || 0 }))
      : [],
    activeDays: row.active_days || 0,
  };
}

/** USD estimate for one agent's token mix: its own `costUsd` (e.g. OpenCode), else priced from `topModel`'s
 * Anthropic tier applied to `mix`. `priced: false` means tokens exist but no rate could be found. */
function estimateAgentCost(agent) {
  if (agent.costUsd != null) return { cost: agent.costUsd, priced: true };
  if (!agent.tokens) return { cost: 0, priced: true };
  const top = agent.models && agent.models[0];
  const price = top ? priceFor(top.provider, top.model) : null;
  if (!price || !agent.mix) return { cost: null, priced: false };
  const m = agent.mix;
  const cost =
    (m.input / 1e6) * price.input +
    (m.output / 1e6) * price.output +
    (m.cacheRead / 1e6) * price.cacheRead +
    (m.cacheWrite / 1e6) * price.cacheWrite;
  return { cost, priced: true };
}

/** Sums each agent's `mix` into one camelCase totals object; missing mixes read as 0. */
function sumMix(agents) {
  const mix = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
  for (const a of agents) {
    const m = a.mix;
    if (!m) continue;
    mix.input += m.input;
    mix.output += m.output;
    mix.cacheRead += m.cacheRead;
    mix.cacheWrite += m.cacheWrite;
  }
  return mix;
}

/** Per-day token totals across every agent: `windowDays` present -> every day in the window (ascending,
 * zero-filled); absent (older binary) -> just the union of whatever `daily` entries exist, sorted ascending. */
function buildDailyUnion(agents, windowDays, nowMs) {
  const byDate = new Map();
  for (const a of agents) {
    for (const d of a.daily || []) byDate.set(d.date, (byDate.get(d.date) || 0) + d.tokens);
  }
  if (typeof windowDays === 'number' && windowDays > 0) {
    const days = [];
    for (let i = windowDays - 1; i >= 0; i--) {
      const iso = new Date(nowMs - i * 86400000).toISOString().slice(0, 10);
      days.push({ date: iso, tokens: byDate.get(iso) || 0 });
    }
    return days;
  }
  return Array.from(byDate.entries())
    .map(([date, tokens]) => ({ date, tokens }))
    .sort((a, b) => a.date.localeCompare(b.date));
}

/** Aggregates stats across `agents` for the dashboard's stats layer; `null` fields mean the data genuinely isn't known. */
function buildStats(usageJson, agents, nowMs) {
  const windowDays = typeof (usageJson && usageJson.window_days) === 'number' ? usageJson.window_days : null;
  const totalTokens = agents.reduce((sum, a) => sum + a.tokens, 0);
  const sessions = agents.reduce((sum, a) => sum + a.sessions, 0);
  const turns = agents.reduce((sum, a) => sum + a.turns, 0);
  const mix = sumMix(agents);
  const mixTotal = mix.input + mix.output + mix.cacheRead + mix.cacheWrite;
  const cacheShare = mixTotal > 0 ? mix.cacheRead / mixTotal : 0;
  const daily = buildDailyUnion(agents, windowDays, nowMs);
  const activeDays = daily.filter((d) => d.tokens > 0).length;
  let bestDay = null;
  for (const d of daily) {
    if (!bestDay || d.tokens > bestDay.tokens) bestDay = d;
  }
  let costUsd = agents.length ? 0 : null;
  let costPartial = false;
  for (const a of agents) {
    if (!a.tokens) continue;
    const { cost, priced } = estimateAgentCost(a);
    if (!priced) {
      costPartial = true;
      continue;
    }
    costUsd += cost;
  }
  return { windowDays, totalTokens, sessions, turns, activeDays, costUsd, costPartial, mix, cacheShare, daily, bestDay };
}

/** `configure provider --list --format json` → one entry per provider whose credential resolves, tier → model rows. */
function buildProviderModels(profileJson) {
  const list = (profileJson && Array.isArray(profileJson.list) && profileJson.list) || [];
  const session = (profileJson && profileJson.session) || null;
  return list
    .filter((p) => p && p.name && p.source && p.source !== 'missing')
    .map((p) => ({
      name: p.name,
      group: p.group || null,
      source: p.source,
      session: p.name === session,
      models: Object.entries(p.models || {})
        .map(([tier, id]) => ({ tier, id }))
        .sort((a, b) => a.tier.localeCompare(b.tier)),
    }));
}

/** Flat `usage --format json` + `configure provider --list --format json` → the model the status bar/webview render. */
function buildUsageModel(usageJson, profileJson, nowMs) {
  const now = typeof nowMs === 'number' ? nowMs : Date.now(); // reset labels still render at call time, not from this
  const billing = (usageJson && usageJson.billing) || {};
  const mode = billing.mode === 'api' ? 'api' : 'subscription';
  const billingProvider = billing.provider || null;
  const rows = (usageJson && usageJson.providers) || [];
  const order = [];
  const byKey = new Map();
  for (const row of rows) {
    const id = row.provider;
    if (!id) continue;
    const account = row.account && row.account !== '-' ? row.account : '';
    const key = account ? `${id}::${account}` : id;
    if (!byKey.has(key)) {
      order.push(key);
      byKey.set(key, { id, account, rows: [] });
    }
    byKey.get(key).rows.push(row);
  }
  const keys = order.includes('anthropic')
    ? ['anthropic', ...order.filter((k) => k !== 'anthropic')]
    : order;
  const providers = keys.map((key) => {
    const g = byKey.get(key);
    return buildProvider(g.id, g.account, g.rows);
  });
  const api = ((usageJson && usageJson.api) || []).map((r) => ({
    provider: r.provider,
    profile: r.profile || null,
    model: r.model,
    today: r.today || 0,
    last30d: r.last_30d || 0,
    allTime: r.all_time || 0,
  }));
  const agents = Array.isArray(usageJson && usageJson.agents) ? usageJson.agents.map(buildAgent) : [];
  const stats = buildStats(usageJson, agents, now);
  return { mode, billingProvider, providers, api, agents, stats, providerModels: buildProviderModels(profileJson) };
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
  buildStats,
};
