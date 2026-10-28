import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { join, dirname } from 'node:path';
import { existsSync } from 'node:fs';
import { writeFileSync } from 'node:fs';
import { startFakeSite, USERNAME, PASSWORD, TOTP_SECRET, API_TOKEN } from './fakeSite.mjs';
import { runPlugin, workspace, cleanup, listFilesUnder, readText } from './harness.mjs';

// A scheduled verify only runs an entry whose Verify every has elapsed; in a test the clock does
// not move, so this backdates an entry's last check so the next scheduled run sees it as due.
function markDue(ws, name) {
  const path = join(ws, '.web-credentials', 'state.json');
  const st = JSON.parse(readText(path));
  if (st.entries[name]) st.entries[name].lastCheckAt = new Date(Date.now() - 48 * 3600 * 1000).toISOString();
  writeFileSync(path, JSON.stringify(st));
}

// One entry row, with overrides. The plugin's secret cells sit under secrets.entries.<row>.
function entryRow(over = {}) {
  return { name: 'prod', siteUrl: '', method: 'password', signInUrl: '/login', checkUrl: '/dashboard', checkText: 'Dashboard', username: USERNAME, tokenHeader: 'Authorization', verifyEvery: 24, active: true, ...over };
}

let site, offsite;
before(async () => {
  offsite = await startFakeSite();
  site = await startFakeSite();
});
after(async () => {
  await site.close();
  await offsite.close();
});

function req(over, secrets, work, ws) {
  return { entries: [entryRow({ siteUrl: site.origin, ...over })], secrets: { entries: { prod: secrets } }, work, workingDirectory: ws };
}

test('password entry verifies, writes its status, and publishes the transition', async () => {
  const ws = workspace();
  try {
    const r = await runPlugin(req({}, { password: PASSWORD }, [{ source: 'T/Manager', instruction: 'verify prod' }], ws));
    assert.equal(r.result.ok, true);
    assert.equal(r.rowStatus[0].values.status, 'Verified');
    assert.equal(r.events[0].type, 'status.changed');
    assert.deepEqual([r.events[0].payload.from, r.events[0].payload.to], ['Never checked', 'Verified']);
  } finally {
    cleanup(ws);
  }
});

test('a wrong password is Expired, and the second in a row trips the lockout guard (Needs you, paused)', async () => {
  const ws = workspace();
  try {
    const bad = req({ verifyEvery: 1 }, { password: 'wrong-password' }, [{ source: 'schedule:s', instruction: 'verify all-due' }], ws);
    const r1 = await runPlugin(bad);
    assert.equal(r1.rowStatus.at(-1).values.status, 'Expired');

    markDue(ws, 'prod');
    const r2 = await runPlugin(bad);
    assert.equal(r2.rowStatus.at(-1).values.status, 'Needs you');
    assert.match(r2.rowStatus.at(-1).values.detail, /paused/);

    // A third scheduled run does not try again while it is held, even once due.
    markDue(ws, 'prod');
    const r3 = await runPlugin(bad);
    assert.ok(!r3.progress.some((p) => p.status.includes('Checking')));
  } finally {
    cleanup(ws);
  }
});

test('an edit to a held row (the new password) lifts the lockout guard and tries again', async () => {
  const ws = workspace();
  try {
    const bad = req({}, { password: 'wrong-password' }, [{ source: 'schedule:s', instruction: 'verify all-due' }], ws);
    await runPlugin(bad);
    markDue(ws, 'prod');
    await runPlugin(bad); // held now
    const good = req({}, { password: PASSWORD }, [{ source: 'T/Manager', instruction: 'verify prod' }], ws);
    const r = await runPlugin(good);
    assert.equal(r.rowStatus.at(-1).values.status, 'Verified');
  } finally {
    cleanup(ws);
  }
});

