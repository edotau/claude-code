'use strict';
// Unit tests for the task query language in lib/task-query.js (ported from
// claude-orcastra's src/shared/task-query.ts): tokenizer, scope detection, parser,
// serializer, withQualifier and stripRepoQualifiers.
const test = require('node:test');
const assert = require('node:assert/strict');
const {
  tokenizeSearchQuery,
  hasExplicitTaskScope,
  parseTaskQuery,
  serializeTaskQuery,
  withQualifier,
  stripRepoQualifiers,
} = require('../lib/task-query');

// -- tokenizeSearchQuery --

test('tokenizeSearchQuery: splits on whitespace', () => {
  assert.deepEqual(tokenizeSearchQuery('is:open assignee:@me foo'), ['is:open', 'assignee:@me', 'foo']);
});

test('tokenizeSearchQuery: unwraps standalone double-quoted tokens', () => {
  assert.deepEqual(tokenizeSearchQuery('"needs review" foo'), ['needs review', 'foo']);
});

test('tokenizeSearchQuery: unwraps standalone single-quoted tokens', () => {
  assert.deepEqual(tokenizeSearchQuery("'with spaces' bar"), ['with spaces', 'bar']);
});

test('tokenizeSearchQuery: keeps quoted qualifier values as one token', () => {
  assert.deepEqual(tokenizeSearchQuery('label:"needs review" author:alice'), ['label:needs review', 'author:alice']);
});

test('tokenizeSearchQuery: returns an empty list for an empty string', () => {
  assert.deepEqual(tokenizeSearchQuery(''), []);
});

// -- hasExplicitTaskScope --

test('hasExplicitTaskScope: recognizes scope qualifiers and their quoted values', () => {
  for (const query of ['is:issue', 'IS:PR', 'is:pull-request', 'is:"pr"']) {
    assert.equal(hasExplicitTaskScope(query), true, query);
  }
});

test('hasExplicitTaskScope: keeps quoted phrases and values of other qualifiers as search text', () => {
  for (const query of ['"is:pr"', "'is:issue'", 'label:is:pr', 'title:"is:pull-request"']) {
    assert.equal(hasExplicitTaskScope(query), false, query);
    assert.equal(parseTaskQuery(query).scope, 'all', query);
  }
});

// -- parseTaskQuery --

test('parseTaskQuery: returns defaults for an empty query', () => {
  const parsed = parseTaskQuery('');
  assert.equal(parsed.scope, 'all');
  assert.equal(parsed.state, null);
  assert.deepEqual(parsed.labels, []);
  assert.equal(parsed.freeText, '');
});

test('parseTaskQuery: parses is:issue and is:open', () => {
  const parsed = parseTaskQuery('is:issue is:open');
  assert.equal(parsed.scope, 'issue');
  assert.equal(parsed.state, 'open');
});

test('parseTaskQuery: parses is:pull-request as a PR scope alias', () => {
  const parsed = parseTaskQuery('is:pull-request is:open');
  assert.equal(parsed.scope, 'pr');
  assert.equal(parsed.state, 'open');
});

test('parseTaskQuery: widens scope to all when both is:issue and is:pr are present', () => {
  assert.equal(parseTaskQuery('is:issue is:pr').scope, 'all');
});

test('parseTaskQuery: widens scope to all regardless of issue and PR token order', () => {
  assert.equal(parseTaskQuery('is:pr is:issue').scope, 'all');
});

test('parseTaskQuery: is:draft forces scope to pr and state to open', () => {
  const parsed = parseTaskQuery('is:draft');
  assert.equal(parsed.scope, 'pr');
  assert.equal(parsed.state, 'open');
  assert.equal(parsed.draft, true);
});

test('parseTaskQuery: keeps draft scoped to open PRs even when a later token says issue', () => {
  const parsed = parseTaskQuery('is:draft is:issue');
  assert.equal(parsed.scope, 'pr');
  assert.equal(parsed.state, 'open');
  assert.equal(parsed.draft, true);
});

test('parseTaskQuery: is:pr is:open does not set draft', () => {
  const parsed = parseTaskQuery('is:pr is:open');
  assert.equal(parsed.scope, 'pr');
  assert.equal(parsed.state, 'open');
  assert.equal(parsed.draft, false);
});

test('parseTaskQuery: extracts assignee, author, label, and review qualifiers', () => {
  const parsed = parseTaskQuery('assignee:@me author:alice review-requested:@me label:bug free text');
  assert.equal(parsed.assignee, '@me');
  assert.equal(parsed.author, 'alice');
  assert.equal(parsed.reviewRequested, '@me');
  assert.equal(parsed.scope, 'pr');
  assert.deepEqual(parsed.labels, ['bug']);
  assert.equal(parsed.freeText, 'free text');
});

test('parseTaskQuery: keeps review qualifiers scoped to PRs even when a later token says issue', () => {
  const parsed = parseTaskQuery('review-requested:@me is:issue');
  assert.equal(parsed.scope, 'pr');
  assert.equal(parsed.reviewRequested, '@me');
});

