'use strict';
// Unit tests for the GitHub conversation/detail additions to lib/tasks-github.js: argv builders
// (conversationArgs, timelineArgs, checkRunsArgs, addCommentArgs), mappers (mapTimeline,
// mapConversation, mapCheckRuns) against test/fixtures/github/, and the execFileImpl wrappers
// (conversation, checkRuns, addComment).
//
// NOTE (deviation, not fixed by this agent): test/fixtures/github/timeline-activity.json is an
// empty `[]` on disk, not the commented/reviewed/review_requested/cross-referenced/renamed/assigned
// fixture the brief describes. Tests for those event kinds build minimal inline objects shaped like
// the real GitHub timeline API (same field names as timeline-pr.json) instead of reading that file.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {
  conversationArgs,
  timelineArgs,
  checkRunsArgs,
  addCommentArgs,
  mapTimeline,
  mapConversation,
  mapCheckRuns,
  conversation,
  checkRuns,
  addComment,
} = require('../lib/tasks-github');

const FIXTURE_DIR = path.join(__dirname, 'fixtures', 'github');
function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(FIXTURE_DIR, `${name}.json`), 'utf8'));
}

const REPO = { owner: 'o', name: 'r' };
const NUMBER = 7;

// -- argv builders --

test('conversationArgs: gh api issues/<n> with the html accept header', () => {
  assert.deepEqual(conversationArgs({ repo: REPO, number: NUMBER }), [
    'api',
    'repos/o/r/issues/7',
    '-H',
    'Accept: application/vnd.github.html+json',
  ]);
});

test('timelineArgs: gh api --paginate the timeline, 100 per page, html accept header', () => {
  assert.deepEqual(timelineArgs({ repo: REPO, number: NUMBER }), [
    'api',
    '--paginate',
    'repos/o/r/issues/7/timeline?per_page=100',
    '-H',
    'Accept: application/vnd.github.html+json',
  ]);
});

test('checkRunsArgs: gh api commits/<sha>/check-runs, 100 per page', () => {
  assert.deepEqual(checkRunsArgs({ repo: REPO, sha: 'abc123' }), ['api', 'repos/o/r/commits/abc123/check-runs?per_page=100']);
});

test('addCommentArgs: POST comments with html accept header, body piped on stdin not argv', () => {
  const args = addCommentArgs({ repo: REPO, number: NUMBER });
  assert.deepEqual(args, ['api', '-X', 'POST', 'repos/o/r/issues/7/comments', '-H', 'Accept: application/vnd.github.html+json', '--input', '-']);
  assert.ok(!args.includes('--raw-field'));
  assert.ok(!args.some((a) => /body/i.test(a))); // the comment text never appears as an argv element
});

// -- mapTimeline --

const PR_TIMELINE = fixture('timeline-pr');

test('mapTimeline: committed maps to an event with the first message line and the commit html_url', () => {
  const entries = mapTimeline(PR_TIMELINE);
  const committed = entries.find((e) => e.event === 'committed');
  assert.ok(committed, 'committed entry present');
  assert.equal(committed.kind, 'event');
  assert.equal(committed.actor, 'dependabot[bot]');
  assert.equal(committed.text, 'chore(deps): bump the go-modules group with 2 updates');
  assert.equal(committed.url, 'https://github.com/edotau/claude-code/commit/f8b76b08a0c0def2c4159f2c9a1e66ca20a8b607');
});

test('mapTimeline: labeled/unlabeled map to "added label <name>" / "removed label <name>"', () => {
  const entries = mapTimeline(PR_TIMELINE);
  const labeled = entries.filter((e) => e.event === 'labeled');
  assert.equal(labeled.length, 2);
  assert.deepEqual(labeled.map((e) => e.text).sort(), ['added label dependencies', 'added label go']);
  const unlabeled = mapTimeline([{ ...PR_TIMELINE[1], event: 'unlabeled' }]);
  assert.equal(unlabeled[0].text, 'removed label dependencies');
});

