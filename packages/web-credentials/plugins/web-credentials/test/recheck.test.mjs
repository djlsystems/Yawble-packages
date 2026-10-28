// Tester Ingrid's recheck of 9e3b559 (card 21647), P7-P12: each test failed before its fix.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { join } from 'node:path';
import { readFileSync } from 'node:fs';
import { startFakeSite, USERNAME, PASSWORD, API_TOKEN } from './fakeSite.mjs';
import { runPlugin, workspace, cleanup } from './harness.mjs';

function entryRow(over = {}) {
  return { name: 'prod', siteUrl: '', method: 'password', signInUrl: '/login', checkUrl: '/dashboard', checkText: 'Dashboard', username: USERNAME, tokenHeader: 'Authorization', verifyEvery: 24, active: true, ...over };
}
const ask = (instruction) => [{ source: 'T/Manager', instruction }];
const scheduled = [{ source: 'schedule:hourly', instruction: 'verify all-due' }];
const stateOf = (ws) => JSON.parse(readFileSync(join(ws, '.web-credentials', 'state.json'), 'utf8'));

let other; // the host a page wrongly leaves for: it must never be asked anything
let site;
before(async () => {
  other = await startFakeSite({ host: '127.0.0.2' });
  site = await startFakeSite({ offsiteOrigin: other.origin });
});
after(async () => {
  await site.close();
  await other.close();
});

// P8: the platform clears a member's notice when its next run starts, so every run re-raises it while
// an entry needs the person, in the same words each time.
test('P8: Needs you is still raised after list and a quiet scheduled verify, in the same words', async () => {
  const ws = workspace();
  try {
    const base = { entries: [entryRow({ siteUrl: site.origin, method: 'interactive', signInUrl: '' })], secrets: {}, workingDirectory: ws };
    const r1 = await runPlugin({ ...base, work: ask('verify prod') });
    assert.equal(r1.decisions.length, 1);
    const r2 = await runPlugin({ ...base, work: ask('list') });
    assert.equal(r2.decisions.length, 1, 'list raises it again');
    const r3 = await runPlugin({ ...base, work: scheduled });
    assert.equal(r3.result.quiet, true, 'the scheduled run with nothing due stays quiet');
    assert.equal(r3.decisions.length, 1, 'the quiet scheduled run raises it again');
    assert.equal(r2.decisions[0].question, r1.decisions[0].question);
    assert.equal(r3.decisions[0].question, r1.decisions[0].question);
    assert.equal(r2.events.length + r3.events.length, 0, 'no status event from a repeat');

    // Fixed: nothing is raised any more.
    const live = JSON.stringify(site.liveSession('127.0.0.1'));
    const r4 = await runPlugin({ ...base, secrets: { entries: { prod: { session: live } } }, work: ask('verify prod') });
    assert.equal(r4.rowStatus.at(-1).values.status, 'Verified');
    assert.equal(r4.decisions.length, 0);
  } finally {
    cleanup(ws);
  }
});

// P7: the person's way out works as a person does it: a save, with no run in between.
test('P7: a held entry is let try once more by the person flipping Try again, with no run between saves', async () => {
  const s = await startFakeSite();
  const ws = workspace();
  try {
    const bad = { entries: [entryRow({ siteUrl: s.origin })], secrets: { entries: { prod: { password: 'wrong' } } }, workingDirectory: ws };
    await runPlugin({ ...bad, work: ask('verify prod') });
    const r2 = await runPlugin({ ...bad, work: ask('verify prod') });
    assert.equal(r2.rowStatus.at(-1).values.status, 'Needs you');
    const detail = r2.rowStatus.at(-1).values.detail;
    assert.match(detail, /Try again/);
    assert.doesNotMatch(detail, /off and on/, 'the text offers only a way out that works');

    // Off, save, on, save, no run between: the plugin sees the same row, and stays held.
    const posts = s.signInPosts();
    await runPlugin({ ...bad, work: ask('verify prod') });
    assert.equal(s.signInPosts(), posts, 'held');

    // The person switches Try again on and saves; the next verify may POST once, and its refusal holds it
    // again at once (P13).
    const retry = { ...bad, entries: [entryRow({ siteUrl: s.origin, retry: true })] };
    const r3 = await runPlugin({ ...retry, work: ask('verify prod') });
    assert.equal(s.signInPosts(), posts + 1, 'one new try after Try again');
    assert.equal(r3.rowStatus.at(-1).values.status, 'Needs you');
    await runPlugin({ ...retry, work: ask('verify prod') });
    assert.equal(s.signInPosts(), posts + 1, 'held again after that one refusal');

    // Switching it off again is a change too: one more try.
    await runPlugin({ ...bad, work: ask('verify prod') });
    assert.equal(s.signInPosts(), posts + 2);
  } finally {
    cleanup(ws);
    await s.close();
  }
});

