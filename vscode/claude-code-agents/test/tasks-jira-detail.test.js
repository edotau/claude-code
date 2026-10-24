'use strict';
// Unit tests for lib/tasks-jira.js detail-view helpers: classifyTransition, mapTransition, mapComment,
// mapDetail, branchName, prompt.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { classifyTransition, mapTransition, mapComment, mapDetail, branchName, prompt } = require('../lib/tasks-jira');

const FIXTURE_DIR = path.join(__dirname, 'fixtures', 'jira');
function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(FIXTURE_DIR, `${name}.json`), 'utf8'));
}

const BASE = 'https://jira.biscrum.com';
const ISSUES = fixture('search').issues;
const RAW_TRANSITIONS = fixture('transitions').transitions;

// -- classifyTransition --

test('classifyTransition: no required fields -> {kind: "none"}', () => {
  assert.deepEqual(classifyTransition({ fields: {} }), { kind: 'none' });
  assert.deepEqual(classifyTransition({}), { kind: 'none' });
});

test('classifyTransition: required resolution (with allowedValues) + required comment -> form', () => {
  const t = {
    fields: {
      resolution: {
        required: true,
        name: 'Resolution',
        schema: { type: 'resolution' },
        allowedValues: [{ id: '1', name: 'Fixed' }, { id: '2', name: "Won't Fix" }],
      },
      comment: { required: true, schema: { type: 'comment' } },
    },
  };
  const result = classifyTransition(t);
  assert.equal(result.kind, 'form');
  assert.equal(result.commentRequired, true);
  assert.equal(result.resolution.key, 'resolution');
  assert.equal(result.resolution.allowedValues.length, 2);
});

test('classifyTransition: a required custom field other than resolution/comment -> unsupported', () => {
  const t = {
    fields: {
      customfield_10099: { required: true, name: 'Sprint', schema: { type: 'array', custom: 'com.pyxis.greenhopper.jira:gh-sprint' } },
    },
  };
  const result = classifyTransition(t);
  assert.equal(result.kind, 'unsupported');
  assert.equal(result.fields.length, 1);
  assert.equal(result.fields[0].key, 'customfield_10099');
});

test('classifyTransition: a duplicated required field key -> unsupported', () => {
  // the object-keyed DC shape can't carry a duplicate key, so pass the already-array normalised
  // shape directly (transitionFields passes an array through unchanged) to hit the duplicate-key guard.
  const t = {
    fields: [
      { key: 'customfield_x', name: 'X', required: true, schema: { type: 'string' } },
      { key: 'customfield_x', name: 'X', required: true, schema: { type: 'string' } },
    ],
  };
  const result = classifyTransition(t);
  assert.equal(result.kind, 'unsupported');
  assert.equal(result.fields.length, 2);
  assert.ok(result.fields.every((f) => f.key === 'customfield_x'));
});

test('classifyTransition: a resolution field with schema.custom set is treated as unsupported, not the form select', () => {
  const t = {
    fields: {
      resolution: {
        required: true,
        name: 'Resolution',
        schema: { type: 'resolution', custom: 'com.example:custom-resolution' },
        allowedValues: [{ id: '1', name: 'Fixed' }],
      },
    },
  };
  const result = classifyTransition(t);
  assert.equal(result.kind, 'unsupported');
  assert.equal(result.fields[0].key, 'resolution');
});

test('classifyTransition: optional (non-required) fields are ignored entirely -> none', () => {
  const t = {
    fields: {
      resolution: { required: false, schema: { type: 'resolution' }, allowedValues: [{ id: '1', name: 'Fixed' }] },
      comment: { required: false, schema: { type: 'comment' } },
    },
  };
  assert.deepEqual(classifyTransition(t), { kind: 'none' });
});

test('classifyTransition: resolution required but with no allowedValues falls to unsupported', () => {
  const t = { fields: { resolution: { required: true, schema: { type: 'resolution' }, allowedValues: [] } } };
  const result = classifyTransition(t);
  assert.equal(result.kind, 'unsupported');
  assert.equal(result.fields[0].key, 'resolution');
});

// -- mapTransition --