test('a token entry verifies against the token API and Expires when the token is refused', async () => {
  const ws = workspace();
  try {
    const good = req({ method: 'token', checkUrl: '/api/me', checkText: '', signInUrl: '' }, { token: API_TOKEN }, [{ source: 'T/Manager', instruction: 'verify prod' }], ws);
    const r = await runPlugin(good);
    assert.equal(r.rowStatus.at(-1).values.status, 'Verified');

    const bad = req({ method: 'token', checkUrl: '/api/me', checkText: '', signInUrl: '' }, { token: 'nope-token' }, [{ source: 'T/Manager', instruction: 'verify prod' }], ws);
    const r2 = await runPlugin(bad);
    assert.equal(r2.rowStatus.at(-1).values.status, 'Expired');
  } finally {
    cleanup(ws);
  }
});

test('an interactive entry verifies from a saved session and Expires when it no longer works', async () => {
  const ws = workspace();
  try {
    const live = JSON.stringify(site.liveSession('127.0.0.1'));
    const ok = req({ method: 'interactive', signInUrl: '' }, { session: live }, [{ source: 'T/Manager', instruction: 'verify prod' }], ws);
    const r = await runPlugin(ok);
    assert.equal(r.rowStatus.at(-1).values.status, 'Verified');

    const dead = JSON.stringify(site.deadSession('127.0.0.1'));
    const r2 = await runPlugin(req({ method: 'interactive', signInUrl: '' }, { session: dead }, [{ source: 'T/Manager', instruction: 'verify prod' }], ws));
    assert.equal(r2.rowStatus.at(-1).values.status, 'Expired');
  } finally {
    cleanup(ws);
  }
});

test('a token entry whose site is unreachable is Failed, and Needs you after three such runs', async () => {
  const dead = await startFakeSite();
  const origin = dead.origin;
  await dead.close(); // nothing is listening now: every check fails
  const ws = workspace();
  try {
    const base = { entries: [entryRow({ siteUrl: origin, method: 'token', checkUrl: '/api/me', checkText: '', signInUrl: '', verifyEvery: 1 })], secrets: { entries: { prod: { token: API_TOKEN } } }, work: [{ source: 'schedule:s', instruction: 'verify all-due' }], workingDirectory: ws };
    const r1 = await runPlugin(base);
    assert.equal(r1.rowStatus.at(-1).values.status, 'Failed');
    markDue(ws, 'prod');
    await runPlugin(base);
    markDue(ws, 'prod');
    const r3 = await runPlugin(base);
    assert.equal(r3.rowStatus.at(-1).values.status, 'Needs you');
  } finally {
    cleanup(ws);
  }
});

test('an interactive entry with no saved session is Needs you with "Sign in"', async () => {
  const ws = workspace();
  try {
    const r = await runPlugin(req({ method: 'interactive', signInUrl: '' }, {}, [{ source: 'T/Manager', instruction: 'verify prod' }], ws));
    assert.equal(r.rowStatus.at(-1).values.status, 'Needs you');
    assert.match(r.rowStatus.at(-1).values.detail, /Sign in/);
  } finally {
    cleanup(ws);
  }
});

test('a CAPTCHA at password sign-in is Needs you, never a bypass', async () => {
  const cap = await startFakeSite({ captcha: true });
  const ws = workspace();
  try {
    const r = await runPlugin({ entries: [entryRow({ siteUrl: cap.origin, signInUrl: '/login' })], secrets: { entries: { prod: { password: PASSWORD } } }, work: [{ source: 'T/Manager', instruction: 'verify prod' }], workingDirectory: ws });
    assert.equal(r.rowStatus.at(-1).values.status, 'Needs you');
    assert.match(r.rowStatus.at(-1).values.detail, /CAPTCHA|Sign in/);
  } finally {
    cleanup(ws);
    await cap.close();
  }
});

