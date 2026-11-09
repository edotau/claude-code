const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

class Element {
  constructor() {
    this.style = {};
    this.children = [];
    this.text = '';
    this.innerHTML = '';
  }
  set textContent(value) { this.text = value; this.children = []; }
  get textContent() { return this.text + this.children.map(child => child.textContent).join(''); }
  appendChild(child) { this.children.push(child); child.parentNode = this; return child; }
  addEventListener() {}
  setAttribute() {}
  querySelectorAll() { return []; }
}

function dashboard() {
  const source = fs.readFileSync(0, 'utf8');
  const requests = [];
  const elements = new Map();
  const document = {
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, new Element());
      return elements.get(id);
    },
    createElement: () => new Element(),
    createDocumentFragment: () => new Element(),
    querySelector: () => ({ content: 'csrf-test-token' }),
    querySelectorAll: () => [],
  };
  const context = vm.createContext({
    document, window: {}, AbortController, TextDecoder,
    fetch(url, options) {
      const request = { url, options };
      requests.push(request);
      return new Promise((resolve, reject) => Object.assign(request, { resolve, reject }));
    },
  });
  const functionSource = name => {
    const start = source.indexOf('function ' + name + '(');
    assert.notEqual(start, -1, 'missing template function ' + name);
    const end = source.indexOf('\n}\n', start);
    assert.notEqual(end, -1, 'missing function end for ' + name);
    return source.slice(start, end + 3);
  };
  const declaration = name => {
    const match = source.match(new RegExp('^(?:const|let) ' + name + ' = .+', 'm'));
    assert.ok(match, 'missing template declaration ' + name);
    return match[0];
  };
  const functions = ['getJSON', 'restoreFocus', 'el', 'planCardFor', 'markPlanOpen',
    'closePlan', 'planFull', 'taskRow', 'renderPlanDetail', 'showPlan', 'postTick', 'loadUsageModules'];
  const declarations = ['csrfToken', 'focusKey', 'openPlan', 'planFullOpen', 'planX', 'planPark',
    'USAGE_MODULES', 'usageState'];
  vm.runInContext(source.slice(source.indexOf('function escapeHTML('), source.indexOf('function renderBrowsers(')) +
    functions.map(functionSource).join('\n') + '\n' + declarations.map(declaration).join('\n'), context);
  return { requests, document, run: code => vm.runInContext(code, context) };
}

const flush = () => new Promise(resolve => setImmediate(resolve));
const jsonResponse = value => ({ ok: true, json: async () => value });
const planResponse = path => jsonResponse({ path, markdown: 'Plan ' + path, tasks: [] });

async function detail() {
  const page = dashboard();
  page.run("openDetail('session', 'agent-A'); openDetail('session', 'agent-B');");
  page.requests[1].resolve(jsonResponse({ id: 'agent-B', status: 'running' }));
  await flush();
  const selectedHTML = page.document.getElementById('detail').innerHTML;
  assert.match(selectedHTML, /agent-B/, 'B must render before the stale response');
  const selectedStream = page.requests[2];
  assert.match(selectedStream.url, /agent=agent-B&follow=1/);
  page.requests[0].resolve(jsonResponse({ id: 'agent-A', status: 'running' }));
  await flush();
  assert.equal(page.document.getElementById('detail').innerHTML, selectedHTML, 'stale A snapshot overwrote B');
  assert.equal(page.requests.length, 3, 'stale A snapshot opened another stream');
  assert.equal(selectedStream.options.signal.aborted, false, 'B stream must remain active');
  page.run("openDetail('session', 'agent-C'); openDetail('session', 'agent-D');");
  assert.equal(selectedStream.options.signal.aborted, true, 'navigation must abort B stream');
  page.requests[4].resolve(jsonResponse({ id: 'agent-D', status: 'finished' }));
  await flush();
  const latestHTML = page.document.getElementById('detail').innerHTML;
  page.requests[3].reject(new Error('stale C failure'));
  await flush();
  assert.equal(page.document.getElementById('detail').innerHTML, latestHTML, 'stale error overwrote D');
  page.run("openDetail('session', 'agent-E'); closeDetail();");
  page.requests[5].resolve(jsonResponse({ id: 'agent-E', status: 'running' }));
  await flush();
  assert.equal(page.document.getElementById('detail').innerHTML, '', 'closed detail received a late snapshot');
  assert.equal(page.requests.length, 6, 'closed detail started a stream');
}

async function planFailure() {
  const page = dashboard();
  page.run("showPlan('A'); showPlan('B');");
  page.requests[1].resolve(planResponse('B'));
  await flush();
  assert.equal(page.run('planX.textContent'), 'Show full planPlan B');
  page.requests[0].reject(new Error('stale A failure'));
  await flush();
  assert.equal(page.run('openPlan'), 'B');
  assert.equal(page.run('planX.textContent'), 'Show full planPlan B', 'stale A failure replaced B panel');
  page.run("showPlan('C');");
  page.requests[2].reject(new Error('current C failure'));
  await flush();
  assert.equal(page.run('planX.textContent'), 'current C failure', 'current failure must remain visible');
}

async function tick() {
  const page = dashboard();
  page.run("showPlan('A');");
  page.requests[0].resolve(planResponse('A'));
  await flush();
  page.run("postTick('A', {line: 1, title: 'task'}, true, {}); showPlan('B');");
  page.requests[2].resolve(planResponse('B'));
  await flush();
  page.requests[1].resolve({ ok: true });
  await flush();
  assert.equal(page.run('openPlan'), 'B', 'tick completion reopened A after navigation to B');
  assert.equal(page.run('planX.textContent'), 'Show full planPlan B');
  assert.equal(page.requests.length, 3, 'tick completion refetched a plan no longer selected');
  page.run("postTick('B', {line: 2, title: 'task'}, true, {});");
  page.requests[3].resolve({ ok: true });
  await flush();
  assert.equal(page.requests[4].url, '/api/plan?path=B', 'selected plan must still refresh after tick');
  page.requests[4].resolve(planResponse('B'));
  await flush();
  page.run("postTick('B', {line: 2, title: 'task'}, false, {}); closePlan();");
  page.requests[5].resolve({ ok: true });
  await flush();
  assert.equal(page.run('openPlan'), null, 'tick completion reopened a closed plan');
  assert.equal(page.requests.length, 6, 'tick completion refetched a closed plan');
}

async function usage() {
  const page = dashboard();
  const first = page.run('loadUsageModules()');
  assert.equal(page.run('loadUsageModules()'), first, 'concurrent callers must share the pending load');
  const failure = assert.rejects(first, /temporary outage/);
  page.requests[0].reject(new Error('temporary outage'));
  await failure;
  const retry = page.run('loadUsageModules()');
  assert.notEqual(retry, first, 'failed module promise stayed cached');
  assert.equal(page.requests.length, 8, 'retry must fetch the four modules again');
  for (const request of page.requests.slice(4)) {
    request.resolve({ ok: true, text: async () => 'module.exports = { loaded: true };' });
  }
  const modules = await retry;
  assert.equal(modules.model.loaded, true);
  assert.equal(modules.view.loaded, true);
  assert.equal(page.run('loadUsageModules()'), retry, 'successful load must remain cached');
  assert.equal(page.requests.length, 8, 'cached success must not refetch modules');
}

const scenarios = { detail, planFailure, tick, usage };
scenarios[process.argv[2]]().then(() => {
  console.log(process.argv[2] + ': passed');
}).catch(error => {
  console.error(error);
  process.exitCode = 1;
});