test('parseTaskQuery: leaves unknown qualifiers and bare words in freeText', () => {
  assert.equal(parseTaskQuery('custom:value hello').freeText, 'custom:value hello');
});

test('parseTaskQuery: parses state:all for the Any state filter', () => {
  const parsed = parseTaskQuery('is:pr state:all');
  assert.equal(parsed.scope, 'pr');
  assert.equal(parsed.state, 'all');
});

// -- stripRepoQualifiers --

test('stripRepoQualifiers: removes repo:owner/name tokens', () => {
  assert.equal(stripRepoQualifiers('is:open repo:foo/bar assignee:@me'), 'is:open assignee:@me');
});

test('stripRepoQualifiers: is case-insensitive on the repo: key', () => {
  assert.equal(stripRepoQualifiers('REPO:Foo/Bar is:open'), 'is:open');
});

test('stripRepoQualifiers: keeps other qualifiers intact', () => {
  assert.equal(stripRepoQualifiers('label:bug repo:a/b'), 'label:bug');
});

test('stripRepoQualifiers: re-quotes a standalone token that contains whitespace', () => {
  assert.equal(stripRepoQualifiers('"needs review" repo:x/y'), '"needs review"');
});

test('stripRepoQualifiers: returns empty string when only repo qualifiers are present', () => {
  assert.equal(stripRepoQualifiers('repo:foo/bar repo:baz/qux'), '');
});

test('stripRepoQualifiers: preserves bare words around a removed repo: token', () => {
  assert.equal(stripRepoQualifiers('hello repo:a/b world'), 'hello world');
});

// -- serializeTaskQuery --

test('serializeTaskQuery: round-trips qualifiers and free text', () => {
  const raw = 'is:pr is:open author:alice label:bug review-requested:bob hello world';
  const reserialized = serializeTaskQuery(parseTaskQuery(raw));
  assert.deepEqual(parseTaskQuery(reserialized), parseTaskQuery(raw));
});

test('serializeTaskQuery: quotes label values containing whitespace', () => {
  const parsed = parseTaskQuery('label:"needs review"');
  assert.deepEqual(parsed.labels, ['needs review']);
  assert.equal(parsed.freeText, '');
  assert.ok(serializeTaskQuery(parsed).includes('label:"needs review"'));
});

test('serializeTaskQuery: serializes all state so filter changes do not fall back to the default open state', () => {
  assert.equal(serializeTaskQuery(parseTaskQuery('is:pr state:all')), 'is:pr state:all');
});

// -- withQualifier --

test('withQualifier: sets and clears the author qualifier without disturbing free text', () => {
  const set = withQualifier('hello', 'author', 'alice');
  assert.equal(parseTaskQuery(set).author, 'alice');
  assert.equal(parseTaskQuery(set).freeText, 'hello');
  const cleared = withQualifier(set, 'author', null);
  assert.equal(parseTaskQuery(cleared).author, null);
  assert.equal(parseTaskQuery(cleared).freeText, 'hello');
});

test('withQualifier: replaces the labels list', () => {
  const result = withQualifier('label:bug label:enh', 'labels', ['triage']);
  assert.deepEqual(parseTaskQuery(result).labels, ['triage']);
});

test('withQualifier: clears labels when given an empty array, keeping the pr scope', () => {
  const result = withQualifier('label:bug is:pr', 'labels', []);
  assert.deepEqual(parseTaskQuery(result).labels, []);
  assert.equal(parseTaskQuery(result).scope, 'pr');
});

test('withQualifier: sets the all state filter', () => {
  const result = withQualifier('is:pr is:open', 'state', 'all');
  assert.equal(parseTaskQuery(result).state, 'all');
  assert.ok(result.includes('state:all'));
});

test('withQualifier: preserves quoted free-text tokens when applying a filter', () => {
  const result = withQualifier('"exact phrase" milestone:"next release"', 'author', 'alice');
  assert.ok(result.includes('"exact phrase"'));
  assert.ok(result.includes('milestone:"next release"'));
  assert.equal(parseTaskQuery(result).author, 'alice');
});

test('withQualifier: keeps PR-only filters scoped to PRs', () => {
  assert.equal(parseTaskQuery(withQualifier('', 'draft', 'true')).scope, 'pr');
  assert.equal(parseTaskQuery(withQualifier('', 'state', 'merged')).scope, 'pr');
  assert.equal(parseTaskQuery(withQualifier('', 'reviewRequested', '@me')).scope, 'pr');
});

test('withQualifier: forces draft filters back to open PRs', () => {
  const parsed = parseTaskQuery(withQualifier('is:pr is:closed', 'draft', 'true'));
  assert.equal(parsed.scope, 'pr');
  assert.equal(parsed.state, 'open');
  assert.equal(parsed.draft, true);
});
