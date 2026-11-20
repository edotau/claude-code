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

const THREE = { profiles: ['dev', 'qa', 'prod'].map((p) => ({ profile: p, base: `http://127.0.0.1:1/p/${p}`, models: [{ id: `m-${p}` }] })) };
const fakeState = (store = new Map()) => ({ store, get: (k) => store.get(k), update: async (k, v) => store.set(k, v) });

function setup(responses, { listing = LISTING, state } = {}) {
  const runs = [], fetches = [], logs = [];
  let n = 0;
  const run = async (argv) => {
    runs.push(argv.join(' '));
    if (argv[0] === 'configure') return JSON.stringify(listing);
    return `${SECRET}${n++}\n`;
  };
  const fetch = async (url, init) => {
    fetches.push({ url, init, body: JSON.parse(init.body) });
    return responses.shift();
  };
  return { runs, fetches, logs, provider: new ChatProvider({ run, fetch, state, log: (l) => logs.push(l) }) };
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

const ids = async (provider) => (await provider.provideLanguageModelChatInformation()).map((i) => i.id);
const pick = (label) => { vscode.window.showQuickPick = async (items) => { pick.items = items; return items.find((i) => i.label === label); }; };

test('only the active profile is listed; the first is the default', async () => {
  const { provider } = setup([], { listing: THREE, state: fakeState() });
  assert.deepEqual(await ids(provider), ['dev:m-dev']);
});

test('choosing a profile relists it and fires the change event once; the pick marks the current one', async () => {
  const { provider } = setup([], { listing: THREE, state: fakeState() });
  let fired = 0;
  provider.onDidChangeLanguageModelChatInformation(() => fired++);
  pick('qa');
  await provider.switchProfile();
  assert.deepEqual(pick.items.map((i) => i.label), ['dev', 'qa', 'prod', 'Refresh']);
  assert.equal(pick.items[0].description, 'current');
  assert.equal(fired, 1);
  assert.deepEqual(await ids(provider), ['qa:m-qa']);
});

test('Refresh refetches the list and keeps the profile', async () => {
  const { provider, runs } = setup([], { listing: THREE, state: fakeState(new Map([['claudeCodeAgents.databricksProfile', 'prod']])) });
  await ids(provider);
  pick('Refresh');
  await provider.switchProfile();
  assert.deepEqual(await ids(provider), ['prod:m-prod']);
  assert.equal(runs.filter((r) => r.startsWith('configure')).length, 2);
});

test('the choice survives a restart; a vanished stored profile falls back to the first and logs it', async () => {
  const state = fakeState();
  pick('prod');
  await setup([], { listing: THREE, state }).provider.switchProfile();
  assert.deepEqual(await ids(setup([], { listing: THREE, state }).provider), ['prod:m-prod']);
  state.store.set('claudeCodeAgents.databricksProfile', 'gone');
  const { provider, logs } = setup([], { listing: THREE, state });
  assert.deepEqual(await ids(provider), ['dev:m-dev']);
  assert.ok(logs.some((l) => l.includes('gone')));
});

test('a model of a non-active profile is still served on its own route, even before any listing', async () => {
  const { provider, fetches } = setup([sse(delta({ content: 'ok' }, 'stop'))], { listing: THREE, state: fakeState() });
  const user = { role: 1, content: [new vscode.LanguageModelTextPart('x')] };
  await provider.provideLanguageModelChatResponse({ id: 'prod:m-prod' }, [user], {}, { report() {} }, { isCancellationRequested: false });
  assert.equal(fetches[0].url, 'http://127.0.0.1:1/p/prod/openai/chat/completions');
});
