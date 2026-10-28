// Tester Ingrid's verification of 7a50635 (card 21647), P1-P6: each test failed before its fix.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { join } from 'node:path';
import { existsSync } from 'node:fs';
import { startFakeSite, USERNAME, PASSWORD, API_TOKEN } from './fakeSite.mjs';
import { runPlugin, workspace, cleanup, documentsOf, listFilesUnder } from './harness.mjs';

function entryRow(over = {}) {
  return { name: 'prod', siteUrl: '', method: 'password', signInUrl: '/login', checkUrl: '/dashboard', checkText: 'Dashboard', username: USERNAME, tokenHeader: 'Authorization', verifyEvery: 24, active: true, ...over };
}

let other; // the host a redirect wrongly points at: it must never be asked anything
let site;
before(async () => {
  other = await startFakeSite({ host: '127.0.0.2' });
  site = await startFakeSite({ offsiteOrigin: other.origin });
});
after(async () => {
  await site.close();
  await other.close();
});

const ask = (instruction) => [{ source: 'T/Manager', instruction }];

// P1: every path that submits a password goes through the lockout guard.
test('P1: repeated check against a changed password makes at most the guard’s POSTs, says it failed, and ends in Needs you', async () => {
  const s = await startFakeSite();
  const ws = workspace();
  try {
    const base = { entries: [entryRow({ siteUrl: s.origin })], secrets: { entries: { prod: { password: PASSWORD } } }, workingDirectory: ws };
    const first = await runPlugin({ ...base, work: ask('verify prod') });
    assert.equal(first.rowStatus.at(-1).values.status, 'Verified');

    s.changePassword('a-new-password'); // the person changed it on the site, which ended every session
    const before = s.signInPosts();
    const answers = [];
    let last;
    for (let i = 0; i < 4; i++) {
      last = await runPlugin({ ...base, work: ask('check prod /dashboard') });
      answers.push(last);
    }
    assert.ok(s.signInPosts() - before <= 2, `${s.signInPosts() - before} sign-in POSTs; the guard allows 2`);
    // Each answer says plainly the sign-in failed or is held; none reports the page as if fine.
    for (const a of answers) {
      assert.doesNotMatch(a.result.output, /HTTP 200/);
      assert.match(a.result.output, /sign-in FAILED|stopped trying/);
    }
    assert.match(answers[0].result.output, /sign-in FAILED/);
    const statuses = answers.flatMap((a) => a.rowStatus.map((r) => r.values.status));
    assert.equal(statuses.at(-1), 'Needs you');
    assert.ok(answers.flatMap((a) => a.events).some((e) => e.payload.to === 'Needs you'), 'a status event to Needs you');
    assert.ok(answers.some((a) => a.decisions.length === 1), 'the board is told');

    // Held: neither a verify nor a session from a member submits the password again.
    const held = s.signInPosts();
    const v = await runPlugin({ ...base, work: ask('verify prod') });
    const se = await runPlugin({ ...base, work: ask('session prod') });
    assert.equal(s.signInPosts(), held, 'no POST once the guard has tripped');
    assert.match(v.result.output, /stopped trying/);
    assert.match(se.result.output, /stopped trying/);

    // The person edits the row with the new password: the guard lifts and the sign-in works.
    const fixed = await runPlugin({ ...base, secrets: { entries: { prod: { password: 'a-new-password' } } }, work: ask('check prod /dashboard') });
    assert.match(fixed.result.output, /HTTP 200/);
    assert.equal(fixed.rowStatus.at(-1).values.status, 'Verified');
  } finally {
    cleanup(ws);
    await s.close();
  }
});

