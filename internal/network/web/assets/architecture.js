// Architecture tab: a tree + detail browser over /api/architecture (curated notes, git file list, Go scan, CLI verbs).
(function () {
  'use strict';
  const MAXROWS = 1500;
  const MAXCOMMENT = 70;
  const NAMES = { dir: 'Directory', commands: 'Commands' };
  const st = { data: null, route: { sub: 'dir', rest: '' }, views: {}, clicked: '' };
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
  function num(n) { return n >= 1000 ? (n / 1000).toFixed(1) + 'k' : String(n); }
  function curated() { return (st.data && st.data.curated) || {}; }
  function scan() { return (st.data && st.data.scan) || {}; }
  function cdir(p) { return (curated().dirs || {})[p] || {}; }
  function hashFor(sub, rest) {
    return '#arch/' + sub + (rest ? '/' + rest.split('/').map(encodeURIComponent).join('/') : '');
  }
  function parseHash() {
    const m = /^#arch(?:\/([^/]*))?(?:\/(.*))?$/.exec(location.hash);
    if (!m || !NAMES[m[1]]) return { sub: 'dir', rest: '' };
    let rest = (m[2] || '').replace(/\/+$/, '');
    try { rest = rest.split('/').map(decodeURIComponent).join('/'); } catch (e) { /* keep the raw segments */ }
    return { sub: m[1], rest: rest };
  }
  function sentence(text) {
    const first = String(text || '').split(/(?<=[.!?])\s/)[0].trim();
    return first.length > MAXCOMMENT ? first.slice(0, MAXCOMMENT - 1) + '…' : first;
  }

  // ---- Models: nodes {key, name, label, dir, kids, parent, comment, hash} ----
  function node(par, key, name, dir) {
    return { key: key, name: name, label: name, dir: dir, kids: [], parent: par, comment: '', hash: '' };
  }
  function sortKids(list) {
    list.sort(function (a, b) { return (a.dir === b.dir ? 0 : a.dir ? -1 : 1) || (a.name < b.name ? -1 : 1); });
    list.forEach(function (n) { sortKids(n.kids); });
  }
  function dirModel() {
    const roots = [];
    const byKey = {};
    (scan().files || []).forEach(function (f) {
      const segs = f.path.split('/');
      let par = null;
      segs.forEach(function (s, i) {
        const key = segs.slice(0, i + 1).join('/');
        const leaf = i === segs.length - 1;
        if (!byKey[key]) {
          const n = node(par, key, s, !leaf);
          n.hash = hashFor('dir', key);
          if (leaf) n.lines = f.lines || 0;
          else { n.label = s + '/'; n.comment = sentence(cdir(key).purpose); }
          (par ? par.kids : roots).push(n);
          byKey[key] = n;
        }
        par = byKey[key];
      });
    });
    sortKids(roots);
    return { roots: roots, byKey: byKey, detail: dirDetail, home: dirHome };
  }
  function cmdModel() {
    const by = {};
    const order = [];
    ((st.data && st.data.verbs) || []).forEach(function (v) {
      const g = v.group || 'HIDDEN';
      if (!by[g]) { by[g] = []; order.push(g); }
      by[g].push(v);
    });
    const names = order.filter(function (g) { return g !== 'HIDDEN'; }).concat(by.HIDDEN ? ['HIDDEN'] : []);
    const byKey = {};
    const roots = names.map(function (g) {
      const gn = node(null, 'group/' + g, g, true);
      gn.verbs = by[g];
      byKey[gn.key] = gn;
      by[g].forEach(function (v) { gn.kids.push(verbNode(gn, v, byKey)); });
      return gn;
    });
    return { roots: roots, byKey: byKey, detail: cmdDetail, home: cmdHome };
  }
  function verbNode(par, v, byKey) {
    const vn = node(par, v.name, v.name, (v.subverbs || []).length > 0);
    vn.verb = v;
    vn.hash = hashFor('commands', v.name);
    vn.comment = sentence(v.summary);
    byKey[vn.key] = vn;
    (v.subverbs || []).forEach(function (s) {
      const sn = node(vn, v.name + '/' + s.name, s.name, false);
      sn.verb = v;
      sn.sub = s;
      sn.hash = hashFor('commands', sn.key);
      byKey[sn.key] = sn;
      vn.kids.push(sn);
    });
    return vn;
  }

  // ---- Detail panels ----
  function field(label, text) { return text ? el('div', 'arch-field', null, [el('b', '', label + ': '), document.createTextNode(text)]) : null; }
  function chip(hash, text, title) {
    const a = el('a', 'arch-chip', text);
    a.href = hash;
    if (title) a.title = title;
    return a;
  }
  function chipField(label, list) {
    if (!list || !list.length) return null;
    const chips = list.map(function (p) { return chip(hashFor('dir', p), p.replace(/^internal\//, ''), p); });
    return el('div', 'arch-field', null, [el('b', '', label + ':'), el('div', 'arch-chips', null, chips)]);
  }
  function statLine(d) {
    const parts = ['package ' + (d.package || '?'), 'layer ' + d.layer, d.files + ' files', num(d.loc) + ' lines',
      'imported by ' + d.importedBy.length, 'imports ' + d.imports.length];
    return el('p', 'muted', parts.join(' · ') + (d.hasDoc ? '' : ' · no package doc'));
  }
  function goDir(key) {
    return (scan().dirs || []).find(function (d) { return d.path === key; });
  }
  function dirDetail(n) {
    if (!n.dir) return fileDetail(n);
    const c = cdir(n.key);
    const d = goDir(n.key);
    return [el('h3', 'arch-title', n.key + '/'), el('p', '', c.purpose || 'No curated description.'), d ? statLine(d) : null,
      field('Add here', c.addHere), field('Not here', c.notHere),
      d ? chipField('Imports', d.imports) : null, d ? chipField('Imported by', d.importedBy) : null,
      d && d.external.length ? field('External', d.external.join(', ')) : null,
      d ? chipField('Test-only imports', d.testImports) : null];
  }
  function fileDetail(n) {
    const dir = n.parent ? n.parent.key : '';
    return [el('h3', 'arch-title', n.key), n.lines ? el('p', 'muted', n.lines + ' lines') : null,
      dir ? el('div', 'arch-field', null, [el('b', '', 'Directory: '), chip(hashFor('dir', dir), dir + '/')]) : null,
      dir && cdir(dir).purpose ? el('p', '', cdir(dir).purpose) : null];
  }
  function dirHome() {
    const c = curated();
    const meta = [c.audited && 'audited ' + c.audited, c.commit && 'commit ' + c.commit].filter(Boolean).join(' · ');
    const rules = (c.rules || []).map(function (r) {
      return el('div', 'arch-field', null, [el('b', '', (r.title || r.id) + '. '), document.createTextNode(r.detail || '')]);
    });
    return [el('p', '', c.summary || 'No summary recorded.'), meta ? el('p', 'muted', meta) : null,
      rules.length ? el('h4', '', 'Import rules') : null].concat(rules);
  }
  function flagList(flags) {
    if (!flags || !flags.length) return el('p', 'muted', 'No flags.');
    return el('div', 'arch-flags', null, flags.map(function (f) { return el('span', '', f); }));
  }
  function cmdDetail(n) {
    if (n.verbs) {
      const hidden = n.verbs.filter(function (v) { return v.hidden; }).length;
      return [el('h3', 'arch-title', n.name), el('p', '', n.verbs.length + ' verbs, ' + hidden + ' hidden.')];
    }
    const v = n.verb;
    const own = (curated().verbs || {})[v.name] || {};
    const owners = n.sub ? [] : (own.owner || []).map(function (p) { return chip(hashFor('dir', p), p.replace(/^internal\//, ''), p); });
    return [el('h3', 'arch-title', n.sub ? v.name + ' ' + n.sub.name : v.name), n.sub ? null : el('p', '', v.summary || ''),
      v.hidden ? el('span', 'arch-badge', 'hidden') : null,
      owners.length ? el('div', 'arch-field', null, [el('b', '', 'Owner:'), el('div', 'arch-chips', null, owners)]) : null,
      n.sub ? null : field('Note', own.note), flagList(n.sub ? n.sub.flags : v.flags)];
  }
  function cmdHome() {
    const roots = view('commands').model.roots;
    const total = roots.reduce(function (a, g) { return a + g.verbs.length; }, 0);
    return [el('p', '', total + ' verbs in ' + roots.length + ' groups. Pick one to see its owner and flags.')];
  }

  // ---- Tree + detail component, shared by both sub-tabs ----
  function hit(n, q, memo) {
    if (!memo.has(n)) {
      memo.set(n, (n.key + ' ' + n.comment).toLowerCase().indexOf(q) >= 0 || n.kids.some(function (k) { return hit(k, q, memo); }));
    }
    return memo.get(n);
  }
  function collect(v, list, prefix, q, memo, out) {
    const shown = q ? list.filter(function (n) { return hit(n, q, memo); }) : list;
    shown.forEach(function (n, i) {
      if (out.length > MAXROWS) return;
      const last = i === shown.length - 1;
      out.push({ n: n, text: prefix + (last ? '└── ' : '├── ') });
      if (n.dir && (q || v.open.has(n.key))) collect(v, n.kids, prefix + (last ? '    ' : '│   '), q, memo, out);
    });
  }
  function rowEl(v, r) {
    const n = r.n;
    const name = el('span', n.dir ? 'arch-d' : '', n.label);
    const row = el('div', 'arch-tl' + (v.sel === n.key ? ' sel' : ''), null,
      [el('span', 'arch-tn', null, [document.createTextNode(r.text), name]), n.comment ? el('span', 'arch-tc', '# ' + n.comment) : null]);
    row.dataset.key = n.key;
    row.tabIndex = 0;
    return row;
  }
  function paintTree(v) {
    const q = v.q.trim().toLowerCase();
    const out = [];
    collect(v, v.model.roots, '', q, new Map(), out);
    v.tree.textContent = '';
    if (!out.length) { v.tree.appendChild(el('p', 'muted', q ? 'Nothing matches.' : 'Nothing to list.')); return; }
    const capped = out.length > MAXROWS;
    const rows = capped ? out.slice(0, MAXROWS) : out;
    const w = rows.reduce(function (a, r) { return Math.max(a, r.text.length + r.n.label.length); }, 0);
    v.tree.style.setProperty('--nw', Math.min(w + 2, 48) + 'ch');
    rows.forEach(function (r) { v.tree.appendChild(rowEl(v, r)); });
    if (capped) v.tree.appendChild(el('p', 'muted', 'More rows hidden; narrow the filter.'));
  }
  function paintDetail(v) {
    v.detail.textContent = '';
    const n = v.model.byKey[v.sel];
    const kids = n ? v.model.detail(n) : v.missing ? [el('p', 'muted', 'No such entry: ' + v.missing)] : v.model.home();
    kids.forEach(function (k) { if (k) v.detail.appendChild(k); });
  }
  function paint(v, reveal) {
    paintTree(v);
    paintDetail(v);
    const sel = reveal && v.tree.querySelector('.arch-tl.sel');
    if (sel) v.tree.scrollTop = Math.max(0, sel.offsetTop - v.tree.clientHeight / 2);
  }
  function openAncestors(v, n) {
    for (let p = n.parent; p; p = p.parent) v.open.add(p.key);
  }
  function pick(v, key) {
    const n = v.model.byKey[key];
    if (!n) return;
    if (n.dir) { if (v.open.has(key)) v.open.delete(key); else v.open.add(key); }
    v.sel = key;
    v.missing = '';
    st.clicked = key;
    openAncestors(v, n);
    paint(v, false);
    if (n.hash && location.hash !== n.hash) location.hash = n.hash;
    if (window.matchMedia('(max-width: 700px)').matches) v.detail.scrollIntoView({ block: 'start' });
  }
  function makeView(sub) {
    const v = { sub: sub, model: sub === 'dir' ? dirModel() : cmdModel(), open: new Set(), sel: '', missing: '', q: '' };
    v.model.roots.forEach(function (n) { v.open.add(n.key); });
    const input = el('input');
    input.placeholder = 'Filter by path or comment';
    input.addEventListener('input', function () { v.q = input.value; paintTree(v); });
    v.tree = el('div', 'arch-tree');
    v.tree.addEventListener('click', function (e) {
      const r = e.target.closest('.arch-tl');
      if (r) pick(v, r.dataset.key);
    });
    v.tree.addEventListener('keydown', function (e) {
      const r = e.target.closest('.arch-tl');
      if (r && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); pick(v, r.dataset.key); }
    });
    v.detail = el('div', 'arch-detail');
    v.root = el('div', 'arch-split', null, [el('div', 'arch-left', null, [input, v.tree]), v.detail]);
    return v;
  }
  function view(sub) {
    if (!st.views[sub]) st.views[sub] = makeView(sub);
    return st.views[sub];
  }

  // ---- Shell ----
  function buildTabs() {
    tabBar = el('div', 'arch-subtabs');
    Object.keys(NAMES).forEach(function (k) {
      const b = el('button', '', NAMES[k]);
      b.type = 'button';
      b.dataset.sub = k;
      b.addEventListener('click', function () { location.hash = hashFor(k, ''); });
      tabBar.appendChild(b);
    });
    return tabBar;
  }
  function render() {
    if (!st.data || !body) return;
    const v = view(st.route.sub);
    const n = v.model.byKey[st.route.rest];
    v.sel = n ? n.key : '';
    v.missing = !n && st.route.rest ? st.route.rest : '';
    if (n) openAncestors(v, n);
    tabBar.querySelectorAll('button').forEach(function (b) { b.classList.toggle('active', b.dataset.sub === v.sub); });
    body.textContent = '';
    body.appendChild(v.root);
    paint(v, st.clicked !== v.sel);
    st.clicked = '';
  }
  function build() {
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
