'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const ts = require('../lib/tailscale');

const HARNESS = {
  BackendState: 'Running',
  DNSName: 'erics-macbook-pro.tailbb611b.ts.net.',
  IPs: ['100.107.35.117'],
  Peers: [
    { DNSName: 'wks0003289670.tailbb611b.ts.net.', IPs: ['100.112.103.4'], OS: 'linux', Online: true, ExitNode: true, ExitNodeOption: true },
    { DNSName: 'relay.tailbb611b.ts.net.', IPs: ['100.1.1.1'], OS: 'linux', Online: false, ExitNode: false, ExitNodeOption: true },
    { DNSName: 'iphone181.tailbb611b.ts.net.', IPs: ['100.105.201.90'], OS: 'iOS', Online: true, ExitNode: false, ExitNodeOption: false },
  ],
};
const CLI = {
  BackendState: 'Running',
  ExitNodeStatus: { ID: 'n1', Online: true, TailscaleIPs: ['100.112.103.4/32'] },
  Peer: {
    a: { ID: 'n1', HostName: 'WKS0003289670', DNSName: 'wks0003289670.tailbb611b.ts.net.', TailscaleIPs: ['100.112.103.4'], Online: true, ExitNodeOption: true, ExitNode: true },
    b: { ID: 'n2', HostName: 'relay', DNSName: 'relay.tailbb611b.ts.net.', TailscaleIPs: ['100.1.1.1'], Online: false, ExitNodeOption: true },
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
  assert.deepEqual(ts.harnessStatusArgs(), ['tailscale', 'status', '--format', 'json']);
  assert.deepEqual(ts.cliStatusArgs(), ['status', '--json']);
  assert.deepEqual(ts.setExitNodeArgs('100.112.103.4'), ['set', '--exit-node=100.112.103.4', '--exit-node-allow-lan-access=true']);
  assert.deepEqual(ts.setExitNodeArgs(''), ['set', '--exit-node=']);
});

test('parseHarnessStatus: advertised (ExitNodeOption) and in-use peers, trailing dot stripped, hostName = first label', () => {
  const s = ts.parseHarnessStatus(JSON.stringify(HARNESS));
  assert.equal(s.self, 'erics-macbook-pro.tailbb611b.ts.net');
  assert.deepEqual(s.options.map((o) => [o.hostName, o.ip, o.online, o.selected]), [['wks0003289670', '100.112.103.4', true, true], ['relay', '100.1.1.1', false, false]]);
});

test('parseCliStatus: active exit node IP without the /32, advertised peers with selection', () => {
  const s = ts.parseCliStatus(JSON.stringify(CLI));
  assert.equal(s.activeIp, '100.112.103.4');
  assert.deepEqual(s.advertised.map((p) => [p.hostName, p.selected]), [['WKS0003289670', true], ['relay', false]]);
  assert.equal(ts.parseCliStatus({ Peer: {} }).activeIp, null);
});

test('status(): merges the harness list with CLI selection; the harness binary is asked first', async () => {
  const f = fake({ 'claude-code': HARNESS, tailscale: CLI });
  const s = await ts.status({ binary: 'claude-code', tailscaleBinary: 'tailscale', execFileImpl: f.execFileImpl });
  assert.equal(s.ok, true);
  assert.deepEqual(f.calls[0], ['claude-code', 'tailscale', 'status', '--format', 'json']);
  assert.equal(s.exitNode.ip, '100.112.103.4');
  assert.deepEqual(s.options.map((o) => [o.hostName, o.selected]), [['wks0003289670', true], ['relay', false]]);
});

test('status(): CLI missing → the harness list and its in-use flag stand alone; both missing → ok:false; stopped backend → ok:false', async () => {
  const noCli = await ts.status({ execFileImpl: fake({ 'claude-code': HARNESS }).execFileImpl });
  assert.equal(noCli.ok, true);
  assert.equal(noCli.exitNode.hostName, 'wks0003289670');
  const none = await ts.status({ execFileImpl: fake({}).execFileImpl });
  assert.equal(none.ok, false);
  assert.match(none.message, /not found/);
  const stopped = await ts.status({ execFileImpl: fake({ 'claude-code': { ...HARNESS, BackendState: 'Stopped' }, tailscale: CLI }).execFileImpl });
  assert.equal(stopped.ok, false);
  assert.match(stopped.message, /stopped/);
});

test('setExitNode(): runs the local CLI set argv; a failure rejects with stderr (teeth: never the harness binary)', async () => {
  const f = fake({ tailscale: '' });
  await ts.setExitNode('100.112.103.4', { execFileImpl: f.execFileImpl });
  assert.deepEqual(f.calls[0], ['tailscale', 'set', '--exit-node=100.112.103.4', '--exit-node-allow-lan-access=true']);
  const err = Object.assign(new Error('exit 1'), { stderr: 'access denied' });
  await assert.rejects(ts.setExitNode('x', { execFileImpl: fake({ tailscale: err }).execFileImpl }), (e) => e.stderr === 'access denied');
});