test('mapTimeline: merged/closed/head_ref_deleted map to fixed event text', () => {
  const entries = mapTimeline(PR_TIMELINE);
  assert.equal(entries.find((e) => e.event === 'merged').text, 'merged this');
  assert.equal(entries.find((e) => e.event === 'closed').text, 'closed this');
  assert.equal(entries.find((e) => e.event === 'head_ref_deleted').text, 'deleted the head branch');
});

test('mapTimeline: closed with a state_reason appends " as <state_reason>" (underscore -> space)', () => {
  const closed = PR_TIMELINE.find((e) => e.event === 'closed');
  const entries = mapTimeline([{ ...closed, state_reason: 'not_planned' }]);
  assert.equal(entries[0].text, 'closed this as not planned');
});

test('mapTimeline: the merged/closed/head_ref_deleted trio sorts ascending by createdAt', () => {
  const entries = mapTimeline(PR_TIMELINE).filter((e) => ['merged', 'closed', 'head_ref_deleted'].includes(e.event));
  const times = entries.map((e) => new Date(e.createdAt).getTime());
  assert.deepEqual(times, [...times].sort((a, b) => a - b));
});

test('mapTimeline: commented maps to kind comment with sanitised bodyHtml', () => {
  const entries = mapTimeline([
    { event: 'commented', id: 1, actor: { login: 'alice' }, user: { login: 'alice', avatar_url: 'https://x/a.png' }, body_html: '<p>hi <b>there</b></p>', created_at: '2026-01-01T00:00:00Z' },
  ]);
  assert.equal(entries.length, 1);
  assert.equal(entries[0].kind, 'comment');
  assert.equal(entries[0].actor, 'alice');
  assert.equal(entries[0].avatarUrl, 'https://x/a.png');
  assert.equal(entries[0].bodyHtml, '<p>hi <b>there</b></p>');
});

test('mapTimeline: reviewed maps to kind review with state, falling back createdAt to submitted_at', () => {
  const entries = mapTimeline([
    { event: 'reviewed', id: 2, user: { login: 'bob', avatar_url: 'https://x/b.png' }, state: 'APPROVED', body_html: '<p>lgtm</p>', submitted_at: '2026-01-02T00:00:00Z' },
  ]);
  assert.equal(entries.length, 1);
  assert.equal(entries[0].kind, 'review');
  assert.equal(entries[0].state, 'APPROVED');
  assert.equal(entries[0].bodyHtml, '<p>lgtm</p>');
  assert.equal(entries[0].createdAt, '2026-01-02T00:00:00Z');
});

test('mapTimeline: a COMMENTED review with an empty body is dropped', () => {
  const entries = mapTimeline([
    { event: 'reviewed', id: 3, user: { login: 'carl' }, state: 'COMMENTED', body_html: '', body: '', submitted_at: '2026-01-02T01:00:00Z' },
  ]);
  assert.equal(entries.length, 0);
});

test('mapTimeline: a CHANGES_REQUESTED review with a body is kept', () => {
  const entries = mapTimeline([
    { event: 'reviewed', id: 4, user: { login: 'dana' }, state: 'CHANGES_REQUESTED', body_html: '<p>fix this</p>', submitted_at: '2026-01-02T02:00:00Z' },
  ]);
  assert.equal(entries.length, 1);
  assert.equal(entries[0].state, 'CHANGES_REQUESTED');
});

test('mapTimeline: review_requested maps to "requested a review from <login>"', () => {
  const entries = mapTimeline([
    { event: 'review_requested', id: 5, actor: { login: 'carol' }, requested_reviewer: { login: 'dave' }, created_at: '2026-01-03T00:00:00Z' },
  ]);
  assert.equal(entries[0].text, 'requested a review from dave');
});

