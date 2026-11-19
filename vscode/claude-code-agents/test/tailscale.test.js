'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const ts = require('../lib/tailscale');

const HARNESS = {
  BackendState: 'Running',
  DNSName: 'erics-macbook-pro.tail0example.ts.net.',
  IPs: ['100.64.0.1'],
  Peers: [
    { DNSName: 'devhost0001.tail0example.ts.net.', IPs: ['100.64.0.10'], OS: 'linux', Online: true, ExitNode: true, ExitNodeOption: true },
    { DNSName: 'relay.tail0example.ts.net.', IPs: ['192.0.2.1'], OS: 'linux', Online: false, ExitNode: false, ExitNodeOption: true },
    { DNSName: 'iphone181.tail0example.ts.net.', IPs: ['100.64.0.2'], OS: 'iOS', Online: true, ExitNode: false, ExitNodeOption: false },
  ],
};
const CLI = {
  BackendState: 'Running',
  ExitNodeStatus: { ID: 'n1', Online: true, TailscaleIPs: ['100.64.0.10/32'] },
  Peer: {
    a: { ID: 'n1', HostName: 'DEVHOST0001', DNSName: 'devhost0001.tail0example.ts.net.', TailscaleIPs: ['100.64.0.10'], Online: true, ExitNodeOption: true, ExitNode: true },
    b: { ID: 'n2', HostName: 'relay', DNSName: 'relay.tail0example.ts.net.', TailscaleIPs: ['192.0.2.1'], Online: false, ExitNodeOption: true },
  },
};

const fake = (answers) => {
  const calls = [];
  return {
    calls,
    execFileImpl: (bin, args, opts, cb) => {
      calls.push([bin, ...args]);
      const a = answers[bin];
      if (a === undefined) return cb(Object.assign(new Error('spawn'), { code: 'ENOENT' }), '', '');
      if (a instanceof Error) return cb(a, '', a.stderr || '');
      cb(null, typeof a === 'string' ? a : JSON.stringify(a), '');
    },
  };
};

test('argv: harness list, CLI status, set --exit-node keeps LAN access, empty clears', () => {
  assert.deepEqual(ts.harnessStatusArgs(), ['network', 'tailscale', '--format', 'json']);
  assert.deepEqual(ts.cliStatusArgs(), ['status', '--json']);
  assert.deepEqual(ts.setExitNodeArgs('100.64.0.10'), ['set', '--exit-node=100.64.0.10', '--exit-node-allow-lan-access=true']);
  assert.deepEqual(ts.setExitNodeArgs(''), ['set', '--exit-node=']);
});

test('parseHarnessStatus: advertised (ExitNodeOption) and in-use peers, trailing dot stripped, hostName = first label', () => {
  const s = ts.parseHarnessStatus(JSON.stringify(HARNESS));
  assert.equal(s.self, 'erics-macbook-pro.tail0example.ts.net');
  assert.deepEqual(s.options.map((o) => [o.hostName, o.ip, o.online, o.selected]), [['devhost0001', '100.64.0.10', true, true], ['relay', '192.0.2.1', false, false]]);
});

test('parseCliStatus: active exit node IP without the /32, advertised peers with selection', () => {
  const s = ts.parseCliStatus(JSON.stringify(CLI));
  assert.equal(s.activeIp, '100.64.0.10');
  assert.deepEqual(s.advertised.map((p) => [p.hostName, p.selected]), [['DEVHOST0001', true], ['relay', false]]);
  assert.equal(ts.parseCliStatus({ Peer: {} }).activeIp, null);
});

test('status(): merges the harness list with CLI selection; the harness binary is asked first', async () => {
  const f = fake({ 'claude-code': HARNESS, tailscale: CLI });
  const s = await ts.status({ binary: 'claude-code', tailscaleBinary: 'tailscale', execFileImpl: f.execFileImpl });
  assert.equal(s.ok, true);
  assert.deepEqual(f.calls[0], ['claude-code', 'network', 'tailscale', '--format', 'json']);
  assert.equal(s.exitNode.ip, '100.64.0.10');
  assert.deepEqual(s.options.map((o) => [o.hostName, o.selected]), [['devhost0001', true], ['relay', false]]);
});

test('status(): CLI missing → the harness list and its in-use flag stand alone; both missing → ok:false; stopped backend → ok:false', async () => {
  const noCli = await ts.status({ execFileImpl: fake({ 'claude-code': HARNESS }).execFileImpl });
  assert.equal(noCli.ok, true);
  assert.equal(noCli.exitNode.hostName, 'devhost0001');
  const none = await ts.status({ execFileImpl: fake({}).execFileImpl });
  assert.equal(none.ok, false);
  assert.match(none.message, /not found/);
  const stopped = await ts.status({ execFileImpl: fake({ 'claude-code': { ...HARNESS, BackendState: 'Stopped' }, tailscale: CLI }).execFileImpl });
  assert.equal(stopped.ok, false);
  assert.match(stopped.message, /stopped/);
});

test('setExitNode(): runs the local CLI set argv; a failure rejects with stderr (teeth: never the harness binary)', async () => {
  const f = fake({ tailscale: '' });
  await ts.setExitNode('100.64.0.10', { execFileImpl: f.execFileImpl });
  assert.deepEqual(f.calls[0], ['tailscale', 'set', '--exit-node=100.64.0.10', '--exit-node-allow-lan-access=true']);
  const err = Object.assign(new Error('exit 1'), { stderr: 'access denied' });
  await assert.rejects(ts.setExitNode('x', { execFileImpl: fake({ tailscale: err }).execFileImpl }), (e) => e.stderr === 'access denied');
});
