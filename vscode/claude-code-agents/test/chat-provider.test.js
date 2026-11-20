'use strict';
// Contract: the chat provider lists the harness's models, sends one chat/completions request per turn,
// reports streamed text and tool calls, retries a 401 once with a fresh token, and never logs a token.
const test = require('node:test');
const assert = require('node:assert/strict');
const { vscode } = require('./fake-vscode');

// The fake vscode has no language-model API; add the classes the provider needs before loading it.
class Part { constructor(...a) { this.a = a; } }
Object.assign(vscode, {
  EventEmitter: class { constructor() { this.listeners = []; this.event = (fn) => this.listeners.push(fn); } fire() { this.listeners.forEach((f) => f()); } },
  LanguageModelTextPart: class extends Part { get value() { return this.a[0]; } },
  LanguageModelToolCallPart: class extends Part { get callId() { return this.a[0]; } get name() { return this.a[1]; } get input() { return this.a[2]; } },
  LanguageModelToolResultPart: class extends Part { get callId() { return this.a[0]; } get content() { return this.a[1]; } },
  LanguageModelDataPart: class extends Part {},
  LanguageModelChatMessageRole: { User: 1, Assistant: 2 },
  LanguageModelChatToolMode: { Auto: 1, Required: 2 },
});
const { ChatProvider } = require('../lib/chat-provider');

const LISTING = { profiles: [{ profile: 'dev', base: 'http://127.0.0.1:1/p/dev', models: [
  { id: 'databricks-gpt-x', toolCalling: true, vision: false, contextWindow: 200000, maxOutputTokens: 32000 }] }] };
const SECRET = 'cct_tokensecret1';

const sse = (...events) => ({ ok: true, status: 200, body: [new TextEncoder().encode(events.map((e) => `data: ${typeof e === 'string' ? e : JSON.stringify(e)}\n\n`).join(''))] });
const delta = (d, finish) => ({ choices: [{ delta: d, finish_reason: finish || null }] });

function setup(responses) {
  const runs = [], fetches = [], logs = [];
  let n = 0;
  const run = async (argv) => {
    runs.push(argv.join(' '));
    if (argv[0] === 'configure') return JSON.stringify(LISTING);
    return `${SECRET}${n++}\n`;
  };
  const fetch = async (url, init) => {
    fetches.push({ url, init, body: JSON.parse(init.body) });
    return responses.shift();
  };
  return { runs, fetches, logs, provider: new ChatProvider({ run, fetch, log: (l) => logs.push(l) }) };
}

async function chat(provider, messages, options) {
  const [info] = await provider.provideLanguageModelChatInformation();
  const parts = [];
  await provider.provideLanguageModelChatResponse(info, messages, options || {}, { report: (p) => parts.push(p) }, { isCancellationRequested: false });
  return parts;
}

test('models come from the harness command and carry its limits', async () => {
  const { provider, runs } = setup([]);
  const infos = await provider.provideLanguageModelChatInformation();
  await provider.provideLanguageModelChatInformation();
  assert.deepEqual(runs, ['configure --vscode --print'], 'listed once per session');
  assert.equal(infos[0].id, 'dev:databricks-gpt-x');
  assert.equal(infos[0].maxInputTokens, 200000);
  assert.equal(infos[0].capabilities.toolCalling, true);
});

test('a turn becomes a chat/completions body; streamed text and a tool call come back', async () => {
  const { provider, fetches } = setup([sse(
    delta({ content: 'hel' }), delta({ content: 'lo' }),
    delta({ tool_calls: [{ index: 0, id: 'c1', function: { name: 'read', arguments: '{"p"' } }] }),
    delta({ tool_calls: [{ index: 0, function: { arguments: ':1}' } }] }, 'tool_calls'), '[DONE]')]);
  const user = { role: 1, content: [new vscode.LanguageModelTextPart('hi')] };
  const tool = { name: 'read', description: 'd', inputSchema: { type: 'object' } };
  const parts = await chat(provider, [user], { tools: [tool] });
  assert.equal(fetches[0].url, 'http://127.0.0.1:1/p/dev/openai/chat/completions');
  assert.equal(fetches[0].init.headers.Authorization, `Bearer ${SECRET}0`);
  assert.deepEqual(fetches[0].body.messages, [{ role: 'user', content: [{ type: 'text', text: 'hi' }] }]);
  assert.equal(fetches[0].body.model, 'databricks-gpt-x');
  assert.equal(fetches[0].body.tools[0].function.name, 'read');
  assert.equal(parts.filter((p) => p.value).map((p) => p.value).join(''), 'hello');
  const call = parts.find((p) => p.callId);
  assert.deepEqual([call.callId, call.name, call.input], ['c1', 'read', { p: 1 }]);
});

test('a 401 fetches one new token and retries once; no token reaches the log', async () => {
  const { provider, fetches, runs, logs } = setup([{ status: 401, ok: false }, sse(delta({ content: 'ok' }, 'stop'), '[DONE]')]);
  const parts = await chat(provider, [{ role: 1, content: [new vscode.LanguageModelTextPart('x')] }]);
  assert.equal(parts[0].value, 'ok');
  assert.equal(fetches.length, 2);
  assert.equal(runs.filter((r) => r.startsWith('resolve')).length, 2, 'one token, one renewal');
  assert.deepEqual(runs.filter((r) => r.startsWith('resolve')).map((r) => r.endsWith('--refresh')), [false, true], 'only the renewal forces a new token');
  assert.equal(fetches[0].init.headers.Authorization, `Bearer ${SECRET}0`);
  assert.equal(fetches[1].init.headers.Authorization, `Bearer ${SECRET}1`);
  assert.ok(logs.length > 0 && !logs.join('\n').includes('cct_'), 'log holds byte counts only');
});

const ask = (provider) => chat(provider, [{ role: 1, content: [new vscode.LanguageModelTextPart('x')] }]);

test('two concurrent 401s share one refresh', async () => {
  const { provider, runs } = setup([{ status: 401, ok: false }, { status: 401, ok: false }, sse(delta({ content: 'a' }, 'stop')), sse(delta({ content: 'b' }, 'stop'))]);
  await provider.provideLanguageModelChatInformation();
  await provider.tokens.get('dev');
  await Promise.all([ask(provider), ask(provider)]);
  assert.equal(runs.filter((r) => r.endsWith('--refresh')).length, 1, 'one refresh for both callers');
});

test('a second 401 after the refresh surfaces the proxy error and logs no token', async () => {
  const text = async () => 'invalid session token';
  const { provider, runs, logs } = setup([{ status: 401, ok: false }, { status: 401, ok: false, text }]);
  await assert.rejects(ask(provider), /proxy returned 401: invalid session token/);
  assert.equal(runs.filter((r) => r.endsWith('--refresh')).length, 1, 'refreshed once, no loop');
  assert.ok(logs.length > 0 && !logs.join('\n').includes('cct_'), 'log holds byte counts only');
});