test('P1: turning a held entry off and on lifts the guard', async () => {
  const s = await startFakeSite();
  const ws = workspace();
  try {
    const bad = { entries: [entryRow({ siteUrl: s.origin })], secrets: { entries: { prod: { password: 'wrong' } } }, workingDirectory: ws };
    await runPlugin({ ...bad, work: ask('verify prod') });
    const r2 = await runPlugin({ ...bad, work: ask('verify prod') });
    assert.equal(r2.rowStatus.at(-1).values.status, 'Needs you');
    const posts = s.signInPosts();
    await runPlugin({ ...bad, entries: [entryRow({ siteUrl: s.origin, active: false })], work: ask('list') });
    const r3 = await runPlugin({ ...bad, work: ask('verify prod') });
    assert.equal(s.signInPosts(), posts + 1, 'one new try after off and on');
    assert.equal(r3.rowStatus.at(-1).values.status, 'Expired');
  } finally {
    cleanup(ws);
    await s.close();
  }
});

// P2: a request to a host outside the row never leaves the browser.
test('P2: an off-host redirect on check, sign-in and an interactive check page is blocked before the other host is asked', async () => {
  const ws = workspace();
  try {
    const base = { entries: [entryRow({ siteUrl: site.origin })], secrets: { entries: { prod: { password: PASSWORD } } }, workingDirectory: ws };
    await runPlugin({ ...base, work: ask('verify prod') });
    const r = await runPlugin({ ...base, work: ask('check prod /redir') });
    assert.match(r.result.output, /127\.0\.0\.2/);
    assert.match(r.result.output, /off the entry's hosts/);

    const signIn = await runPlugin({ ...base, entries: [entryRow({ name: 'other', siteUrl: site.origin, signInUrl: '/offsite-login' })], secrets: { entries: { other: { password: PASSWORD } } }, work: ask('verify other') });
    assert.equal(signIn.rowStatus.at(-1).values.status, 'Needs you');
    assert.match(signIn.rowStatus.at(-1).values.detail, /127\.0\.0\.2/);

    const live = JSON.stringify(site.liveSession('127.0.0.1'));
    const inter = await runPlugin({ ...base, entries: [entryRow({ name: 'int', siteUrl: site.origin, method: 'interactive', signInUrl: '', checkUrl: '/redir', checkText: '' })], secrets: { entries: { int: { session: live } } }, work: ask('verify int') });
    assert.equal(inter.rowStatus.at(-1).values.status, 'Needs you');

    assert.deepEqual(other.requests, [], 'the other host received no request at all');
  } finally {
    cleanup(ws);
  }
});

// P3: the session a password sign-in obtains is redacted, value by value.
test('P3: check on a page that prints its own cookie and local-storage token shows them redacted', async () => {
  const ws = workspace();
  try {
    const base = { entries: [entryRow({ siteUrl: site.origin })], secrets: { entries: { prod: { password: PASSWORD } } }, workingDirectory: ws };
    await runPlugin({ ...base, work: ask('verify prod') });
    const r = await runPlugin({ ...base, work: ask('check prod /leaky expect "Cookie"') });
    const cookie = site.lastSession();
    assert.ok(cookie, 'the site gave out a session');
    const blob = r.lines.join('\n');
    assert.ok(!blob.includes(cookie), 'the cookie value is in a record');
    assert.ok(!blob.includes(`ls-access-${cookie}-token`), 'the local-storage token is in a record');
    assert.match(r.result.output, /\[redacted\]/);
  } finally {
    cleanup(ws);
  }
});

// P4: the documents folder is the platform's, not a fixed path; tests keep to temp folders.
test('P4: the team documents folder comes from the request, never a fixed /data/documents', async () => {
  const { teamDocuments } = await import('../src/main.mjs');
  assert.equal(typeof teamDocuments, 'function');
  assert.equal(teamDocuments({ siteFiles: [{ site: 'board', folder: '/srv/yawble/documents/Ops/sites/board/files' }] }, '/x/y'), '/srv/yawble/documents/Ops');
  assert.equal(teamDocuments({}, '/srv/yawble/teams/Ops/workspaces/Creds'), '/srv/yawble/documents/Ops');
  assert.equal(teamDocuments({}, '/tmp/somewhere'), null);
});

test('P4: a check screenshot and a handoff stay inside the run’s own data root', async () => {
  const ws = workspace();
  try {
    const base = { entries: [entryRow({ siteUrl: site.origin, method: 'token', checkUrl: '/api/me', checkText: '', signInUrl: '' })], secrets: { entries: { prod: { token: API_TOKEN } } }, workingDirectory: ws };
    await runPlugin({ ...base, work: ask('verify prod') });
    await runPlugin({ ...base, work: ask('session prod') });
    const manager = join(ws, '..', 'Manager', '.web-credentials-sessions');
    assert.equal(listFilesUnder(manager).length, 1, 'the handoff is in T/Manager’s workspace beside the plugin’s');

    const pw = { entries: [entryRow({ siteUrl: site.origin })], secrets: { entries: { prod: { password: PASSWORD } } }, workingDirectory: ws };
    await runPlugin({ ...pw, work: ask('verify prod') });
    const r = await runPlugin({ ...pw, work: ask('check prod /dashboard') });
    const rel = /screenshot: (.+)/.exec(r.result.output)[1];
    assert.ok(rel.startsWith('Web Credentials/'));
    assert.ok(existsSync(join(documentsOf(ws), rel)), 'the screenshot is in the run’s documents folder');
  } finally {
    cleanup(ws);
  }
});

// P5: a CAPTCHA on a password row says what the person can do.
test('P5: a CAPTCHA on a password entry tells the person to switch it to interactive', async () => {
  const cap = await startFakeSite({ captcha: true });
  const ws = workspace();
  try {
    const r = await runPlugin({ entries: [entryRow({ siteUrl: cap.origin })], secrets: { entries: { prod: { password: PASSWORD } } }, work: ask('verify prod'), workingDirectory: ws });
    const detail = r.rowStatus.at(-1).values.detail;
    assert.match(detail, /CAPTCHA/);
    assert.match(detail, /Method to interactive, then open its Sign in link/);
    assert.equal(cap.signInPosts(), 0, 'nothing submitted past a CAPTCHA');
  } finally {
    cleanup(ws);
    await cap.close();
  }
});

// P6: the notice once per new condition, and the wording.
test('P6: Needs you is raised once per new condition, in plain words', async () => {
  const ws = workspace();
  try {
    const base = { entries: [entryRow({ siteUrl: site.origin, method: 'interactive', signInUrl: '' })], secrets: {}, workingDirectory: ws };
    const r1 = await runPlugin({ ...base, work: [{ source: 'schedule:s', instruction: 'verify all-due' }] });
    assert.equal(r1.decisions.length, 1);
    assert.match(r1.decisions[0].question, /^1 website sign-in needs you:/);
    assert.equal((r1.decisions[0].question.match(/Sign in link/g) || []).length, 1, 'said once');

    const r2 = await runPlugin({ ...base, work: ask('list') });
    assert.equal(r2.decisions.length, 0, 'list does not raise it again');
    const r3 = await runPlugin({ ...base, work: ask('verify prod') });
    assert.equal(r3.decisions.length, 0, 'the same condition is not raised again');

    // Expired is a new condition: raised again, the Sign in link named once.
    const dead = JSON.stringify(site.deadSession('127.0.0.1'));
    const r4 = await runPlugin({ ...base, secrets: { entries: { prod: { session: dead } } }, work: ask('verify prod') });
    assert.equal(r4.decisions.length, 1);
    assert.equal((r4.decisions[0].question.match(/Sign in link/g) || []).length, 1, 'said once');
  } finally {
    cleanup(ws);
  }
});

test('P6: an unreachable site names the cause, not "TypeError"', async () => {
  const dead = await startFakeSite();
  const origin = dead.origin;
  await dead.close();
  const ws = workspace();
  try {
    const r = await runPlugin({ entries: [entryRow({ siteUrl: origin, method: 'token', checkUrl: '/api/me', checkText: '', signInUrl: '' })], secrets: { entries: { prod: { token: API_TOKEN } } }, work: ask('verify prod'), workingDirectory: ws });
    const detail = r.rowStatus.at(-1).values.detail;
    assert.match(detail, /connection refused/);
    assert.doesNotMatch(detail, /TypeError/);
  } finally {
    cleanup(ws);
  }
});
