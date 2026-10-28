// Tester Ingrid's recheck of 76996e3 (card 22338), P17: the test failed before its fix.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { join } from 'node:path';
import { readFileSync, writeFileSync } from 'node:fs';
import { startFakeSite, USERNAME } from './fakeSite.mjs';
import { runPlugin, workspace, cleanup } from './harness.mjs';

function entryRow(over = {}) {
  return { name: 'prod', siteUrl: '', method: 'password', signInUrl: '/login', checkUrl: '/dashboard', checkText: 'Dashboard', username: USERNAME, tokenHeader: 'Authorization', verifyEvery: 24, active: true, ...over };
}
const ask = (instruction) => [{ source: 'T/Manager', instruction }];
const stateFile = (ws) => join(ws, '.web-credentials', 'state.json');
const SwitchWords = /Switch its Try again setting \(on or off, either way\) and save|switch its Try again setting \(on or off, either way\) and save/;
// The way out as the 33a5756 plugin stored it in a held entry's reason.
const OldWayOut = 'Set a new password if it changed, or tick (or untick) its Try again box, and save, to let it try once more.';

let site;
before(async () => {
  site = await startFakeSite();
});
after(async () => {
  await site.close();
});

// P17: a held reason is reported with the current way-out words, built from the hold's kind, whatever
// words an older plugin stored for it.
test('P17: an entry held with 33a5756-style stored words is reported in the current words, a new password only for a password hold', async () => {
  const ws = workspace();
  try {
    const bad = { entries: [entryRow({ siteUrl: site.origin })], secrets: { entries: { prod: { password: 'wrong' } } }, workingDirectory: ws };
    await runPlugin({ ...bad, work: ask('verify prod') });
    await runPlugin({ ...bad, work: ask('verify prod') });
    const posts = site.signInPosts();

    const cases = [
      { kind: 'password', cause: 'prod needs you: the site refused the password, so the plugin has stopped trying, to keep the account from being locked.', newPassword: true },
      { kind: 'off-host', cause: "prod needs you: the check page redirected to 127.0.0.2, off the entry's hosts; the browser did not go there. Add 127.0.0.2 to the entry's Other hosts if it belongs to the site. The plugin will not sign in again until the row changes.", newPassword: false },
      { kind: 'CAPTCHA', cause: 'prod needs you: a CAPTCHA or device approval was shown during sign-in. The plugin never gets around one: change the entry’s Method to interactive, then open its Sign in link and sign in yourself. The plugin will not sign in again until the row changes.', newPassword: false },
    ];
    for (const c of cases) {
      // The state an older plugin left: its words stored, no hold kind.
      const state = JSON.parse(readFileSync(stateFile(ws), 'utf8'));
      const s = state.entries.prod;
      assert.equal(s.held, true);
      delete s.holdKind;
      s.reason = `${c.cause} ${OldWayOut}`;
      writeFileSync(stateFile(ws), JSON.stringify(state, null, 2));

      const r = await runPlugin({ ...bad, work: ask('verify prod') });
      const said = [r.rowStatus.at(-1)?.values.detail, r.decisions.at(-1)?.question, r.result.output ?? r.result.error];
      for (const words of said) {
        assert.ok(words, `${c.kind}: reported`);
        assert.ok(words.includes(c.cause), `${c.kind}: the cause is kept: ${words}`);
        assert.match(words, SwitchWords, `${c.kind}: current words`);
        assert.doesNotMatch(words, /tick|box/, `${c.kind}: no old words`);
        if (c.newPassword) assert.match(words, /Set a new password if it changed/);
        else assert.doesNotMatch(words, /new password/, `${c.kind}: no new password asked for`);
      }
      // Reported the same on the next run, with no row rewrite, no event and no POST.
      const again = await runPlugin({ ...bad, work: ask('verify prod') });
      assert.equal(again.rowStatus.length, 0, `${c.kind}: Detail written once`);
      assert.equal(again.events.length + r.events.length, 0, `${c.kind}: no event`);
      assert.equal(again.decisions.at(-1)?.question, r.decisions.at(-1)?.question);
      assert.equal(site.signInPosts(), posts, `${c.kind}: still held`);
    }
  } finally {
    cleanup(ws);
  }
});