// P9: a navigation stopped after the page loaded is reported as stopped, not as the page.
test('P9: check on a page that leaves by meta refresh says it was stopped, not HTTP 200; an off-host frame is named', async () => {
  const ws = workspace();
  try {
    const base = { entries: [entryRow({ siteUrl: site.origin })], secrets: { entries: { prod: { password: PASSWORD } } }, workingDirectory: ws };
    await runPlugin({ ...base, work: ask('verify prod') });
    const meta = await runPlugin({ ...base, work: ask('check prod /meta-off') });
    assert.doesNotMatch(meta.result.output, /HTTP 200/);
    assert.match(meta.result.output, /tried to go to 127\.0\.0\.2/);
    assert.match(meta.result.output, /stopped/);

    const frame = await runPlugin({ ...base, work: ask('check prod /frame-off') });
    assert.match(frame.result.output, /HTTP 200/);
    assert.match(frame.result.output, /heading: Framed/);
    assert.match(frame.result.output, /frame from 127\.0\.0\.2 was blocked/);
    assert.deepEqual(other.requests, [], 'the other host received no request at all');
  } finally {
    cleanup(ws);
  }
});

// P10: a good password whose check page then leaves the hosts is not a refused password.
test('P10: a check page that redirects off-host after a good sign-in is not counted against the guard', async () => {
  const ws = workspace();
  try {
    const before = site.signInPosts();
    const base = { entries: [entryRow({ siteUrl: site.origin, checkUrl: '/redir', checkText: '' })], secrets: { entries: { prod: { password: PASSWORD } } }, workingDirectory: ws };
    const r = await runPlugin({ ...base, work: ask('verify prod') });
    const last = r.rowStatus.at(-1).values;
    assert.equal(last.status, 'Needs you', last.detail);
    assert.match(last.detail, /check page redirected to 127\.0\.0\.2, off the entry's hosts/);
    assert.doesNotMatch(last.detail, /username and password/);
    assert.equal(stateOf(ws).entries.prod.passwordFailures, 0);
    // A password already sent is not sent again, every hour, into a page that leaves: it waits for the person.
    await runPlugin({ ...base, work: scheduled.map((w) => ({ ...w, instruction: 'verify prod' })) });
    assert.equal(site.signInPosts(), before + 1, 'one sign-in, not one per run');
    assert.deepEqual(other.requests, []);
  } finally {
    cleanup(ws);
  }
});

// P12: a site that stays down keeps one condition: the same words and no new event per run.
test('P12: a site that stays down gives the same notice and Detail each run, and one event', async () => {
  const dead = await startFakeSite();
  const origin = dead.origin;
  await dead.close();
  const ws = workspace();
  try {
    const base = { entries: [entryRow({ siteUrl: origin, method: 'token', checkUrl: '/api/me', checkText: '', signInUrl: '' })], secrets: { entries: { prod: { token: API_TOKEN } } }, workingDirectory: ws };
    const runs = [];
    for (let i = 0; i < 5; i++) runs.push(await runPlugin({ ...base, work: ask('verify prod') }));
    const needy = runs.slice(2);
    for (const r of needy) {
      assert.equal(r.rowStatus.at(-1).values.status, 'Needs you');
      assert.equal(r.decisions.length, 1);
    }
    assert.equal(new Set(needy.map((r) => r.decisions[0].question)).size, 1, 'the notice reads the same each run');
    assert.equal(new Set(needy.map((r) => r.rowStatus.at(-1).values.detail)).size, 1, 'the Detail reads the same each run');
    assert.equal(needy.flatMap((r) => r.events).length, 1, 'one event, when it became Needs you');
    assert.doesNotMatch(needy[2].decisions[0].question, /\b[45] checks/);
  } finally {
    cleanup(ws);
  }
});
