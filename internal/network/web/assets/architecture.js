// Architecture tab: a GitHub-style repo browser over /api/architecture (curated notes + live source scan + CLI verbs).
(function () {
  'use strict';
  const SEV = ['critical', 'high', 'medium', 'low'];
  const SEVCOL = { critical: '#ef2929', high: '#E95420', medium: '#fce94f', low: '#8ae234' };
  const RISK = { none: 'sev-low', low: 'sev-low', medium: 'sev-medium', high: 'sev-high' };
  const GUARDED = ['internal/cli', 'internal/launch', 'internal/configure', 'internal/hooks', 'internal/agent'];
  const MAXROWS = 200;
  const SUBS = ['code', 'issues', 'pulls', 'commands', 'insights', 'check'];
  const ID_RE = /\b(P\d+|F-[A-Z]\d+)\b/g;
  const ICONS = {
    dir: 'M1.75 1A1.75 1.75 0 0 0 0 2.75v10.5C0 14.216.784 15 1.75 15h12.5A1.75 1.75 0 0 0 16 13.25v-8.5A1.75 1.75 0 0 0 14.25 3H7.5a.25.25 0 0 1-.2-.1l-.9-1.2C6.07 1.26 5.55 1 5 1H1.75Z',
    file: 'M2 1.75C2 .784 2.784 0 3.75 0h6.586c.464 0 .909.184 1.237.513l2.914 2.914c.329.328.513.773.513 1.237v9.586A1.75 1.75 0 0 1 13.25 16h-9.5A1.75 1.75 0 0 1 2 14.25Zm1.75-.25a.25.25 0 0 0-.25.25v12.5c0 .138.112.25.25.25h9.5a.25.25 0 0 0 .25-.25V6h-2.75A1.75 1.75 0 0 1 9 4.25V1.5Zm6.75.062V4.25c0 .138.112.25.25.25h2.688l-.011-.013-2.914-2.914-.013-.011Z',
    issue: 'M8 9.5a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3ZM8 0a8 8 0 1 1 0 16A8 8 0 0 1 8 0ZM1.5 8a6.5 6.5 0 1 0 13 0 6.5 6.5 0 0 0-13 0Z',
    pr: 'M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354ZM3.75 2.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm0 9.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm8.25.75a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0Z',
    chev: 'M6.22 3.22a.75.75 0 0 1 1.06 0l4.25 4.25a.75.75 0 0 1 0 1.06l-4.25 4.25a.75.75 0 0 1-1.06-1.06L9.94 8 6.22 4.28a.75.75 0 0 1 0-1.06Z',
  };
  const st = { data: null, route: { sub: 'code', rest: '' }, sev: '', area: '', q: '', rtOpen: false };
  const pane = document.getElementById('architecturePane');
  let body = null;
  let tabBar = null;

  function el(tag, cls, text, kids) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null && text !== '') n.textContent = text;
    (kids || []).forEach(function (k) { if (k) n.appendChild(k); });
    return n;
  }
  function icon(kind, color) {
    const ns = 'http://www.w3.org/2000/svg';
    const s = document.createElementNS(ns, 'svg');
    const p = document.createElementNS(ns, 'path');
    [[s, 'viewBox', '0 0 16 16'], [s, 'class', 'arch-ico'], [p, 'd', ICONS[kind]], [p, 'fill', color || '#729fcf']]
      .forEach(function (a) { a[0].setAttribute(a[1], a[2]); });
    s.appendChild(p);
    return s;
  }
  function badge(text, cls, color) {
    const b = el('span', 'arch-badge ' + (cls || ''), text);
    if (color) b.style.borderColor = color;
    return b;
  }
  function areaLabel(text) {
    let h = 0;
    for (let i = 0; i < text.length; i++) h = (h * 31 + text.charCodeAt(i)) % 360;
    const b = el('span', 'arch-badge', text);
    b.style.background = 'hsl(' + h + ',35%,22%)';
    b.style.color = 'hsl(' + h + ',70%,78%)';
    return b;
  }
  function num(n) { return n >= 1000 ? (n / 1000).toFixed(1) + 'k' : String(n); }
  function empty(msg) { return el('p', 'muted', msg); }
  function section(title, kids) { return el('div', 'arch-sec', null, [el('h3', '', title)].concat(kids)); }

  function curated() { return (st.data && st.data.curated) || {}; }
  function scan() { return (st.data && st.data.scan) || {}; }
  function scanDirs() { return scan().dirs || []; }
  function hashFor(sub, rest) {
    return '#arch/' + sub + (rest ? '/' + rest.split('/').map(encodeURIComponent).join('/') : '');
  }
  function parseHash() {
    const m = /^#arch(?:\/([^/]*))?(?:\/(.*))?$/.exec(location.hash);
    if (!m) return { sub: 'code', rest: '' };
    let rest = (m[2] || '').replace(/\/+$/, '');
    try { rest = rest.split('/').map(decodeURIComponent).join('/'); } catch (e) { /* keep the raw segments */ }
    return { sub: SUBS.indexOf(m[1]) >= 0 ? m[1] : 'code', rest: rest };
  }
  function go(sub, rest) {
    const h = hashFor(sub, rest);
    if (location.hash === h) render(); else location.hash = h;
  }
  function link(sub, rest, text, cls) {
    const a = el('a', cls || 'arch-a', text);
    a.href = hashFor(sub, rest);
    return a;
  }

  // Derived lookups, built once per payload.
  function prepare() {
    const set = {};
    const add = function (p) {
      for (let q = p; q; q = q.indexOf('/') < 0 ? '' : q.slice(0, q.lastIndexOf('/'))) set[q] = true;
    };
    scanDirs().forEach(function (d) { add(d.path); });
    (scan().top || []).concat(scan().tree || [], Object.keys(curated().dirs || {})).forEach(add);
    const disk = {};
    scanDirs().map(function (d) { return d.path; }).concat(scan().top || [], scan().tree || []).forEach(function (p) { disk[p] = true; });
    const byKey = function (list, key) {
      const m = {};
      (list || []).forEach(function (x) { m[x[key]] = x; });
      return m;
    };
    st.ix = { paths: Object.keys(set).sort(), set: set, disk: disk, dm: byKey(scanDirs(), 'path'),
      props: byKey(curated().proposals, 'id'), finds: byKey(curated().findings, 'id') };
  }
  function cdir(p) { return (curated().dirs || {})[p] || {}; }
  function parentOf(p) { return p.indexOf('/') < 0 ? '' : p.slice(0, p.lastIndexOf('/')); }
  function groupColor(p) {
    const g = (curated().groups || []).find(function (x) { return x.id === cdir(p).group; });
    return g ? g.color : '';
  }

  // Text with P2 / F-S1 ids turned into links to their pages.
  function rich(text) {
    const out = el('span');
    let last = 0;
    String(text || '').replace(ID_RE, function (id, _g, off) {
      const hit = st.ix.props[id] ? 'pulls' : st.ix.finds[id] ? 'issues' : '';
      if (!hit) return id;
      out.appendChild(document.createTextNode(text.slice(last, off)));
      out.appendChild(link(hit, id, id));
      last = off + id.length;
      return id;
    });
    out.appendChild(document.createTextNode(String(text || '').slice(last)));
    return out;
  }
  function field(label, text) { return text ? el('div', 'arch-field', null, [el('b', '', label + ': '), rich(text)]) : null; }
  function chip(sub, rest, text, color) {
    const c = link(sub, rest, text, 'arch-chip');
    c.title = rest;
    if (color) c.style.borderColor = color;
    return c;
  }
  function dirChips(list) {
    return el('div', 'arch-chips', null, (list || []).map(function (p) { return chip('code', p, p.replace(/^internal\//, ''), groupColor(p)); }));
  }
  function chipField(label, list, mk) {
    const box = mk ? el('div', 'arch-chips', null, (list || []).map(mk)) : dirChips(list);
    return list && list.length ? el('div', 'arch-field', null, [el('b', '', label + ':'), box]) : null;
  }
  function flagList(list) {
    return el('div', 'arch-flags', null, (list || []).map(function (f) { return el('span', '', f); }));
  }
  function crumbs(sub, segs) {
    const bar = el('nav', 'arch-crumbs');
    const all = [{ label: 'claude-code', rest: '' }].concat(segs);
    all.forEach(function (s, i) {
      if (i) bar.appendChild(el('span', 'muted', '/'));
      bar.appendChild(i === all.length - 1 ? el('b', '', s.label) : link(sub, s.rest, s.label));
    });
    return bar;
  }
  function readme(kids) {
    return el('div', 'arch-readme', null, [el('div', 'arch-readme-h', 'README'),
      el('div', 'arch-readme-b', null, kids.filter(Boolean))]);
  }
  function row(href, ico, name, desc, meta, cls) {
    const r = el(href ? 'a' : 'div', 'arch-row ' + (cls || ''), null, [ico, name, el('span', 'arch-rd', desc), el('span', 'arch-rm', meta)]);
    if (href) r.href = href;
    return r;
  }

  // ---- Code ----
  function subtree(dir) {
    const pre = dir ? dir + '/' : '';
    let n = 0;
    let loc = 0;
    scanDirs().forEach(function (d) {
      if (d.files > 0 && d.path.indexOf(pre) === 0) { n++; loc += d.loc; }
    });
    return { n: n, loc: loc };
  }
  function dirMeta(p) {
    const d = st.ix.dm[p];
    if (d && d.files > 0) return 'L' + d.layer + ' · ' + num(d.loc) + ' lines';
    const s = subtree(p);
    return s.n ? s.n + (s.n === 1 ? ' pkg · ' : ' pkgs · ') + num(s.loc) + ' lines' : '';
  }
  function presence(p) {
    const c = (curated().dirs || {})[p];
    if (!c && st.ix.disk[p]) return badge('undescribed', 'sev-high');
    return c && !st.ix.disk[p] ? badge('missing', 'sev-critical') : null;
  }
  function dirRow(p, label) {
    const nm = el('span', 'arch-rn', label || p.slice(p.lastIndexOf('/') + 1), [presence(p)]);
    return row(hashFor('code', p), icon('dir', groupColor(p) || '#729fcf'), nm, cdir(p).purpose, dirMeta(p));
  }
  function fileRow(dir, f, full) {
    const nm = el('span', 'arch-rn', full ? dir + '/' + f.name : f.name);
    return row('', icon('file', '#a89fa6'), nm, '', f.lines + ' lines', 'file');
  }
  function childDirs(dir) {
    return st.ix.paths.filter(function (p) { return parentOf(p) === dir; });
  }
  function runtimeRows(list, rows) {
    const btn = el('button', 'arch-row arch-rt', null, [icon('chev', '#a89fa6'),
      el('span', 'arch-rn', 'Runtime data (' + list.length + ', gitignored)'), el('span', 'arch-rd', 'state, caches and logs; not tracked'),
      el('span', 'arch-rm', st.rtOpen ? 'hide' : 'show')]);
    btn.type = 'button';
    btn.addEventListener('click', function () { st.rtOpen = !st.rtOpen; render(); });
    rows.push(btn);
    if (st.rtOpen) list.forEach(function (p) { rows.push(dirRow(p)); });
  }
  function browseRows(dir) {
    const kids = childDirs(dir);
    const isRt = function (p) { return cdir(p).kind === 'runtime'; };
    const rows = [];
    if (dir) rows.push(row(hashFor('code', parentOf(dir)), icon('dir', '#a89fa6'), el('span', 'arch-rn', '..'), '', ''));
    kids.filter(function (p) { return dir || !isRt(p); }).forEach(function (p) { rows.push(dirRow(p)); });
    ((st.ix.dm[dir] || {}).goFiles || []).forEach(function (f) { rows.push(fileRow(dir, f, false)); });
    const rt = dir ? [] : kids.filter(isRt);
    if (rt.length) runtimeRows(rt, rows);
    return rows;
  }
  function searchRows(q) {
    const rows = [];
    st.ix.paths.forEach(function (p) { if (p.toLowerCase().indexOf(q) >= 0 || (cdir(p).purpose || '').toLowerCase().indexOf(q) >= 0) rows.push(dirRow(p, p)); });
    scanDirs().forEach(function (d) {
      (d.goFiles || []).forEach(function (f) { if ((d.path + '/' + f.name).toLowerCase().indexOf(q) >= 0) rows.push(fileRow(d.path, f, true)); });
    });
    return rows.length > MAXROWS ? rows.slice(0, MAXROWS).concat([empty('+' + (rows.length - MAXROWS) + ' more; narrow the filter.')]) : rows;
  }
  function mentions(text, path) {
    const re = new RegExp(path.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '(?![\\w-])');
    return re.test(text || '');
  }
  function linkedIssues(dir, c) {
    return (curated().findings || []).filter(function (f) {
      const txt = f.title + ' ' + f.evidence + ' ' + f.fix;
      return (dir.indexOf('/') >= 0 && mentions(txt, dir)) || (c.proposal || []).some(function (p) { return mentions(txt, p); });
    });
  }
  function statLine(d) {
    const parts = ['package ' + (d.package || '?'), 'layer ' + d.layer, d.files + ' files', num(d.loc) + ' lines',
      'imported by ' + d.importedBy.length, 'imports ' + d.imports.length];
    return el('p', 'muted', parts.join(' · ') + (d.hasDoc ? '' : ' · no package doc'));
  }
  function dirReadme(dir) {
    const c = cdir(dir);
    const d = st.ix.dm[dir];
    const tags = el('div', 'arch-chips', null, [c.group ? badge(c.group, '', groupColor(dir)) : null, c.kind ? badge(c.kind) : null, presence(dir)]);
    return readme([el('p', '', c.purpose || 'No curated description.'), tags, d && d.files ? statLine(d) : null,
      field('Add here', c.addHere), field('Not here', c.notHere),
      d ? chipField('Imports', d.imports) : null, d ? chipField('Imported by', d.importedBy) : null,
      d && d.external.length ? field('External', d.external.join(', ')) : null,
      d ? chipField('Test-only imports', d.testImports) : null,
      chipField('Proposals', c.proposal, function (p) { return chip('pulls', p, p); }),
      chipField('Issues', linkedIssues(dir, c), function (f) { return chip('issues', f.id, f.id + ' ' + f.title); })]);
  }
  function rootReadme() {
    const c = curated();
    const meta = [c.audited && 'audited ' + c.audited, c.commit && 'commit ' + c.commit].filter(Boolean).join(' · ');
    const rules = (c.rules || []).map(function (r) { return el('div', 'card', null, [el('b', '', r.title || r.id), el('div', '', null, [rich(r.detail)])]); });
    return readme([el('p', '', null, [rich(c.summary || 'No summary recorded.')]), meta ? el('p', 'muted', meta) : null,
      rules.length ? section('Import rules', rules) : null]);
  }
  function renderCode() {
    const dir = st.route.rest;
    const input = el('input');
    input.placeholder = 'Go to file (path or purpose)';
    const table = el('div', 'arch-list');
    const fill = function () {
      const q = input.value.trim().toLowerCase();
      table.textContent = '';
      (q ? searchRows(q) : browseRows(dir)).forEach(function (r) { table.appendChild(r); });
    };
    input.addEventListener('input', fill);
    fill();
    const segs = dir ? dir.split('/').map(function (s, i, a) { return { label: s, rest: a.slice(0, i + 1).join('/') }; }) : [];
    const bad = dir && !st.ix.set[dir];
    return [crumbs('code', segs), input, bad ? empty('No such directory: ' + dir) : null, table,
      dir ? dirReadme(dir) : rootReadme()];
  }

  // ---- Issues and pull requests ----
  function sortedFindings() {
    return (curated().findings || []).slice().sort(function (a, b) { return SEV.indexOf(a.severity) - SEV.indexOf(b.severity) || (a.id < b.id ? -1 : 1); });
  }
  function sortedProps() {
    return (curated().proposals || []).slice().sort(function (a, b) { return (a.order || 0) - (b.order || 0); });
  }
  function labelRow(kids) { return el('span', 'arch-labels', null, kids); }
  function labels(f) { return labelRow([badge(f.severity, 'sev-' + f.severity), f.area ? areaLabel(f.area) : null]); }
  function listRow(href, ico, title, sub, lbls) {
    const main = el('span', 'arch-im', null, [el('span', 'arch-it', title),
      el('span', 'arch-is', null, [el('span', 'muted', sub), lbls])]);
    const r = el('a', 'arch-irow', null, [ico, main]);
    r.href = href;
    return r;
  }
  function pillRow(items, cur, onpick) {
    return el('div', 'arch-chips', null, items.map(function (it) {
      const b = el('button', 'arch-pill' + (cur === it[0] ? ' on' : ''), it[1]);
      b.type = 'button';
      b.addEventListener('click', function () { onpick(cur === it[0] ? '' : it[0]); });
      return b;
    }));
  }
  function issueMatches(f, q) {
    const hay = (f.id + ' ' + f.title + ' ' + f.area + ' ' + f.evidence + ' ' + f.fix).toLowerCase();
    return (!st.sev || f.severity === st.sev) && (!st.area || f.area === st.area) && (!q || hay.indexOf(q) >= 0);
  }
  function renderIssues() {
    if (st.route.rest) return issueDetail(st.route.rest);
    const all = sortedFindings();
    const input = el('input');
    input.placeholder = 'Search issues';
    input.value = st.q;
    const head = el('div', 'arch-listhead');
    const pills = el('div');
    const list = el('div', 'arch-list');
    const fill = function () {
      st.q = input.value;
      const q = st.q.trim().toLowerCase();
      const shown = all.filter(function (f) { return issueMatches(f, q); });
      head.textContent = shown.length + ' Open';
      list.textContent = '';
      shown.forEach(function (f) {
        list.appendChild(listRow(hashFor('issues', f.id), icon('issue', SEVCOL[f.severity]), f.title,
          f.id + (f.area ? ' · ' + f.area : ''), labels(f)));
      });
      if (!shown.length) list.appendChild(empty('No issues match.'));
    };
    const drawPills = function () {
      const areas = {};
      all.forEach(function (f) { if (f.area) areas[f.area] = true; });
      pills.textContent = '';
      pills.appendChild(pillRow(SEV.filter(function (s) { return all.some(function (f) { return f.severity === s; }); })
        .map(function (s) { return [s, s]; }), st.sev, function (v) { st.sev = v; drawPills(); fill(); }));
      pills.appendChild(pillRow(Object.keys(areas).sort().map(function (a) { return [a, a]; }), st.area,
        function (v) { st.area = v; drawPills(); fill(); }));
    };
    input.addEventListener('input', fill);
    drawPills();
    fill();
    return [input, pills, head, list];
  }
  function backLink(sub, text) { return el('p', '', null, [link(sub, '', '← ' + text)]); }
  function issueDetail(id) {
    const f = st.ix.finds[id];
    if (!f) return [backLink('issues', 'Issues'), empty('No such issue: ' + id)];
    return [backLink('issues', 'Issues'), el('h2', 'arch-title', null, [icon('issue', SEVCOL[f.severity]),
      document.createTextNode(' ' + f.title), el('span', 'muted', ' ' + f.id)]), labels(f),
    section('Evidence', [el('p', '', null, [rich(f.evidence || 'None recorded.')])]),
    section('Fix', [el('p', '', null, [rich(f.fix || 'None recorded.')])])];
  }
  function moveBlock(moves) {
    const box = el('div', 'arch-diff');
    box.appendChild(el('div', 'arch-diff-h', 'Files changed ' + moves.length));
    moves.forEach(function (m) {
      if (typeof m === 'string') { box.appendChild(el('div', 'arch-ln', m)); return; }
      box.appendChild(el('div', 'arch-ln del', '- ' + m.from));
      box.appendChild(el('div', 'arch-ln add', '+ ' + m.to));
    });
    return box;
  }
  function orderHint(p, total) {
    if (!p.order) return null;
    return el('p', 'muted', p.order === 1 ? 'Depends on nothing.' : 'Position ' + p.order + ' of ' + total + '.');
  }
  function renderPulls() {
    const props = sortedProps();
    if (st.route.rest) return pullDetail(st.route.rest, props.length);
    if (!props.length) return [empty('No proposals.')];
    return [el('div', 'arch-listhead', props.length + ' Open'), el('div', 'arch-list', null, props.map(function (p) { return listRow(hashFor('pulls', p.id), icon('pr', '#8ae234'), '#' + p.order + ' ' + p.title, p.id, propLabels(p)); }))];
  }
  function propLabels(p) {
    return labelRow([p.effort ? badge('effort ' + p.effort) : null, p.risk ? badge('risk ' + p.risk, RISK[p.risk] || '') : null]);
  }
  function pullDetail(id, total) {
    const p = st.ix.props[id];
    if (!p) return [backLink('pulls', 'Pull requests'), empty('No such proposal: ' + id)];
    const dirs = Object.keys(curated().dirs || {}).filter(function (d) { return (cdir(d).proposal || []).indexOf(id) >= 0; });
    return [backLink('pulls', 'Pull requests'), el('h2', 'arch-title', null, [icon('pr', '#8ae234'),
      document.createTextNode(' #' + p.order + ' ' + p.title), el('span', 'muted', ' ' + p.id)]), propLabels(p), orderHint(p, total),
    section('Why', [el('p', '', null, [rich(p.why)])]), moveBlock(p.moves || []),
    section('Cycle impact', [el('p', '', null, [rich(p.cycles || 'None recorded.')])]),
    dirs.length ? section('Touches', [dirChips(dirs)]) : null];
  }

  // ---- Commands ----
  function verbGroups() {
    const order = [];
    const by = {};
    ((st.data && st.data.verbs) || []).forEach(function (v) {
      const g = v.group || 'HIDDEN';
      if (!by[g]) { by[g] = []; order.push(g); }
      by[g].push(v);
    });
    return { order: order.filter(function (g) { return g !== 'HIDDEN'; }).concat(by.HIDDEN ? ['HIDDEN'] : []), by: by };
  }
  function cmdRow(href, isDir, name, desc, meta) {
    return row(href, icon(isDir ? 'dir' : 'file', isDir ? '#729fcf' : '#a89fa6'), el('span', 'arch-rn', name), desc, meta);
  }
  function plural(n, w) { return n + ' ' + w + (n === 1 ? '' : 's'); }
  function cmdRoot(vg) {
    const rows = vg.order.map(function (g) { return cmdRow(hashFor('commands', 'group/' + g), true, g, '', plural(vg.by[g].length, 'verb')); });
    return [crumbs('commands', []), el('div', 'arch-list', null, rows)];
  }
  function verbRow(v) {
    const n = (v.subverbs || []).length;
    return cmdRow(hashFor('commands', v.name), n > 0, v.name, v.summary, n ? plural(n, 'subverb') : plural((v.flags || []).length, 'flag'));
  }
  function cmdGroup(vg, g) {
    const vs = vg.by[g] || [];
    return [crumbs('commands', [{ label: g, rest: 'group/' + g }]), el('div', 'arch-list', null, vs.map(verbRow)),
      readme([el('p', '', g + ': ' + plural(vs.length, 'verb') + ', ' + vs.filter(function (v) { return v.hidden; }).length + ' hidden.')])];
  }
  function verbCard(v, sub) {
    const own = (curated().verbs || {})[v.name] || {};
    const flags = sub ? sub.flags : v.flags;
    return readme([el('p', '', (sub ? v.name + ' ' + sub.name : v.summary) || ''),
      v.hidden ? el('div', 'arch-chips', null, [badge('hidden')]) : null,
      sub ? null : chipField('Owner', own.owner), sub ? null : field('Note', own.note),
      (flags || []).length ? el('div', 'arch-sub', null, [el('b', '', 'Flags'), flagList(flags)]) : empty('No flags.')]);
  }
  function cmdVerb(vg, name, subName) {
    const all = (st.data && st.data.verbs) || [];
    const v = all.find(function (x) { return x.name === name; });
    if (!v) return [crumbs('commands', []), empty('No such verb: ' + name)];
    const segs = [{ label: v.group || 'HIDDEN', rest: 'group/' + (v.group || 'HIDDEN') }, { label: v.name, rest: v.name }];
    const sub = (v.subverbs || []).find(function (s) { return s.name === subName; });
    if (sub) return [crumbs('commands', segs.concat([{ label: sub.name, rest: v.name + '/' + sub.name }])), verbCard(v, sub)];
    const rows = (v.subverbs || []).map(function (s) { return cmdRow(hashFor('commands', v.name + '/' + s.name), false, s.name, '', plural((s.flags || []).length, 'flag')); });
    return [crumbs('commands', segs), rows.length ? el('div', 'arch-list', null, rows) : null, verbCard(v, null)];
  }
  function renderCommands() {
    const vg = verbGroups();
    if (!vg.order.length) return [empty('No verb tree available.')];
    const rest = st.route.rest;
    if (!rest) return cmdRoot(vg);
    if (rest.indexOf('group/') === 0) return cmdGroup(vg, rest.slice(6));
    const parts = rest.split('/');
    return cmdVerb(vg, parts[0], parts[1]);
  }

  // ---- Insights ----
  function tile(label, value, color) {
    const t = el('div', 'arch-tile', null, [el('b', '', String(value)), el('span', 'muted', label)]);
    if (color) t.style.borderTopColor = color;
    return t;
  }
  function tiles() {
    const pk = scanDirs().filter(function (d) { return d.files > 0; });
    const loc = pk.reduce(function (a, d) { return a + d.loc; }, 0);
    const maxL = pk.reduce(function (a, d) { return Math.max(a, d.layer); }, 0);
    const t = [tile('packages', pk.length), tile('Go lines', num(loc)), tile('max layer', 'L' + maxL)];
    SEV.forEach(function (s) {
      const n = (curated().findings || []).filter(function (f) { return f.severity === s; }).length;
      if (n) t.push(tile(s + ' issues', n, SEVCOL[s]));
    });
    t.push(tile('proposals', (curated().proposals || []).length));
    return el('div', 'arch-tiles', null, t);
  }
  function layerBands() {
    const by = {};
    scanDirs().filter(function (d) { return d.files > 0; }).forEach(function (d) { (by[d.layer] = by[d.layer] || []).push(d.path); });
    return Object.keys(by).map(Number).sort(function (a, b) { return a - b; }).map(function (l) { return el('div', 'arch-band', null, [el('b', '', 'L' + l + ' (' + by[l].length + ')'), dirChips(by[l])]); });
  }
  function legend() {
    return (curated().groups || []).map(function (g) {
      const n = Object.keys(curated().dirs || {}).filter(function (p) { return cdir(p).group === g.id; }).length;
      const sw = el('span', 'arch-sw');
      sw.style.background = g.color;
      return el('div', 'arch-field', null, [sw, el('b', '', g.name + ' '), el('span', 'muted', '(' + n + ') '), rich(g.detail)]);
    });
  }
  function seamCell(label, kids) {
    const td = el('td', '', null, kids);
    td.dataset.label = label;
    return td;
  }
  function seamsTable() {
    const seams = curated().seams || [];
    if (!seams.length) return empty('No workarounds recorded.');
    const head = el('tr', '', null, ['Site', 'Kind', 'Wired by', 'Why', 'Removal'].map(function (h) { return el('th', '', h); }));
    const rows = seams.map(function (s) {
      return el('tr', '', null, [seamCell('Site', [el('span', 'arch-path', s.site)]), seamCell('Kind', [badge(s.kind || 'seam')]),
        seamCell('Wired by', [rich(s.wiredBy)]), seamCell('Why', [rich(s.why)]), seamCell('Removal', [rich(s.removal)])]);
    });
    return el('table', 'arch-seams', null, [el('thead', '', null, [head]), el('tbody', '', null, rows)]);
  }
  function renderInsights() {
    return [tiles(), section('Layers', layerBands()), section('Groups', legend()),
      section('Cycle workarounds', [seamsTable()])];
  }

  // ---- Import check ----
  function findCycle(dm, from, to) {
    const prev = {};
    const q = [from];
    prev[from] = null;
    while (q.length) {
      const c = q.shift();
      if (c === to) break;
      ((dm[c] || {}).imports || []).forEach(function (n) { if (!(n in prev)) { prev[n] = c; q.push(n); } });
    }
    const out = [];
    if (!(to in prev)) return null;
    for (let n = to; n !== null; n = prev[n]) out.unshift(n);
    return out;
  }
  function safeVerdict(importer, imported) {
    const dm = st.ix.dm;
    const kids = [badge('safe', 'sev-low')];
    if ((dm[imported] || {}).layer > (dm[importer] || {}).layer) {
      kids.push(el('p', '', 'Warning: ' + imported + ' (L' + dm[imported].layer + ') sits above ' + importer +
        ' (L' + dm[importer].layer + '); this would invert the layering.'));
    }
    if (GUARDED.indexOf(imported) >= 0) {
      (curated().rules || []).forEach(function (r) { kids.push(el('div', 'card', null, [el('b', '', r.title || r.id), el('div', '', null, [rich(r.detail)])])); });
    }
    return el('div', '', null, kids);
  }
  function checkResult(importer, imported) {
    const dm = st.ix.dm;
    if (!importer || !imported) return empty('Pick both packages.');
    if (importer === imported) return empty('Same package.');
    if (((dm[importer] || {}).imports || []).indexOf(imported) >= 0) return badge('already imports', 'sev-low');
    const path = findCycle(dm, imported, importer);
    if (path) return el('div', 'card', 'CYCLE: ' + path.concat([imported]).join(' → '));
    return safeVerdict(importer, imported);
  }
  // A filter box plus a plain dropdown whose options are rebuilt to the matches.
  function filterSelect(label, pkgs, onchange) {
    const s = el('select');
    const q = el('input');
    q.placeholder = 'filter ' + label.toLowerCase();
    const fill = function () {
      const cur = s.value;
      const v = q.value.trim().toLowerCase();
      s.textContent = '';
      s.appendChild(el('option', '', '(pick)'));
      s.firstChild.value = '';
      pkgs.filter(function (p) { return p === cur || p.toLowerCase().indexOf(v) >= 0; }).forEach(function (p) {
        const o = el('option', '', p);
        o.value = p;
        s.appendChild(o);
      });
      s.value = cur;
    };
    q.addEventListener('input', fill);
    s.addEventListener('change', function () { onchange(); });
    fill();
    return el('label', '', label, [q, s]);
  }
  function renderCheck() {
    const pkgs = scanDirs().filter(function (d) { return d.files > 0; }).map(function (d) { return d.path; });
    const out = el('div', 'arch-list');
    let a;
    let b;
    const upd = function () {
      out.textContent = '';
      out.appendChild(checkResult(a.querySelector('select').value, b.querySelector('select').value));
    };
    a = filterSelect('Importer', pkgs, upd);
    b = filterSelect('Imported', pkgs, upd);
    upd();
    return [el('div', 'arch-pair', null, [a, b]), out];
  }

  // ---- Shell ----
  const RENDER = { code: renderCode, issues: renderIssues, pulls: renderPulls, commands: renderCommands,
    insights: renderInsights, check: renderCheck };
  function buildTabs() {
    const counts = { issues: (curated().findings || []).length, pulls: (curated().proposals || []).length,
      commands: ((st.data && st.data.verbs) || []).length };
    const names = { code: 'Code', issues: 'Issues', pulls: 'Pull requests', commands: 'Commands', insights: 'Insights', check: 'Import check' };
    tabBar = el('div', 'arch-subtabs');
    SUBS.forEach(function (k) {
      const b = el('button', '', names[k]);
      b.type = 'button';
      b.dataset.sub = k;
      if (counts[k] != null) b.appendChild(el('span', 'arch-count', String(counts[k])));
      b.addEventListener('click', function () { go(k, ''); });
      tabBar.appendChild(b);
    });
    return tabBar;
  }
  function render() {
    if (!st.data || !body) return;
    tabBar.querySelectorAll('button').forEach(function (b) { b.classList.toggle('active', b.dataset.sub === st.route.sub); });
    body.textContent = '';
    RENDER[st.route.sub]().forEach(function (k) { if (k) body.appendChild(k); });
  }
  function build() {
    prepare();
    body = el('div', 'arch-body');
    pane.textContent = '';
    [buildTabs(), scan().error ? el('p', 'card', 'Source scan unavailable: ' + scan().error + ' (curated notes only).') : null, body]
      .forEach(function (k) { if (k) pane.appendChild(k); });
    st.route = parseHash();
    render();
  }
  window.addEventListener('hashchange', function () {
    if (!/^#arch/.test(location.hash)) return;
    if (pane.style.display === 'none') document.querySelector('#tabs [data-tab="architecture"]').click();
    st.route = parseHash();
    render();
  });

  pane.textContent = 'loading…';
  fetch('/api/architecture', { cache: 'no-store' }).then(function (r) {
    if (!r.ok) throw new Error('HTTP ' + r.status);
    return r.json();
  }).then(function (j) { st.data = j; build(); }).catch(function (e) { pane.textContent = 'architecture failed to load: ' + e.message; });
})();
