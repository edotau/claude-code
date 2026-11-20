'use strict';
// Databricks models in VS Code's picker via the loopback proxy's chat/completions; list from `configure --vscode --print`.
const vscode = require('vscode');

const VENDOR = 'claude-code-databricks';
const SYSTEM_ROLE = 3; // Copilot Chat's system prompt; the stable API names only User and Assistant
const UNAUTHORIZED = 401;
const DEFAULT_INPUT = 128000; // when the harness lists no window
const DEFAULT_OUTPUT = 32000;
const ERROR_BODY_MAX = 300;
const AGENT_HEADER = { 'X-Claude-Code-Agent': 'vscode' };

const parts = (content, ctor) => content.filter((p) => p instanceof ctor);
const textOf = (p) => (p instanceof vscode.LanguageModelTextPart ? p.value : '');
const imageUrl = (p) =>
  p instanceof vscode.LanguageModelDataPart && p.mimeType.startsWith('image/')
    ? `data:${p.mimeType};base64,${Buffer.from(p.data).toString('base64')}`
    : undefined;

/** One VS Code chat message becomes the OpenAI messages it stands for (tool results are their own messages). */
function messageToWire(message) {
  const text = message.content.map(textOf).join('');
  if (Number(message.role) === SYSTEM_ROLE) return [{ role: 'system', content: text }];
  const calls = parts(message.content, vscode.LanguageModelToolCallPart);
  if (message.role === vscode.LanguageModelChatMessageRole.Assistant) {
    const tool_calls = calls.map((c) => ({ id: c.callId, type: 'function', function: { name: c.name, arguments: JSON.stringify(c.input) } }));
    return text || tool_calls.length ? [{ role: 'assistant', content: text || null, ...(tool_calls.length ? { tool_calls } : {}) }] : [];
  }
  const results = parts(message.content, vscode.LanguageModelToolResultPart);
  const images = [...results.flatMap((r) => r.content), ...message.content].map(imageUrl).filter(Boolean);
  const content = [...(text ? [{ type: 'text', text }] : []), ...images.map((url) => ({ type: 'image_url', image_url: { url } }))];
  return [
    ...results.map((r) => ({ role: 'tool', tool_call_id: r.callId, content: r.content.map(textOf).join('') })),
    ...(content.length ? [{ role: 'user', content }] : []),
  ];
}

/** The chat/completions request body for one VS Code chat turn. */
function chatRequest(modelId, messages, options) {
  const tools = (options && options.tools) || [];
  const required = options && options.toolMode === vscode.LanguageModelChatToolMode.Required;
  return {
    model: modelId,
    messages: messages.flatMap(messageToWire),
    stream: true,
    ...(tools.length
      ? {
          tools: tools.map((t) => ({ type: 'function', function: { name: t.name, description: t.description, parameters: t.inputSchema || { type: 'object', properties: {} } } })),
          ...(required ? { tool_choice: 'required' } : {}),
        }
      : {}),
  };
}

/** Yields each SSE `data:` payload of a fetch response body as parsed JSON, stopping at [DONE]. */
async function* sseEvents(body) {
  const decoder = new TextDecoder();
  let buffer = '';
  const dataOf = (line) => (line.startsWith('data:') ? line.slice(5).trim() : '');
  for await (const chunk of body) {
    buffer += decoder.decode(chunk, { stream: true });
    const lines = buffer.split('\n');
    buffer = lines.pop();
    for (const line of lines) {
      const data = dataOf(line);
      if (data === '[DONE]') return;
      if (data) yield JSON.parse(data);
    }
  }
  const last = dataOf(buffer + decoder.decode());
  if (last && last !== '[DONE]') yield JSON.parse(last);
}

const FINISHED = new Set(['stop', 'tool_calls', 'function_call']);

const deltaText = (c) => (typeof c === 'string' ? c : (c || []).filter((b) => b.type === 'text').map((b) => b.text || '').join(''));

function addFragment(calls, f) {
  const call = (calls[f.index || 0] ||= { id: '', name: '', args: '' });
  call.id = f.id || call.id;
  call.name = (f.function && f.function.name) || call.name;
  call.args += (f.function && f.function.arguments) || '';
}

/** Maps chat/completions chunks to VS Code response parts: text as it streams, tool calls once complete. */
async function* replies(events) {
  const calls = [];
  let finish;
  for await (const event of events) {
    if (event.error) throw new Error(typeof event.error === 'string' ? event.error : event.error.message || JSON.stringify(event.error));
    for (const choice of event.choices || []) {
      const delta = choice.delta || {};
      const text = deltaText(delta.content);
      if (text) yield new vscode.LanguageModelTextPart(text);
      for (const f of delta.tool_calls || []) addFragment(calls, f);
      if (choice.finish_reason) {
        finish = choice.finish_reason;
        if (!FINISHED.has(finish)) throw new Error(`The model stopped before finishing the response (${finish}).`);
      }
    }
  }
  if (!finish) throw new Error('The response ended before the model finished. Try again.');
  for (const c of calls.filter(Boolean)) {
    let input;
    try {
      input = c.args.trim() ? JSON.parse(c.args) : {};
    } catch {
      input = undefined;
    }
    if (typeof input !== 'object' || input === null || Array.isArray(input)) throw new Error(`The model called ${c.name} with arguments that are not a JSON object.`);
    yield new vscode.LanguageModelToolCallPart(c.id, c.name, input);
  }
}