test('a redirect off the entry hosts during sign-in is refused, naming the host', async () => {
  const other = await startFakeSite({ host: '127.0.0.2' });
  const redir = await startFakeSite({ offsiteOrigin: other.origin });
  const ws = workspace();
  try {
    const r = await runPlugin({
      entries: [entryRow({ siteUrl: redir.origin, signInUrl: '/offsite-login' })],
      secrets: { entries: { prod: { password: PASSWORD } } },
      work: [{ source: 'T/Manager', instruction: 'verify prod' }],
      workingDirectory: ws,
    });
    assert.equal(r.rowStatus.at(-1).values.status, 'Needs you');
    assert.match(r.rowStatus.at(-1).values.detail, /127\.0\.0\.2|off the entry/);
  } finally {
    cleanup(ws);
    await redir.close();
    await other.close();
  }
});

test('an inactive entry is never checked; list and verify refuse it', async () => {
  const ws = workspace();
  try {
    const r = await runPlugin(req({ active: false }, { password: PASSWORD }, [{ source: 'schedule:s', instruction: 'verify all-due' }, { source: 'T/Manager', instruction: 'verify prod' }], ws));
    assert.ok(!r.progress.some((p) => p.status.includes('Checking')));
    assert.match(r.result.output, /inactive/);
  } finally {
    cleanup(ws);
  }
});

test('check refuses a path off the entry hosts, and reports and screenshots an on-site path', async () => {
  const ws = workspace();
  try {
    // First sign in (verify) so the entry is usable, then check.
    const base = req({}, { password: PASSWORD }, null, ws);
    const r1 = await runPlugin({ ...base, work: [{ source: 'T/Manager', instruction: 'verify prod' }] });
    assert.equal(r1.rowStatus.at(-1).values.status, 'Verified');

    const off = await runPlugin({ ...base, work: [{ source: 'T/Manager', instruction: 'check prod http://127.0.0.2:1/x' }] });
    assert.match(off.result.output, /not one of the entry|off the entry/);

    const on = await runPlugin({ ...base, work: [{ source: 'T/Manager', instruction: 'check prod /dashboard expect "Dashboard"' }] });
    assert.match(on.result.output, /HTTP 200/);
    assert.match(on.result.output, /expected text found/);
    assert.match(on.result.output, /screenshot:/);
  } finally {
    cleanup(ws);
  }
});

test('session hands a member a storage state in its own workspace, pruned, and it is deleted when it expires', async () => {
  const teamRoot = workspace();
  const credsWs = join(teamRoot, 'workspaces', 'Creds');
  const askerWs = join(teamRoot, 'workspaces', 'Asker');
  const { mkdirSync } = await import('node:fs');
  mkdirSync(credsWs, { recursive: true });
  mkdirSync(askerWs, { recursive: true });
  try {
    const base = req({}, { password: PASSWORD }, null, credsWs);
    await runPlugin({ ...base, work: [{ source: 'T/Asker', instruction: 'verify prod' }] });
    const r = await runPlugin({ ...base, work: [{ source: 'T/Asker', instruction: 'session prod for 1' }] });
    assert.match(r.result.output, /Asker:/); // handed to the asking member
    const handoffs = listFilesUnder(join(askerWs, '.web-credentials-sessions'));
    assert.equal(handoffs.length, 1);
    const state = JSON.parse(readText(handoffs[0]));
    assert.equal(state.kind, 'storageState');
    assert.ok(state.storageState.cookies.every((c) => c.domain.includes('127.0.0.1')));

    // A later run after it has expired sweeps the file away.
    const past = handoffs[0];
    // Make it already expired by handing off "for 1" minute then moving time: simplest is to write
    // a run whose sweep sees an expiry in the past, so rewrite the handoff's expiry.
    const { writeFileSync } = await import('node:fs');
    const body = JSON.parse(readText(past));
    body.expiresAt = new Date(Date.now() - 1000).toISOString();
    writeFileSync(past, JSON.stringify(body));
    // The plugin reads its own state for the handoff list; force the recorded expiry too.
    const statePath = join(credsWs, '.web-credentials', 'state.json');
    const st = JSON.parse(readText(statePath));
    for (const h of st.handoffs) h.expiresAt = body.expiresAt;
    writeFileSync(statePath, JSON.stringify(st));
    await runPlugin({ ...base, work: [{ source: 'schedule:s', instruction: 'verify all-due' }] });
    assert.ok(!existsSync(past), 'the expired handoff is deleted');
  } finally {
    cleanup(teamRoot);
  }
});