test('mapTimeline: review_requested for a team falls back to requested_team.name', () => {
  const entries = mapTimeline([
    { event: 'review_requested', id: 5.5, actor: { login: 'carol' }, requested_team: { name: 'platform' }, created_at: '2026-01-03T00:30:00Z' },
  ]);
  assert.equal(entries[0].text, 'requested a review from platform');
});

test('mapTimeline: cross-referenced maps to "referenced this from #<n> <title>" with the source issue url', () => {
  const entries = mapTimeline([
    {
      event: 'cross-referenced',
      id: 6,
      actor: { login: 'eve' },
      created_at: '2026-01-04T00:00:00Z',
      source: { type: 'issue', issue: { number: 42, title: 'Some other issue', html_url: 'https://github.com/o/r/issues/42' } },
    },
  ]);
  assert.equal(entries[0].text, 'referenced this from #42 Some other issue');
  assert.equal(entries[0].url, 'https://github.com/o/r/issues/42');
});

test('mapTimeline: renamed maps to \'renamed from "<from>" to "<to>"\'', () => {
  const entries = mapTimeline([
    { event: 'renamed', id: 7, actor: { login: 'frank' }, created_at: '2026-01-05T00:00:00Z', rename: { from: 'old-title', to: 'new-title' } },
  ]);
  assert.equal(entries[0].text, 'renamed from "old-title" to "new-title"');
});

test('mapTimeline: assigned/unassigned map to "assigned <login>" / "unassigned <login>"', () => {
  const assigned = mapTimeline([
    { event: 'assigned', id: 8, actor: { login: 'grace' }, assignee: { login: 'henry' }, created_at: '2026-01-06T00:00:00Z' },
  ]);
  assert.equal(assigned[0].text, 'assigned henry');
  const unassigned = mapTimeline([
    { event: 'unassigned', id: 9, actor: { login: 'grace' }, assignee: { login: 'henry' }, created_at: '2026-01-06T01:00:00Z' },
  ]);
  assert.equal(unassigned[0].text, 'unassigned henry');
});

test('mapTimeline: milestoned maps to "added to milestone <title>"', () => {
  const entries = mapTimeline([
    { event: 'milestoned', id: 10, actor: { login: 'ida' }, created_at: '2026-01-07T00:00:00Z', milestone: { title: 'v1.0' } },
  ]);
  assert.equal(entries[0].text, 'added to milestone v1.0');
});

test('mapTimeline: mentioned/subscribed/plain referenced/connected/locked-type noise are dropped', () => {
  const noise = [
    { event: 'mentioned', id: 11, actor: { login: 'x' }, created_at: '2026-01-08T00:00:00Z' },
    { event: 'subscribed', id: 12, actor: { login: 'x' }, created_at: '2026-01-08T01:00:00Z' },
    { event: 'referenced', id: 13, actor: { login: 'x' }, created_at: '2026-01-08T02:00:00Z', commit_id: 'deadbeef' },
    { event: 'connected', id: 14, actor: { login: 'x' }, created_at: '2026-01-08T03:00:00Z' },
    { event: 'locked', id: 15, actor: { login: 'x' }, created_at: '2026-01-08T04:00:00Z' },
    { event: 'unlocked', id: 16, actor: { login: 'x' }, created_at: '2026-01-08T05:00:00Z' },
  ];
  assert.deepEqual(mapTimeline(noise), []);
});

test('mapTimeline: entries are sorted oldest first across mixed comment/review/event kinds', () => {
  const entries = mapTimeline([
    { event: 'reviewed', id: 20, user: { login: 'b' }, state: 'APPROVED', body_html: '<p>ok</p>', submitted_at: '2026-01-10T03:00:00Z' },
    { event: 'commented', id: 21, user: { login: 'a' }, body_html: '<p>c1</p>', created_at: '2026-01-10T01:00:00Z' },
    { event: 'closed', id: 22, actor: { login: 'c' }, created_at: '2026-01-10T02:00:00Z' },
  ]);
  assert.deepEqual(entries.map((e) => e.id), ['21', '22', '20']);
});

