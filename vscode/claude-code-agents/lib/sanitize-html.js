'use strict';
// Allowlist HTML sanitiser for provider-rendered bodies (Jira `renderedFields` / `renderedBody`) shown in the webview.
// The webview CSP already blocks scripts and inline handlers; this keeps the markup to inert, styleable tags.

const ALLOWED_TAGS = new Set([
  'p', 'br', 'hr', 'a', 'b', 'strong', 'i', 'em', 'u', 's', 'del', 'ins', 'sup', 'sub', 'code', 'pre', 'tt', 'kbd', 'blockquote',
  'ul', 'ol', 'li', 'dl', 'dt', 'dd', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'table', 'thead', 'tbody', 'tfoot', 'tr', 'th', 'td',
  'img', 'span', 'div', 'cite', 'q', 'small', 'mark',
]);
const DROP_WITH_CONTENT = new Set(['script', 'style', 'iframe', 'object', 'embed', 'noscript', 'template', 'svg', 'math', 'textarea', 'select', 'button', 'form', 'input']);
const ALLOWED_ATTRS = { a: ['href', 'title'], img: ['src', 'alt', 'title', 'width', 'height'], td: ['colspan', 'rowspan'], th: ['colspan', 'rowspan'] };
const VOID = new Set(['br', 'hr', 'img']);

function safeUrl(value, { allowData = false } = {}) {
  const v = String(value || '').trim().replace(/[\u0000-\u001f]/g, '');
  if (/^(https?:|mailto:)/i.test(v) || /^(\/|#)/.test(v)) return v;
  if (allowData && /^data:image\/(png|jpe?g|gif|webp);base64,/i.test(v)) return v;
  return null;
}

function parseAttrs(raw) {
  const out = {};
  const re = /([^\s"'=<>/]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>`]+)))?/g;
  let m;
  while ((m = re.exec(raw))) out[m[1].toLowerCase()] = m[2] != null ? m[2] : m[3] != null ? m[3] : m[4] != null ? m[4] : '';
  return out;
}

function escapeAttr(value) {
  return String(value).replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

/** Rebuilds `html` with only allowlisted tags/attributes; unknown tags are unwrapped, dangerous ones removed with content. */
function sanitizeHtml(html, opts) {
  const { baseUrl = '', allowDataImages = false } = opts || {};
  const src = String(html == null ? '' : html);
  const out = [];
  const open = [];
  const re = /<!--[\s\S]*?-->|<!\[CDATA\[[\s\S]*?\]\]>|<\/?([A-Za-z][A-Za-z0-9-]*)([^>]*)>/g;
  let last = 0;
  let m;
  let dropUntil = null;
  while ((m = re.exec(src))) {
    const text = src.slice(last, m.index);
    last = re.lastIndex;
    if (!dropUntil) out.push(text);
    if (!m[1]) continue; // comment / CDATA: dropped
    const tag = m[1].toLowerCase();
    const closing = m[0][1] === '/';
    if (dropUntil) {
      if (closing && tag === dropUntil) dropUntil = null;
      continue;
    }
    if (DROP_WITH_CONTENT.has(tag)) {
      if (!closing && !/\/\s*>$/.test(m[0])) dropUntil = tag;
      continue;
    }
    if (!ALLOWED_TAGS.has(tag)) continue; // unwrap: keep the text, drop the tag
    if (closing) {
      if (!VOID.has(tag) && open.lastIndexOf(tag) !== -1) {
        while (open.length) {
          const t = open.pop();
          out.push(`</${t}>`);
          if (t === tag) break;
        }
      }
      continue;
    }
    const attrs = parseAttrs(m[2] || '');
    const kept = [];
    for (const name of ALLOWED_ATTRS[tag] || []) {
      if (!(name in attrs)) continue;
      let value = attrs[name];
      if (name === 'href' || name === 'src') {
        value = safeUrl(value, { allowData: name === 'src' && allowDataImages });
        if (!value) continue;
        if (baseUrl && value.startsWith('/')) value = baseUrl.replace(/\/$/, '') + value;
      }
      kept.push(`${name}="${escapeAttr(value)}"`);
    }
    if (tag === 'a') kept.push('rel="noopener noreferrer"', 'target="_blank"');
    out.push(`<${tag}${kept.length ? ' ' + kept.join(' ') : ''}>`);
    if (!VOID.has(tag) && !/\/\s*>$/.test(m[0])) open.push(tag);
  }
  if (!dropUntil) out.push(src.slice(last));
  while (open.length) out.push(`</${open.pop()}>`);
  return out.join('');
}

/** Plain-text excerpt of an HTML string (tags stripped, entities left as-is), capped at `max` chars. */
function textOf(html, max = 200) {
  const t = String(html || '').replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim();
  return t.length > max ? `${t.slice(0, max - 1)}…` : t;
}

module.exports = { sanitizeHtml, textOf, ALLOWED_TAGS };
