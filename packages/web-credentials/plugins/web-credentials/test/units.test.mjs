import { test } from 'node:test';
import assert from 'node:assert/strict';
import { totp } from '../src/totp.mjs';
import { entryHosts, hostAllowed, resolveOnSite, readEntries, secretValues, readSession, stateSecretValues } from '../src/entries.mjs';
import { identityFor } from '../src/profile.mjs';
import { choose, compareVersions } from '../src/chrome.mjs';
import { pruneState, chromeArgs } from '../src/browser.mjs';
import { makeOut } from '../src/out.mjs';

test('totp matches a known RFC 6238 vector', () => {
  // ASCII "12345678901234567890" is base32 GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ; at t=59 the code is 94287082.
  assert.equal(totp('GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ', 59000, 30, 8), '94287082');
});

test('entryHosts takes the site host and the extra hosts, lower case, no port', () => {
  const hosts = entryHosts('https://App.Example.com:8443/', 'www.example.com, https://cdn.example.com/x EXTRA.example.com');
  assert.deepEqual(hosts, ['app.example.com', 'www.example.com', 'cdn.example.com', 'extra.example.com']);
});

test('entryHosts refuses a non-http site URL', () => {
  assert.deepEqual(entryHosts('file:///etc/passwd', ''), []);
});

test('hostAllowed is by host, and resolveOnSite refuses an off-host URL', () => {
  const entry = { name: 'e', siteUrl: 'https://app.example.com', hosts: ['app.example.com'] };
  assert.equal(hostAllowed(entry, 'https://app.example.com/x'), true);
  assert.equal(hostAllowed(entry, 'https://evil.example/x'), false);
  assert.ok(resolveOnSite(entry, '/dashboard', 'p').url.toString().startsWith('https://app.example.com/dashboard'));
  assert.ok(resolveOnSite(entry, 'https://evil.example/x', 'the path').refusal.includes('not one of the entry'));
});

test('readEntries keeps secret cells beside the row, never in it', () => {
  const entries = readEntries({
    config: { entries: [{ name: 'P', siteUrl: 'https://a.example.com', method: 'password', username: 'u', active: true }] },
    secrets: { entries: { p: { password: 'pw', session: '{}' } } },
  });
  assert.equal(entries.length, 1);
  assert.equal(entries[0].secrets.password, 'pw');
  assert.ok(!('password' in entries[0].written));
});

test('pruneState keeps only the allowed hosts', () => {
  const state = {
    cookies: [
      { name: 'a', value: 'x', domain: 'app.example.com' },
      { name: 'b', value: 'y', domain: '.other.example' },
    ],
    origins: [{ origin: 'https://app.example.com' }, { origin: 'https://other.example' }],
  };
  const pruned = pruneState(state, ['app.example.com']);
  assert.equal(pruned.cookies.length, 1);
  assert.equal(pruned.cookies[0].name, 'a');
  assert.equal(pruned.origins.length, 1);
});

test('out redacts every secret value it was given, longest first and any case', () => {
  const lines = [];
  const out = makeOut((l) => lines.push(l));
  out.setSecrets(['s3cr3t', 's3cr3t-longer-token']);
  out.result(true, 'token is S3CR3T-LONGER-TOKEN and also s3cr3t');
  const r = JSON.parse(lines[0]);
  assert.ok(!r.output.toLowerCase().includes('s3cr3t'));
  assert.match(r.output, /\[redacted\].*\[redacted\]/);
});

test('stateSecretValues: whole values of 4+ characters (4-7 as whole tokens), strings inside JSON at 8+, 1-3 characters and settings words never', () => {
  const vals = stateSecretValues({
    cookies: [{ value: 'code123' }, { value: 'abcd' }, { value: 'xq7' }, { value: 'true' }, { value: 'DARK' }, { value: 'undefined' }, { value: 'cookie-long-1' }],
    origins: [{ localStorage: [{ value: 'ls12' }, { value: 'q' }, { value: 'default' }, { value: JSON.stringify({ a: 'short77', b: 'eightch8' }) }] }],
  });
  const tokens = vals.filter((v) => typeof v !== 'string').map((v) => v.token);
  const anywhere = vals.filter((v) => typeof v === 'string');
  for (const v of ['code123', 'abcd', 'ls12']) assert.ok(tokens.includes(v), v);
  for (const v of ['eightch8', 'cookie-long-1']) assert.ok(anywhere.includes(v), v);
  for (const v of ['xq7', 'q', 'short77', 'true', 'DARK', 'undefined', 'default']) assert.ok(!tokens.includes(v) && !anywhere.includes(v), v);
});