// -- mapConversation --

test('mapConversation: sanitised body_html, author/avatar/createdAt, entries + commentCount', () => {
  const issue = fixture('issue-html');
  const conv = mapConversation(issue, PR_TIMELINE, REPO);
  assert.equal(conv.author, 'dependabot[bot]');
  assert.ok(conv.bodyHtml.startsWith('<p'));
  assert.equal(conv.createdAt, issue.created_at);
  assert.equal(Array.isArray(conv.entries), true);
  assert.equal(conv.commentCount, conv.entries.filter((e) => e.kind === 'comment').length);
});

test('mapConversation: null body_html maps to an empty string, never null', () => {
  const conv = mapConversation({ ...fixture('issue-html'), body_html: null }, [], REPO);
  assert.equal(conv.bodyHtml, '');
});

test('mapConversation: commentCount counts only kind comment entries, not reviews or events', () => {
  const timeline = [
    { event: 'commented', id: 1, user: { login: 'a' }, body_html: '<p>1</p>', created_at: '2026-01-01T00:00:00Z' },
    { event: 'commented', id: 2, user: { login: 'b' }, body_html: '<p>2</p>', created_at: '2026-01-02T00:00:00Z' },
    { event: 'reviewed', id: 3, user: { login: 'c' }, state: 'APPROVED', body_html: '<p>3</p>', submitted_at: '2026-01-03T00:00:00Z' },
    { event: 'closed', id: 4, actor: { login: 'd' }, created_at: '2026-01-04T00:00:00Z' },
  ];
  const conv = mapConversation(fixture('issue-html'), timeline, REPO);
  assert.equal(conv.commentCount, 2);
});

// -- mapCheckRuns --

test('mapCheckRuns: the two completed/success fixture runs fold to success, sorted by name', () => {
  const out = mapCheckRuns(fixture('check-runs'));
  assert.equal(out.total, 2);
  assert.equal(out.runs.length, 2);
  assert.ok(out.runs.every((r) => r.state === 'success'));
  assert.deepEqual(out.runs.map((r) => r.name), ['go (macos-latest)', 'go (ubuntu-latest)']);
  assert.equal(out.runs[0].url, 'https://github.com/edotau/claude-code/actions/runs/39689740993/job/119532680621');
  assert.equal(out.runs[0].app, 'github-actions');
});

test('mapCheckRuns: failed sorts before in_progress before success before skipped; conclusion folds per summarizeChecks', () => {
  const json = {
    total_count: 5,
    check_runs: [
      { name: 'z-success', status: 'completed', conclusion: 'success', html_url: 'u1', app: { slug: 'gha' } },
      { name: 'b-skip', status: 'completed', conclusion: 'skipped', html_url: 'u2', app: { slug: 'gha' } },
      { name: 'a-fail', status: 'completed', conclusion: 'failure', html_url: 'u3', app: { slug: 'gha' } },
      { name: 'c-neutral', status: 'completed', conclusion: 'neutral', html_url: 'u4', app: { slug: 'gha' } },
      { name: 'm-running', status: 'in_progress', conclusion: null, html_url: 'u5', app: { slug: 'gha' } },
    ],
  };
  const out = mapCheckRuns(json);
  assert.equal(out.total, 5);
  assert.deepEqual(out.runs.map((r) => r.name), ['a-fail', 'm-running', 'c-neutral', 'z-success', 'b-skip']);
  assert.deepEqual(out.runs.map((r) => r.state), ['failed', 'in_progress', 'success', 'success', 'skipped']);
});

