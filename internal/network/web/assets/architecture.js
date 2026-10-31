// Architecture tab: renders /api/architecture (curated notes + live source scan + CLI verbs) into #architecturePane.
(function () {
  'use strict';
  const SEV = ['critical', 'high', 'medium', 'low'];
  const SUBTABS = [['overview', 'Overview'], ['dirs', 'Directories'], ['cli', 'CLI'], ['findings', 'Findings'],
    ['reorg', 'Reorg'], ['check', 'Import check']];
  const GUARDED = ['internal/cli', 'internal/launch', 'internal/configure', 'internal/hooks', 'internal/agent'];
  const st = { data: null, sub: 'overview', dirFilter: '', sev: '', area: '', focus: '' };
  const pane = document.getElementById('architecturePane');
  let body = null;

  function el(tag, cls, text, kids) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null && text !== '') n.textContent = text;
    (kids || []).forEach(function (k) { if (k) n.appendChild(k); });
    return n;
  }
  function badge(text, cls, color) {
    const b = el('span', 'arch-badge ' + (cls || ''), text);
    if (color) b.style.borderColor = color;
    return b;
  }
  function field(label, text) {
    return text ? el('div', 'arch-field', null, [el('b', '', label + ': '), el('span', '', text)]) : null;
  }
  function section(title, kids) {
    return el('div', 'arch-sec', null, [el('h3', '', title)].concat(kids));
  }
  function empty(msg) { return el('p', 'muted', msg); }

  function curated() { return (st.data && st.data.curated) || {}; }
  function scan() { return (st.data && st.data.scan) || {}; }
  function scanDirs() { return scan().dirs || []; }
  function dirMap() {
    const m = {};
    scanDirs().forEach(function (d) { m[d.path] = d; });
    return m;
  }
  function groupColor(path) {
    const cd = (curated().dirs || {})[path];
    const g = cd && (curated().groups || []).find(function (x) { return x.id === cd.group; });
    return g ? g.color : '';
  }
  function openDir(path) { st.focus = path; st.dirFilter = ''; showSub('dirs'); }

  function chip(path) {
    const c = el('button', 'arch-chip', path.replace(/^internal\//, ''));
    c.type = 'button';
    c.title = path;
    const col = groupColor(path);
    if (col) c.style.borderColor = col;
    c.addEventListener('click', function () { openDir(path); });
    return c;
  }
  function chips(list) {
    return el('div', 'arch-chips', null, (list || []).map(chip));
  }
  function flagList(list) {
    return el('div', 'arch-flags', null, (list || []).map(function (f) { return el('span', '', f); }));
  }

  function countBy(items, key) {
    const m = {};
    (items || []).forEach(function (i) { m[i[key]] = (m[i[key]] || 0) + 1; });
    return m;
  }
  function overviewCounts() {
    const pk = scanDirs().filter(function (d) { return d.files > 0; });
    const loc = pk.reduce(function (a, d) { return a + d.loc; }, 0);
    const maxL = pk.reduce(function (a, d) { return Math.max(a, d.layer); }, 0);
    const sev = countBy(curated().findings, 'severity');
    const row = el('div', 'arch-chips');
    [['packages', pk.length], ['Go LOC', loc], ['max layer', maxL]].forEach(function (p) {
      row.appendChild(badge(p[0] + ' ' + p[1]));
    });
    SEV.forEach(function (s) { if (sev[s]) row.appendChild(badge(s + ' ' + sev[s], 'sev-' + s)); });
    return row;
  }
  function layerBands() {
    const by = {};
    scanDirs().filter(function (d) { return d.files > 0; }).forEach(function (d) {
      (by[d.layer] = by[d.layer] || []).push(d.path);
    });
    return Object.keys(by).map(Number).sort(function (a, b) { return a - b; }).map(function (l) {
      return el('div', 'arch-band', null, [el('b', '', 'L' + l + ' (' + by[l].length + ')'), chips(by[l])]);
    });
  }
  function seamsTable() {
    const seams = curated().seams || [];
    if (!seams.length) return empty('No seams recorded.');
    return el('div', '', null, seams.map(function (s) {
      return el('div', 'card', null, [el('b', 'arch-path', s.site), badge(s.kind || 'seam'),
        field('Wired by', s.wiredBy), field('Why', s.why), field('Removal', s.removal)]);
    }));
  }
  function renderOverview() {
    const c = curated();
    const rules = (c.rules || []).map(function (r) {
      return el('div', 'card', null, [el('b', '', r.title || r.id), el('div', '', r.detail)]);
    });
    const meta = [c.audited && 'audited ' + c.audited, c.commit && 'commit ' + c.commit].filter(Boolean).join(' · ');
    return [el('p', '', c.summary || ''), meta ? el('p', 'muted', meta) : null, overviewCounts(),
      section('Layers', layerBands()), section('Import rules', rules.length ? rules : [empty('No rules recorded.')]),
      section('Seams', [seamsTable()])];
  }

  function allPaths() {
    const set = {};
    const add = function (p) { if (p) set[p] = true; };
    scanDirs().forEach(function (d) { add(d.path); });
    (scan().top || []).forEach(add);
    (scan().tree || []).forEach(add);
    Object.keys(curated().dirs || {}).forEach(add);
    return Object.keys(set).sort();
  }
  function dirPresence(p, onDisk) {
    const c = (curated().dirs || {})[p];
    if (!c && onDisk) return badge('undescribed', 'sev-high');
    if (c && !onDisk) return badge('missing', 'sev-critical');
    return null;
  }
  function onDiskSet() {
    const s = {};
    scanDirs().forEach(function (d) { s[d.path] = true; });
    (scan().top || []).forEach(function (p) { s[p] = true; });
    (scan().tree || []).forEach(function (p) { s[p] = true; });
    return s;
  }
  function dirSummary(p, d, c, onDisk) {
    const sm = el('summary', 'arch-sum', null, [el('span', 'arch-path', p)]);
    if (c.purpose) sm.appendChild(el('span', 'arch-purpose', c.purpose));
    const bs = el('div', 'arch-chips');
    if (d && d.files) {
      bs.appendChild(badge('L' + d.layer));
      bs.appendChild(badge('in ' + d.importedBy.length));
      bs.appendChild(badge('out ' + d.imports.length));
      bs.appendChild(badge(d.loc + ' loc'));
    }
    if (c.group) bs.appendChild(badge(c.group, '', groupColor(p)));
    if (c.kind) bs.appendChild(badge(c.kind));
    const pres = dirPresence(p, onDisk);
    if (pres) bs.appendChild(pres);
    sm.appendChild(bs);
    return sm;
  }
  function dirDetail(p, d, c) {
    const kids = [field('Add here', c.addHere), field('Not here', c.notHere)];
    if (d) {
      if (d.package) kids.push(field('Package', d.package + (d.hasDoc ? '' : ' (no package doc)')));
      [['Imports', d.imports], ['Imported by', d.importedBy], ['Test-only imports', d.testImports]].forEach(function (x) {
        if (x[1].length) kids.push(el('div', '', null, [el('b', '', x[0] + ':'), chips(x[1])]));
      });
      if (d.external.length) kids.push(field('External', d.external.join(', ')));
    }
    if ((c.proposal || []).length) {
      kids.push(el('div', '', null, [el('b', '', 'Proposals: '), el('span', '', c.proposal.join(', '))]));
    }
    return el('div', 'arch-detail', null, kids.filter(Boolean));
  }
  function dirRow(p, dm, onDisk, q) {
    const d = dm[p];
    const c = (curated().dirs || {})[p] || {};
    const det = el('details', 'arch-dir', null, [dirSummary(p, d, c, !!onDisk[p]), dirDetail(p, d, c)]);
    const hit = q && (p.toLowerCase().indexOf(q) >= 0 || (c.purpose || '').toLowerCase().indexOf(q) >= 0);
    det.open = !!hit || st.focus === p;
    det.dataset.hit = hit ? '1' : '';
    if (st.focus === p) setTimeout(function () { det.scrollIntoView({ block: 'center' }); st.focus = ''; }, 0);
    return det;
  }
  function renderDirs() {
    const input = el('input');
    input.placeholder = 'filter by path or purpose';
    input.value = st.dirFilter;
    const list = el('div', 'arch-list');
    const fill = function () {
      const q = st.dirFilter.trim().toLowerCase();
      const dm = dirMap();
      const disk = onDiskSet();
      list.textContent = '';
      allPaths().forEach(function (p) {
        const row = dirRow(p, dm, disk, q);
        if (!q || row.dataset.hit) list.appendChild(row);
      });
    };
    input.addEventListener('input', function () { st.dirFilter = input.value; fill(); });
    fill();
    return [input, list];
  }

  function renderVerb(v, cur) {
    const own = (cur.verbs || {})[v.name] || {};
    const kids = [v.hidden ? badge('hidden') : null, field('Note', own.note)];
    if ((own.owner || []).length) kids.push(el('div', '', null, [el('b', '', 'Owner:'), chips(own.owner)]));
    (v.subverbs || []).forEach(function (s) {
      kids.push(el('div', 'arch-sub', null, [el('b', '', s.name), flagList(s.flags)]));
    });
    if ((v.flags || []).length) kids.push(el('div', 'arch-sub', null, [el('b', '', 'Flags'), flagList(v.flags)]));
    return el('details', 'arch-dir', null, [el('summary', 'arch-sum', null,
      [el('span', 'arch-path', v.name), el('span', 'arch-purpose', v.summary)]), el('div', 'arch-detail', null, kids)]);
  }
  function renderCLI() {
    const verbs = (st.data && st.data.verbs) || [];
    if (!verbs.length) return [empty('No verb tree available.')];
    const groups = [];
    const by = {};
    verbs.forEach(function (v) {
      const g = v.group || 'HIDDEN';
      if (!by[g]) { by[g] = []; groups.push(g); }
      by[g].push(v);
    });
    return groups.map(function (g) {
      return section(g, by[g].map(function (v) { return renderVerb(v, curated()); }));
    });
  }

  function select(label, opts, val, onchange) {
    const s = el('select');
    [''].concat(opts).forEach(function (o) {
      const op = el('option', '', o || 'all');
      op.value = o;
      s.appendChild(op);
    });
    s.value = val;
    s.addEventListener('change', function () { onchange(s.value); });
    return el('label', '', label, [s]);
  }
  function renderFindings() {
    const all = curated().findings || [];
    const areas = Object.keys(countBy(all, 'area')).sort();
    const list = el('div', 'arch-list');
    const shown = all.filter(function (f) {
      return (!st.sev || f.severity === st.sev) && (!st.area || f.area === st.area);
    });
    shown.forEach(function (f) {
      list.appendChild(el('div', 'card', null, [el('div', 'arch-chips', null,
        [badge(f.severity, 'sev-' + f.severity), badge(f.id), badge(f.area || '')]),
      el('b', '', f.title), field('Evidence', f.evidence), field('Fix', f.fix)]));
    });
    if (!shown.length) list.appendChild(empty('No findings.'));
    return [select('Severity', SEV, st.sev, function (v) { st.sev = v; showSub('findings'); }),
      select('Area', areas, st.area, function (v) { st.area = v; showSub('findings'); }), list];
  }

  function renderReorg() {
    const props = (curated().proposals || []).slice().sort(function (a, b) { return (a.order || 0) - (b.order || 0); });
    if (!props.length) return [empty('No proposals.')];
    return props.map(function (p) {
      const moves = (p.moves || []).map(function (m) {
        return el('li', 'arch-path', m.from + ' → ' + m.to);
      });
      return el('div', 'card', null, [el('div', 'arch-chips', null, [badge('#' + p.order), badge(p.id),
        p.effort ? badge('effort ' + p.effort) : null, p.risk ? badge('risk ' + p.risk) : null]),
      el('b', '', p.title), field('Why', p.why), field('Cycles', p.cycles), el('ul', '', null, moves)]);
    });
  }

  function findCycle(dm, from, to) {
    const prev = {};
    prev[from] = null;
    const q = [from];
    while (q.length) {
      const cur = q.shift();
      if (cur === to) break;
      ((dm[cur] || {}).imports || []).forEach(function (n) {
        if (!(n in prev)) { prev[n] = cur; q.push(n); }
      });
    }
    if (!(to in prev)) return null;
    const out = [];
    for (let n = to; n !== null; n = prev[n]) out.unshift(n);
    return out;
  }
  function checkResult(importer, imported) {
    const dm = dirMap();
    if (!importer || !imported) return empty('Pick both packages.');
    if (importer === imported) return empty('Same package.');
    if (((dm[importer] || {}).imports || []).indexOf(imported) >= 0) return badge('already imports', 'sev-low');
    const path = findCycle(dm, imported, importer);
    if (path) return el('div', 'card', 'CYCLE: ' + path.concat([imported]).join(' → '));
    const kids = [badge('safe', 'sev-low')];
    if ((dm[imported] || {}).layer > (dm[importer] || {}).layer) {
      kids.push(el('p', '', 'Warning: ' + imported + ' (L' + dm[imported].layer + ') sits above ' + importer +
        ' (L' + dm[importer].layer + '); this would invert the layering.'));
    }
    if (GUARDED.indexOf(imported) >= 0) {
      (curated().rules || []).forEach(function (r) {
        kids.push(el('div', 'card', null, [el('b', '', r.title || r.id), el('div', '', r.detail)]));
      });
    }
    return el('div', '', null, kids);
  }
  function searchSelect(label, pkgs, onchange) {
    const box = select(label, pkgs, '', onchange);
    const s = box.querySelector('select');
    const q = el('input');
    q.placeholder = 'search ' + label.toLowerCase();
    q.addEventListener('input', function () {
      const v = q.value.trim().toLowerCase();
      Array.prototype.forEach.call(s.options, function (o) { o.hidden = !!o.value && o.value.toLowerCase().indexOf(v) < 0; });
    });
    box.insertBefore(q, s);
    return s;
  }
  function renderCheck() {
    const pkgs = scanDirs().filter(function (d) { return d.files > 0; }).map(function (d) { return d.path; });
    const out = el('div', 'arch-list');
    let a, b;
    const upd = function () {
      out.textContent = '';
      out.appendChild(checkResult(a.value, b.value));
    };
    a = searchSelect('Importer', pkgs, upd);
    b = searchSelect('Imported', pkgs, upd);
    upd();
    return [a.parentNode, b.parentNode, out];
  }

  const RENDER = { overview: renderOverview, dirs: renderDirs, cli: renderCLI, findings: renderFindings,
    reorg: renderReorg, check: renderCheck };
  function showSub(name) {
    st.sub = name;
    pane.querySelectorAll('.arch-subtabs button').forEach(function (b) {
      b.classList.toggle('active', b.dataset.sub === name);
    });
    body.textContent = '';
    RENDER[name]().forEach(function (k) { if (k) body.appendChild(k); });
  }

  function build() {
    pane.textContent = '';
    const bar = el('div', 'arch-subtabs');
    SUBTABS.forEach(function (t) {
      const b = el('button', '', t[1]);
      b.type = 'button';
      b.dataset.sub = t[0];
      b.addEventListener('click', function () { showSub(t[0]); });
      bar.appendChild(b);
    });
    body = el('div', 'arch-body');
    pane.appendChild(bar);
    if (scan().error) pane.appendChild(el('p', 'card', 'Source scan unavailable: ' + scan().error + ' (curated notes only).'));
    pane.appendChild(body);
    showSub(st.sub);
  }

  pane.textContent = 'loading…';
  fetch('/api/architecture', { cache: 'no-store' }).then(function (r) {
    if (!r.ok) throw new Error('HTTP ' + r.status);
    return r.json();
  }).then(function (j) { st.data = j; build(); }).catch(function (e) {
    pane.textContent = 'architecture failed to load: ' + e.message;
  });
})();