test('session for a token entry hands the header and a reference, never the token', async () => {
  const teamRoot = workspace();
  const credsWs = join(teamRoot, 'workspaces', 'Creds');
  const askerWs = join(teamRoot, 'workspaces', 'Asker');
  const { mkdirSync } = await import('node:fs');
  mkdirSync(credsWs, { recursive: true });
  mkdirSync(askerWs, { recursive: true });
  try {
    const base = req({ method: 'token', checkUrl: '/api/me', checkText: '', signInUrl: '' }, { token: API_TOKEN }, null, credsWs);
    await runPlugin({ ...base, work: [{ source: 'T/Asker', instruction: 'verify prod' }] });
    const r = await runPlugin({ ...base, work: [{ source: 'T/Asker', instruction: 'session prod' }] });
    assert.match(r.result.output, /header Authorization/);
    assert.ok(!r.result.output.includes(API_TOKEN));
    const handoff = JSON.parse(readText(listFilesUnder(join(askerWs, '.web-credentials-sessions'))[0]));
    assert.equal(handoff.kind, 'token');
    assert.equal(handoff.value, `Bearer ${API_TOKEN}`); // usable, but only in the asker's workspace
  } finally {
    cleanup(teamRoot);
  }
});

test('a scheduled run that changes nothing is quiet; one that changes a status is not', async () => {
  const ws = workspace();
  try {
    const sched = req({}, { password: PASSWORD }, [{ source: 'schedule:s', instruction: 'verify all-due' }], ws);
    const first = await runPlugin(sched);
    assert.notEqual(first.result.quiet, true); // Never checked -> Verified is a change
    const second = await runPlugin(sched);
    // Second run: still verified, nothing due again within the hour -> nothing changed -> quiet.
    assert.equal(second.result.quiet, true);
  } finally {
    cleanup(ws);
  }
});

test('while an entry needs you, the board shows it once with what to do', async () => {
  const ws = workspace();
  try {
    const r = await runPlugin(req({ method: 'interactive', signInUrl: '' }, {}, [{ source: 'schedule:s', instruction: 'verify all-due' }], ws));
    assert.equal(r.decisions.length, 1);
    assert.match(r.decisions[0].question, /needs you/);
    assert.match(r.decisions[0].question, /Sign in/);
  } finally {
    cleanup(ws);
  }
});

test('no secret or storage state appears in any record the run writes', async () => {
  const ws = workspace();
  try {
    const live = JSON.stringify(site.liveSession('127.0.0.1'));
    const r = await runPlugin({
      entries: [
        entryRow({ name: 'pw', siteUrl: site.origin }),
        entryRow({ name: 'tok', siteUrl: site.origin, method: 'token', checkUrl: '/api/me', checkText: '', signInUrl: '' }),
        entryRow({ name: 'int', siteUrl: site.origin, method: 'interactive', signInUrl: '' }),
      ],
      secrets: { entries: { pw: { password: PASSWORD, totpSecret: TOTP_SECRET }, tok: { token: API_TOKEN }, int: { session: live } } },
      work: [
        { source: 'T/Manager', instruction: 'verify pw' },
        { source: 'T/Manager', instruction: 'verify tok' },
        { source: 'T/Manager', instruction: 'verify int' },
        { source: 'T/Manager', instruction: 'session tok' },
        { source: 'T/Manager', instruction: 'list' },
      ],
      workingDirectory: ws,
    });
    const blob = r.lines.join('\n');
    for (const secret of [PASSWORD, API_TOKEN, TOTP_SECRET, ...JSON.parse(live).cookies.map((c) => c.value)]) {
      assert.ok(!blob.includes(secret), `a record leaked ${secret}`);
    }
    // Nothing secret written to the team documents folder (only the screenshot, a PNG) either.
  } finally {
    cleanup(ws);
  }
});