test('mapCheckRuns: cancelled/timed_out/action_required/startup_failure/error all fold to failed', () => {
  const conclusions = ['cancelled', 'timed_out', 'action_required', 'startup_failure', 'error'];
  const json = { total_count: conclusions.length, check_runs: conclusions.map((c, i) => ({ name: `r${i}`, status: 'completed', conclusion: c, html_url: `u${i}`, app: { slug: 'gha' } })) };
  const out = mapCheckRuns(json);
  assert.ok(out.runs.every((r) => r.state === 'failed'));
});

// -- wrappers (recording fake execFileImpl) --

function recordingExec(responses) {
  const calls = [];
  const impl = (bin, args, opts, cb) => {
    calls.push({ bin, args, opts });
    const hit = responses.find((r) => r.match(args));
    if (!hit) return cb(new Error(`no fixture for args ${JSON.stringify(args)}`));
    cb(null, hit.stdout, '');
  };
  return { impl, calls };
}

test('conversation(): fetches the issue (html accept) then the timeline, and maps them together', async () => {
  const { impl, calls } = recordingExec([
    { match: (a) => a.includes('repos/o/r/issues/7') && !a.some((x) => String(x).includes('timeline')), stdout: JSON.stringify(fixture('issue-html')) },
    { match: (a) => a.some((x) => String(x).includes('/issues/7/timeline')), stdout: JSON.stringify(fixture('timeline-pr')) },
  ]);
  const conv = await conversation({ repo: REPO, number: NUMBER, execFileImpl: impl });
  assert.equal(calls.length, 2);
  assert.equal(conv.author, 'dependabot[bot]');
  assert.ok(conv.entries.length > 0);
});

test('checkRuns(): fetches commits/<sha>/check-runs and maps it', async () => {
  const { impl } = recordingExec([{ match: (a) => a.some((x) => String(x).includes('check-runs')), stdout: JSON.stringify(fixture('check-runs')) }]);
  const out = await checkRuns({ repo: REPO, sha: 'deadbeef', execFileImpl: impl });
  assert.equal(out.total, 2);
});

test('addComment(): POSTs with the body piped on stdin (options.input), never as an argv element', async () => {
  let seenOpts;
  const impl = (bin, args, opts, cb) => {
    seenOpts = opts;
    assert.deepEqual(args, addCommentArgs({ repo: REPO, number: NUMBER }));
    cb(null, JSON.stringify({ id: 999, user: { login: 'me', avatar_url: 'https://x/me.png' }, body_html: '<p>reply</p>', created_at: '2026-02-02T00:00:00Z' }), '');
  };
  const entry = await addComment({ repo: REPO, number: NUMBER, body: 'reply', execFileImpl: impl });
  assert.ok(seenOpts && seenOpts.input != null, 'the body must be handed through options.input, not argv');
  const payload = JSON.parse(String(seenOpts.input));
  assert.deepEqual(payload, { body: 'reply' });
  assert.equal(entry.kind, 'comment');
  assert.equal(entry.actor, 'me');
  assert.equal(entry.bodyHtml, '<p>reply</p>');
  assert.equal(entry.id, '999');
});

// -- teeth: a <script> in fixture-injected body_html never survives sanitisation --

test('teeth: a <script> smuggled into a commented event bodyHtml is stripped by mapTimeline', () => {
  const evil = { event: 'commented', id: 666, user: { login: 'attacker' }, body_html: '<p>hi</p><script>alert(1)</script>', created_at: '2026-01-01T00:00:00Z' };
  const entries = mapTimeline([evil]);
  assert.equal(entries.length, 1);
  assert.ok(!/<script/i.test(entries[0].bodyHtml), `script tag survived: ${entries[0].bodyHtml}`);
});

test('teeth: a <script> smuggled into the issue body_html never survives mapConversation', () => {
  const issue = { ...fixture('issue-html'), body_html: '<p>legit</p><script>alert(document.cookie)</script>' };
  const conv = mapConversation(issue, [], REPO);
  assert.ok(!/<script/i.test(conv.bodyHtml), `script tag survived: ${conv.bodyHtml}`);
});