/** Per-profile proxy session tokens (opaque, no expiry): kept until the proxy rejects one. */
class ProxyTokens {
  constructor(run, log) {
    this.run = run; // (argv) => Promise<stdout>
    this.log = log;
    this.cache = new Map();
  }

  async get(profile) {
    if (!this.cache.has(profile)) this.cache.set(profile, await this.mint(profile));
    return this.cache.get(profile);
  }

  /** A token the proxy rejected: forget it and ask once more. */
  async renew(profile, rejected) {
    if (this.cache.get(profile) === rejected) this.cache.delete(profile);
    return this.get(profile);
  }

  async mint(profile) {
    const argv = ['resolve', '--secret', '--proxy', '--profile', profile];
    let out;
    try {
      out = await this.run(argv);
    } catch (err) {
      this.log(`token for ${profile} failed (${err.message}); starting the proxy and retrying once`);
      await this.run(['gateway', 'proxy', '--start']);
      out = await this.run(argv);
    }
    const token = String(out).trim();
    if (!token) throw new Error(`claude-code resolve --proxy printed nothing for ${profile}`);
    this.log(`token for ${profile}: ${token.length} bytes, kept until the proxy rejects it`);
    return token;
  }
}

class ChatProvider {
  /** deps: run(argv) -> stdout of the claude-code binary, fetch, log(line). */
  constructor({ run, fetch: fetchFn = globalThis.fetch, log = () => {} }) {
    this.run = run;
    this.fetch = fetchFn;
    this.log = log;
    this.tokens = new ProxyTokens(run, log);
    this.models = undefined; // session cache of the harness list: Map<vscodeId, {profile, base, id, ...}>
    this.changed = new vscode.EventEmitter();
    this.onDidChangeLanguageModelChatInformation = this.changed.event;
  }

  /** Drops the cached model list and asks VS Code to list again. */
  refresh() {
    this.models = undefined;
    this.changed.fire();
  }

  async load() {
    const listing = JSON.parse(await this.run(['configure', '--vscode', '--print']));
    const map = new Map();
    for (const p of listing.profiles || []) {
      for (const m of p.models || []) map.set(`${p.profile}:${m.id}`, { ...m, profile: p.profile, base: p.base });
    }
    this.log(`models: ${map.size} across ${(listing.profiles || []).length} profile(s)`);
    return map;
  }

  async provideLanguageModelChatInformation() {
    try {
      this.models ||= await this.load();
    } catch (err) {
      this.log(`model list failed: ${err.message}`);
      return [];
    }
    return [...this.models].map(([id, m]) => ({
      id,
      name: m.id,
      family: m.id,
      version: '1.0.0',
      detail: `Databricks ${m.profile}`,
      maxInputTokens: m.contextWindow || DEFAULT_INPUT,
      maxOutputTokens: m.maxOutputTokens || DEFAULT_OUTPUT,
      capabilities: { toolCalling: m.toolCalling !== false, imageInput: m.vision === true },
    }));
  }

  async provideTokenCount(_model, input) {
    if (typeof input === 'string') return Math.ceil(input.length / 4);
    return Math.ceil(input.content.map((p) => textOf(p).length || JSON.stringify(p.input || '').length).reduce((a, b) => a + b, 0) / 4);
  }

  async post(m, body, token, signal) {
    return this.fetch(`${m.base}/openai/chat/completions`, {
      method: 'POST',
      headers: { ...AGENT_HEADER, Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal,
    });
  }

  async provideLanguageModelChatResponse(model, messages, options, progress, token) {
    const m = this.models && this.models.get(model.id);
    if (!m) throw new Error(`unknown model ${model.id}: refresh the model list`);
    const body = chatRequest(m.id, messages, options);
    const abort = new AbortController();
    const sub = token && token.onCancellationRequested ? token.onCancellationRequested(() => abort.abort()) : undefined;
    try {
      let bearer = await this.tokens.get(m.profile);
      let res = await this.post(m, body, bearer, abort.signal);
      if (res.status === UNAUTHORIZED) {
        await res.body?.cancel?.();
        bearer = await this.tokens.renew(m.profile, bearer);
        res = await this.post(m, body, bearer, abort.signal);
      }
      if (!res.ok || !res.body) throw new Error(`proxy returned ${res.status}: ${(await res.text()).slice(0, ERROR_BODY_MAX)}`);
      for await (const part of replies(sseEvents(res.body))) progress.report(part);
    } catch (err) {
      if (!(token && token.isCancellationRequested)) {
        this.log(`${model.id} failed: ${err.message}`);
        throw err;
      }
    } finally {
      sub?.dispose?.();
    }
  }
}

module.exports = { VENDOR, ChatProvider, ProxyTokens, chatRequest, replies, sseEvents };
