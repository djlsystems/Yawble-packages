// Tester Ingrid's recheck of 33a5756 (card 21647), P13-P15: each test failed before its fix.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { join, dirname } from 'node:path';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { startFakeSite, USERNAME, PASSWORD } from './fakeSite.mjs';
import { runPlugin, workspace, cleanup } from './harness.mjs';

function entryRow(over = {}) {
  return { name: 'prod', siteUrl: '', method: 'password', signInUrl: '/login', checkUrl: '/dashboard', checkText: 'Dashboard', username: USERNAME, tokenHeader: 'Authorization', verifyEvery: 24, active: true, ...over };
}
const ask = (instruction) => [{ source: 'T/Manager', instruction }];
const stateOf = (ws) => JSON.parse(readFileSync(join(ws, '.web-credentials', 'state.json'), 'utf8'));
const SwitchWords = /[Ss]witch its Try again setting \(on or off, either way\) and save/;

let other;
let site;
before(async () => {
  other = await startFakeSite({ host: '127.0.0.2' });
  site = await startFakeSite({ offsiteOrigin: other.origin });
});
after(async () => {
  await site.close();
  await other.close();
});

// P13: any lift of a held entry - Try again, a new password - allows exactly one more password POST.
test('P13: a Try-again lift and a new-password lift each allow exactly one POST, then the entry is held again', async () => {
  const s = await startFakeSite();
  const ws = workspace();
  try {
    const row = (over = {}) => [entryRow({ siteUrl: s.origin, ...over })];
    const secrets = (password) => ({ entries: { prod: { password } } });
    const bad = { entries: row(), secrets: secrets('wrong'), workingDirectory: ws };
    await runPlugin({ ...bad, work: ask('verify prod') });
    await runPlugin({ ...bad, work: ask('verify prod') });
    const posts = s.signInPosts();
    assert.equal(posts, 2);

    // Try again switched on and saved.
    const retry = { ...bad, entries: row({ retry: true }) };
    const r1 = await runPlugin({ ...retry, work: ask('verify prod') });
    assert.equal(s.signInPosts(), posts + 1, 'exactly one POST after a Try-again lift');
    assert.equal(r1.rowStatus.at(-1).values.status, 'Needs you', 'held again at once');
    await runPlugin({ ...retry, work: ask('verify prod') });
    assert.equal(s.signInPosts(), posts + 1, '0 POSTs on the next verify');

    // A new password (still not the site's) saved.
    const newPw = { ...retry, secrets: secrets('also wrong') };
    const r2 = await runPlugin({ ...newPw, work: ask('verify prod') });
    assert.equal(s.signInPosts(), posts + 2, 'exactly one POST after a new-password lift');
    assert.equal(r2.rowStatus.at(-1).values.status, 'Needs you');
    await runPlugin({ ...newPw, work: ask('check prod /dashboard') });
    assert.equal(s.signInPosts(), posts + 2, '0 POSTs on the next command');

    // The right password: one POST, Verified, and the count starts over.
    const good = { ...retry, secrets: secrets(PASSWORD) };
    const r3 = await runPlugin({ ...good, work: ask('verify prod') });
    assert.equal(s.signInPosts(), posts + 3);
    assert.equal(r3.rowStatus.at(-1).values.status, 'Verified');
    assert.equal(stateOf(ws).entries.prod.passwordFailures, 0);
  } finally {
    cleanup(ws);
    await s.close();
  }
});

// P14: the words name the control as it looks, and a hold that is not about the password does not
// ask for one; the column sits next to Active.
test('P14: Try again is next to Active, and every held text says to switch it, a new password only when it is about one', async () => {
  const here = dirname(fileURLToPath(import.meta.url));
  const manifest = JSON.parse(readFileSync(join(here, '..', 'plugin.json'), 'utf8'));
  const cols = manifest.config.entries.columns.map((c) => c.name);
  assert.equal(cols.indexOf('retry'), cols.indexOf('active') + 1);
  assert.ok(cols.indexOf('retry') < 6, `retry is column ${cols.indexOf('retry') + 1}`);
  const col = (n) => manifest.config.entries.columns.find((c) => c.name === n);
  assert.match(col('retry').description, /on or off/);
  assert.match(col('hosts').description, /pages and frames/);

  const s = await startFakeSite();
  const ws = workspace();
  const ws2 = workspace();
  try {
    const bad = { entries: [entryRow({ siteUrl: s.origin })], secrets: { entries: { prod: { password: 'wrong' } } }, workingDirectory: ws };
    await runPlugin({ ...bad, work: ask('verify prod') });
    const refused = await runPlugin({ ...bad, work: ask('verify prod') });
    const detail = refused.rowStatus.at(-1).values.detail;
    assert.match(detail, SwitchWords);
    assert.match(detail, /new password/);
    assert.doesNotMatch(detail, /tick|box/);

    // The P10 hold: the site took the password, so no new password is asked for.
    const off = { entries: [entryRow({ siteUrl: site.origin, checkUrl: '/redir', checkText: '' })], secrets: { entries: { prod: { password: PASSWORD } } }, workingDirectory: ws2 };
    const r = await runPlugin({ ...off, work: ask('verify prod') });
    const held = r.rowStatus.at(-1).values.detail;
    assert.match(held, SwitchWords);
    assert.doesNotMatch(held, /new password|tick|box/);
  } finally {
    cleanup(ws);
    cleanup(ws2);
    await s.close();
  }
});

// P15: a script that leaves for another host is named by the host it tried, not Chrome's error page.
test('P15: check on a page that leaves by location.href names the blocked host, not chromewebdata', async () => {
  const ws = workspace();
  try {
    const base = { entries: [entryRow({ siteUrl: site.origin })], secrets: { entries: { prod: { password: PASSWORD } } }, workingDirectory: ws };
    await runPlugin({ ...base, work: ask('verify prod') });
    const r = await runPlugin({ ...base, work: ask('check prod /js-off') });
    assert.doesNotMatch(r.result.output, /chromewebdata/);
    assert.doesNotMatch(r.result.output, /HTTP 200/);
    assert.match(r.result.output, /127\.0\.0\.2/);
    assert.deepEqual(other.requests, [], 'the other host received no request at all');
  } finally {
    cleanup(ws);
  }
});
