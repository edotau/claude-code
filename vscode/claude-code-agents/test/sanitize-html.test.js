'use strict';
// Unit tests for lib/sanitize-html.js: allowlist/strip behaviour, attribute scrubbing, url safety, textOf.
const test = require('node:test');
const assert = require('node:assert/strict');
const { sanitizeHtml, textOf } = require('../lib/sanitize-html');

// -- allowlisted tags kept --

test('sanitizeHtml: keeps common allowlisted tags (p, strong, em, ul/li, code, pre, blockquote, h1-h6)', () => {
  const html = '<p>Hello <strong>bold</strong> <em>em</em></p><ul><li>one</li></ul><code>x</code><pre>y</pre><blockquote>q</blockquote><h1>H</h1>';
  const out = sanitizeHtml(html, {});
  assert.match(out, /<p>Hello <strong>bold<\/strong> <em>em<\/em><\/p>/);
  assert.match(out, /<ul><li>one<\/li><\/ul>/);
  assert.match(out, /<code>x<\/code>/);
  assert.match(out, /<pre>y<\/pre>/);
  assert.match(out, /<blockquote>q<\/blockquote>/);
  assert.match(out, /<h1>H<\/h1>/);
});

test('sanitizeHtml: keeps table parts, span and div', () => {
  const html = '<table><thead><tr><th>A</th></tr></thead><tbody><tr><td>B</td></tr></tbody></table><span>s</span><div>d</div>';
  const out = sanitizeHtml(html, {});
  assert.match(out, /<table><thead><tr><th>A<\/th><\/tr><\/thead><tbody><tr><td>B<\/td><\/tr><\/tbody><\/table>/);
  assert.match(out, /<span>s<\/span>/);
  assert.match(out, /<div>d<\/div>/);
});

// -- unknown tags unwrapped --

test('sanitizeHtml: unwraps an unknown tag, keeping its text', () => {
  const out = sanitizeHtml('<p>before <unknowntag>middle</unknowntag> after</p>', {});
  assert.equal(out, '<p>before middle after</p>');
});

test('sanitizeHtml: unwraps a self-closing unknown tag', () => {
  const out = sanitizeHtml('<p>a<weird-tag/>b</p>', {});
  assert.equal(out, '<p>ab</p>');
});

// -- dangerous tags removed with content --

test('sanitizeHtml: removes <script> and its content entirely', () => {
  const out = sanitizeHtml('<p>safe</p><script>alert(1)</script><p>also safe</p>', {});
  assert.equal(out.includes('alert'), false);
  assert.equal(out.includes('<script'), false);
  assert.equal(out, '<p>safe</p><p>also safe</p>');
});

test('sanitizeHtml: removes <style>, <iframe>, <object>, <svg>, <form>, <input> with content', () => {
  const html =
    '<style>.x{color:red}</style>' +
    '<iframe src="https://evil.example">x</iframe>' +
    '<object data="x">y</object>' +
    '<svg><circle/></svg>' +
    '<form><input type="text"/></form>' +
    '<p>kept</p>';
  const out = sanitizeHtml(html, {});
  assert.equal(out, '<p>kept</p>');
});

// -- comments dropped --

test('sanitizeHtml: drops HTML comments entirely', () => {
  const out = sanitizeHtml('<p>a</p><!-- a comment with <script>alert(1)</script> inside --><p>b</p>', {});
  assert.equal(out, '<p>a</p><p>b</p>');
});

// -- attribute scrubbing --

test('sanitizeHtml: keeps only href/title on <a>, drops everything else', () => {
  const out = sanitizeHtml('<a href="https://example.com" title="t" onclick="evil()" class="c" id="i" style="color:red">link</a>', {});
  assert.match(out, /^<a href="https:\/\/example\.com" title="t" rel="noopener noreferrer" target="_blank">link<\/a>$/);
  assert.equal(out.includes('onclick'), false);
  assert.equal(out.includes('class='), false);
  assert.equal(out.includes('id='), false);
  assert.equal(out.includes('style='), false);
});

test('sanitizeHtml: keeps only src/alt/title/width/height on <img>, drops other attrs', () => {
  const out = sanitizeHtml('<img src="https://example.com/x.png" alt="a" title="t" width="10" height="20" onerror="evil()" class="c">', {});
  assert.match(out, /^<img src="https:\/\/example\.com\/x\.png" alt="a" title="t" width="10" height="20">$/);
  assert.equal(out.includes('onerror'), false);
  assert.equal(out.includes('class='), false);
});