test('out redacts a 4-7 character session value only as a whole token, an 8+ one anywhere', () => {
  const lines = [];
  const out = makeOut((l) => lines.push(l));
  out.setSecrets([{ token: 'k7q2z' }, { token: 'code123' }, 'eightch8']);
  out.result(true, 'k7q2z, (CODE123) abck7q2z9 code1234 xeightch8x é-k7q2z-é');
  assert.equal(JSON.parse(lines[0]).output, '[redacted], ([redacted]) abck7q2z9 code1234 x[redacted]x é-[redacted]-é');
  // A password the run was handed (a plain string) stays redacted anywhere.
  out.setSecrets(['hunt2', { token: 'hunt2' }]);
  out.result(true, 'xhunt2x');
  assert.equal(JSON.parse(lines[1]).output, 'x[redacted]x');
});

test('secretValues pulls cookie and local-storage values out of a saved session', () => {
  const session = JSON.stringify({ cookies: [{ value: 'cookievalue1234' }], origins: [{ localStorage: [{ value: 'lsvalue5678' }] }] });
  const vals = secretValues({ secrets: { entries: { e: { session } } } }, [{ secrets: { session }, session: readSession(session) }]);
  assert.ok(vals.includes('cookievalue1234'));
  assert.ok(vals.includes('lsvalue5678'));
  // The same from a v2 envelope.
  const v2 = JSON.stringify({ v: 2, storageState: { cookies: [{ value: 'cookievalue9999' }], origins: [] }, identity: { version: '156.0.1.2' } });
  const vals2 = secretValues({ secrets: { entries: { e: { session: v2 } } } }, [{ secrets: { session: v2 }, session: readSession(v2) }]);
  assert.ok(vals2.includes('cookievalue9999'));
});

test('readSession reads a v2 envelope and a bare (v1) storage state, and refuses anything else', () => {
  const state = { cookies: [{ name: 'a', value: 'b' }], origins: [] };
  const identity = { browser: 'chrome', version: '156.0.8078.73', languages: ['de-DE', 'de'], timezoneId: 'Europe/Berlin' };
  const v2 = readSession(JSON.stringify({ v: 2, storageState: state, identity }));
  assert.deepEqual(v2.storageState, state);
  assert.equal(v2.identity.timezoneId, 'Europe/Berlin');
  const v1 = readSession(JSON.stringify(state));
  assert.deepEqual(v1.storageState, state);
  assert.equal(v1.identity, null);
  assert.equal(readSession('not json'), null);
  assert.equal(readSession(JSON.stringify({ v: 3 })), null);
  assert.equal(readSession(''), null);
});

test('identityFor takes the session identity, else the settings, else en-US; sizes and zones are checked', () => {
  const session = { identity: { version: '156.0.8078.73', languages: ['fr-FR', 'fr'], timezoneId: 'Europe/Paris', screen: { width: 2560, height: 1440 }, viewport: { width: 1400, height: 900 }, deviceScaleFactor: 1 } };
  const a = identityFor({ session, settings: { languages: 'de-DE', timeZone: 'Europe/Berlin' } });
  assert.deepEqual([a.locale, a.timezoneId, a.screen.width, a.viewport.height, a.version], ['fr-FR', 'Europe/Paris', 2560, 900, '156.0.8078.73']);
  const b = identityFor({ session: null, settings: { languages: 'de-DE, de', timeZone: 'Europe/Berlin' } });
  assert.deepEqual([b.languages, b.timezoneId, b.viewport], [['de-DE', 'de'], 'Europe/Berlin', null]);
  const c = identityFor({ session: { identity: { timezoneId: 'Not/AZone', screen: { width: 5, height: 5 }, languages: ['<script>'] } } });
  assert.deepEqual([c.languages, c.screen], [['en-US', 'en'], { width: 1920, height: 1080 }]);
  assert.notEqual(c.timezoneId, 'Not/AZone');
});

test('chrome: choose takes the session version, else its major, else the newest', () => {
  const copies = [{ version: '157.0.1.0' }, { version: '156.0.8078.73' }, { version: '156.0.8078.4' }];
  assert.equal(choose(copies, '156.0.8078.4').version, '156.0.8078.4');
  assert.equal(choose(copies, '156.0.9999.1').version, '156.0.8078.73');
  assert.equal(choose(copies, '150.0.1.1').version, '157.0.1.0');
  assert.equal(choose(copies, null).version, '157.0.1.0');
  assert.equal(choose([], null), null);
  assert.ok(compareVersions('156.0.10.0', '156.0.9.0') > 0);
});

test('chromeArgs never carries a headless, automation or no-sandbox switch', () => {
  const identity = identityFor({ session: null });
  const args = chromeArgs({ dir: '/tmp/p', identity, window: { width: 1920, height: 1080 } });
  for (const bad of ['--headless', '--enable-automation', '--no-sandbox', '--disable-gpu', '--hide-scrollbars', '--mute-audio']) {
    assert.ok(!args.some((a) => a.startsWith(bad)), bad);
  }
  assert.ok(args.includes('--disable-blink-features=AutomationControlled'));
  assert.ok(args.includes('--lang=en-US'));
});