test('mapTransition: maps id/name/to.name/to.category and runs classifyTransition as requirement', () => {
  const plain = mapTransition(RAW_TRANSITIONS[0]); // Cancelled, done, no fields -> none
  assert.equal(plain.id, '696880717');
  assert.equal(plain.name, 'Cancelled');
  assert.equal(plain.to.name, 'Cancelled');
  assert.equal(plain.to.category, 'done');
  assert.deepEqual(plain.requirement, { kind: 'none' });
});

test('mapTransition: a transition with fielded requirements maps requirement to form', () => {
  const t = {
    ...RAW_TRANSITIONS[1],
    fields: { comment: { required: true, schema: { type: 'comment' } } },
  };
  const mapped = mapTransition(t);
  assert.equal(mapped.id, String(RAW_TRANSITIONS[1].id));
  assert.equal(mapped.to.category, 'done');
  assert.deepEqual(mapped.requirement, { kind: 'form', resolution: null, commentRequired: true });
});

// -- mapComment --

test('mapComment: sanitises renderedBody, stripping <script> but keeping <p>', () => {
  const c = {
    id: '501',
    author: { displayName: 'User One', avatarUrls: { '24x24': 'https://example.invalid/a.png' } },
    created: '2026-10-01T00:00:00.000+0000',
    renderedBody: '<p>hello <script>alert(1)</script>world</p>',
  };
  const mapped = mapComment(c, { baseUrl: BASE });
  assert.equal(mapped.id, '501');
  assert.equal(mapped.author, 'User One');
  assert.equal(mapped.avatarUrl, 'https://example.invalid/a.png');
  assert.equal(mapped.created, '2026-10-01T00:00:00.000+0000');
  assert.equal(mapped.bodyHtml.includes('<script'), false);
  assert.equal(mapped.bodyHtml.includes('alert'), false);
  assert.match(mapped.bodyHtml, /<p>hello world<\/p>/);
});

test('mapComment: falls back to escaped body wrapped in <p> with newlines as <br> when renderedBody is absent', () => {
  const c = { id: '502', author: { name: 'user.two' }, created: '2026-10-02T00:00:00.000+0000', body: 'line1\nline2 <b>raw</b>' };
  const mapped = mapComment(c, { baseUrl: BASE });
  assert.equal(mapped.author, 'user.two');
  assert.equal(mapped.avatarUrl, null);
  assert.equal(mapped.bodyHtml, '<p>line1<br>line2 &lt;b&gt;raw&lt;/b&gt;</p>');
});

test('mapComment: missing author falls back to "unknown"', () => {
  const mapped = mapComment({ id: '503', body: 'x' }, { baseUrl: BASE });
  assert.equal(mapped.author, 'unknown');
});

// -- mapDetail --

test('mapDetail: keeps row fields (key, status) and adds descriptionHtml/descriptionText, comments, transitions', () => {
  const issue = ISSUES[0]; // DIGITALBTD-8139, renderedFields: null, no fields.description in fixture
  const detail = mapDetail(
    { issue, comments: [{ id: '1', author: { displayName: 'A' }, created: 'c1', body: 'hi' }], transitions: RAW_TRANSITIONS },
    { baseUrl: BASE },
  );
  assert.equal(detail.key, 'DIGITALBTD-8139');
  assert.equal(detail.status.name, 'In Progress');
  assert.equal(detail.descriptionHtml, '');
  assert.equal(detail.descriptionText, '');
  assert.equal(detail.comments.length, 1);
  assert.equal(detail.comments[0].author, 'A');
  assert.equal(detail.transitions.length, RAW_TRANSITIONS.length);
  assert.equal(detail.transitions[0].name, 'Cancelled');
});

test('mapDetail: accepts comments as the raw {comments: […]} body shape', () => {
  const issue = ISSUES[0];
  const detail = mapDetail(
    { issue, comments: { comments: [{ id: '9', body: 'wrapped' }] }, transitions: [] },
    { baseUrl: BASE },
  );
  assert.equal(detail.comments.length, 1);
  assert.equal(detail.comments[0].id, '9');
});

test('mapDetail: accepts comments as a plain array', () => {
  const issue = ISSUES[0];
  const detail = mapDetail({ issue, comments: [{ id: '10', body: 'plain' }], transitions: [] }, { baseUrl: BASE });
  assert.equal(detail.comments.length, 1);
  assert.equal(detail.comments[0].id, '10');
});