test('sanitizeHtml: keeps only colspan/rowspan on td/th', () => {
  const out = sanitizeHtml('<table><tr><td colspan="2" rowspan="3" onclick="evil()" class="c">x</td></tr></table>', {});
  assert.match(out, /<td colspan="2" rowspan="3">x<\/td>/);
  assert.equal(out.includes('onclick'), false);
});

test('sanitizeHtml: every <a> gets rel=noopener noreferrer and target=_blank, even without existing attrs', () => {
  const out = sanitizeHtml('<a>bare</a>', {});
  assert.equal(out, '<a rel="noopener noreferrer" target="_blank">bare</a>');
});

// -- url safety: href/src must be http(s)/mailto/relative --

test('sanitizeHtml: drops javascript: href', () => {
  const out = sanitizeHtml('<a href="javascript:alert(1)">bad</a>', {});
  assert.equal(out.includes('javascript:'), false);
  assert.equal(out, '<a rel="noopener noreferrer" target="_blank">bad</a>');
});

test('sanitizeHtml: drops data: src by default, allows with allowDataImages', () => {
  const dataUrl = 'data:image/png;base64,AAAA';
  const withoutFlag = sanitizeHtml(`<img src="${dataUrl}">`, {});
  assert.equal(withoutFlag.includes('data:'), false);
  const withFlag = sanitizeHtml(`<img src="${dataUrl}">`, { allowDataImages: true });
  assert.match(withFlag, /src="data:image\/png;base64,AAAA"/);
});

test('sanitizeHtml: allows mailto: href', () => {
  const out = sanitizeHtml('<a href="mailto:a@b.com">mail</a>', {});
  assert.match(out, /href="mailto:a@b\.com"/);
});

test('sanitizeHtml: allows relative /path and #fragment hrefs', () => {
  const out1 = sanitizeHtml('<a href="/foo/bar">rel</a>', {});
  assert.match(out1, /href="\/foo\/bar"/);
  const out2 = sanitizeHtml('<a href="#section">frag</a>', {});
  assert.match(out2, /href="#section"/);
});

test('sanitizeHtml: prefixes relative /path href with baseUrl when given', () => {
  const out = sanitizeHtml('<a href="/foo/bar">rel</a>', { baseUrl: 'https://example.com/' });
  assert.match(out, /href="https:\/\/example\.com\/foo\/bar"/);
});

// -- unclosed / orphan tags --

test('sanitizeHtml: closes unclosed allowed tags at the end', () => {
  const out = sanitizeHtml('<p>unclosed paragraph', {});
  assert.equal(out, '<p>unclosed paragraph</p>');
});

test('sanitizeHtml: ignores a closing tag for a tag that was never opened', () => {
  const out = sanitizeHtml('<p>text</div></p>', {});
  assert.equal(out, '<p>text</p>');
});

// -- textOf --

test('textOf: strips tags and truncates with an ellipsis at the given max length', () => {
  assert.equal(textOf('<p>Hello <b>world</b></p>', 8), 'Hello w…');
});

test('textOf: default max is 200 and returns full text when short', () => {
  assert.equal(textOf('<p>short text</p>'), 'short text');
});

test('textOf: collapses whitespace and handles empty/null input', () => {
  assert.equal(textOf('<p>a</p>\n\n<p>b</p>'), 'a b');
  assert.equal(textOf(null), '');
  assert.equal(textOf(''), '');
});

// -- teeth: no dangerous markup/attrs ever survive --

test('TEETH: onclick and javascript: never survive sanitizeHtml on a hostile payload', () => {
  const hostile =
    '<a href="javascript:alert(1)" onclick="alert(2)">x</a>' +
    '<img src="javascript:alert(3)" onerror="alert(4)">' +
    '<div onmouseover="alert(5)" style="background:url(javascript:alert(6))">y</div>' +
    '<script>alert(7)</script>';
  const out = sanitizeHtml(hostile, {});
  assert.equal(/javascript:/i.test(out), false);
  assert.equal(/onclick|onerror|onmouseover/i.test(out), false);
  assert.equal(/<script/i.test(out), false);
  assert.equal(/alert\(/.test(out), false);
});

test('safeUrl: protocol-relative //host URLs are dropped from href and src (teeth for the tracking-pixel case)', () => {
  const out = sanitizeHtml('<a href="//evil.example/x">a</a><img src="//evil.example/p.gif"><a href="/local">b</a>');
  assert.doesNotMatch(out, /evil\.example/);
  assert.match(out, /href="\/local"/);
});