test('mapDetail: missing comments/transitions default to empty arrays', () => {
  const issue = ISSUES[0];
  const detail = mapDetail({ issue }, { baseUrl: BASE });
  assert.deepEqual(detail.comments, []);
  assert.deepEqual(detail.transitions, []);
});

test('mapDetail: prefers renderedFields.description (sanitised) over raw fields.description', () => {
  const issue = {
    ...ISSUES[0],
    fields: { ...ISSUES[0].fields, description: 'raw text' },
    renderedFields: { description: '<p>rendered <script>alert(1)</script>desc</p>' },
  };
  const detail = mapDetail({ issue, comments: [], transitions: [] }, { baseUrl: BASE });
  assert.match(detail.descriptionHtml, /<p>rendered desc<\/p>/);
  assert.equal(detail.descriptionHtml.includes('script'), false);
});

test('mapDetail: falls back to escaped raw fields.description in <p> when renderedFields.description is absent', () => {
  const issue = { ...ISSUES[0], fields: { ...ISSUES[0].fields, description: 'raw <b>text</b>' }, renderedFields: null };
  const detail = mapDetail({ issue, comments: [], transitions: [] }, { baseUrl: BASE });
  assert.equal(detail.descriptionHtml, '<p>raw &lt;b&gt;text&lt;/b&gt;</p>');
  assert.equal(detail.descriptionText, 'raw &lt;b&gt;text&lt;/b&gt;'); // textOf strips tags, leaves entities as-is (documented behaviour)
});

// -- branchName --

test('branchName: lower-cases the key and slugifies the title, joined with "-"', () => {
  const row = { key: 'DIGITALBTD-8139', title: 'celery work scoping (fixture)' };
  assert.equal(branchName(row), 'digitalbtd-8139-celery-work-scoping-fixture');
});

test('branchName: non-alnum runs collapse to a single "-", trimmed at the edges', () => {
  const row = { key: 'DIGITALBTD-8139', title: '  --Weird!! Title?? ' };
  assert.equal(branchName(row), 'digitalbtd-8139-weird-title');
});

test('branchName: caps the slug at 52 chars total', () => {
  const longTitle = 'a'.repeat(100);
  const row = { key: 'DIGITALBTD-8139', title: longTitle };
  const result = branchName(row);
  const slug = result.replace('digitalbtd-8139-', '');
  assert.equal(slug.length, 52);
});

test('branchName: empty title yields just the lower-cased key, no trailing dash', () => {
  const row = { key: 'DIGITALBTD-8139', title: '' };
  assert.equal(branchName(row), 'digitalbtd-8139');
});

// -- prompt --

test('prompt: exact format with description text', () => {
  const row = { key: 'DIGITALBTD-8139', title: 'celery work scoping', url: `${BASE}/browse/DIGITALBTD-8139`, descriptionText: 'Some description.' };
  assert.equal(
    prompt(row),
    'Complete Jira issue DIGITALBTD-8139: celery work scoping\n\nhttps://jira.biscrum.com/browse/DIGITALBTD-8139\n\nSome description.',
  );
});

test('prompt: omits the trailing description block when descriptionText is empty', () => {
  const row = { key: 'DIGITALBTD-8139', title: 'celery work scoping', url: `${BASE}/browse/DIGITALBTD-8139`, descriptionText: '' };
  assert.equal(prompt(row), 'Complete Jira issue DIGITALBTD-8139: celery work scoping\n\nhttps://jira.biscrum.com/browse/DIGITALBTD-8139');
});

// -- end-to-end: mapDetail's row feeds branchName/prompt directly --

test('end-to-end: mapDetail(issue #0) -> branchName and prompt produce the exact expected strings', () => {
  const issue = ISSUES[0];
  const detail = mapDetail({ issue, comments: [], transitions: [] }, { baseUrl: BASE });
  assert.equal(branchName(detail), 'digitalbtd-8139-celery-work-scoping-fixture');
  assert.equal(
    prompt(detail),
    `Complete Jira issue DIGITALBTD-8139: celery work scoping (fixture)\n\n${BASE}/browse/DIGITALBTD-8139`,
  );
});
