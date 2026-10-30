import { test } from 'node:test';
import assert from 'node:assert/strict';
import { totp } from '../src/totp.mjs';
import { entryHosts, hostAllowed, resolveOnSite, readEntries, secretValues, readSession, stateSecretValues } from '../src/entries.mjs';
import { identityFor, withBaseLanguages } from '../src/profile.mjs';
import { readFileSync } from 'node:fs';
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

test('out: an escape right before or after a 4-7 character value is a token boundary, as the platform ValueRedactor', () => {
  const lines = [];
  const out = makeOut((l) => lines.push(l));
  out.setSecrets([{ token: 'k7q2z' }]);
  out.result(true, 'a \\u0022k7q2z\\u0022 b %22k7q2z%22 c \\nk7q2z\\t d %3dK7Q2Z e \\u00e9k7q2z9 f u0022k7q2z');
  assert.equal(JSON.parse(lines[0]).output, 'a \\u0022[redacted]\\u0022 b %22[redacted]%22 c \\n[redacted]\\t d %3d[redacted] e \\u00e9k7q2z9 f u0022k7q2z');
  // The JSON-escaped spellings of a value are redacted too: "a+b<c" printed by .NET's default encoder.
  out.setSecrets(['ab+cd<ef"gh', { token: 'q"1+' }]);
  out.result(true, 'x ab\\u002Bcd\\u003Cef\\u0022gh x ab+cd<ef\\"gh y q\\u00221\\u002B');
  assert.equal(JSON.parse(lines[1]).output, 'x [redacted] x [redacted] y [redacted]');
});

test('stateSecretValues: "undefined" (9 characters) as a JSON string is a settings word, never redacted; a URL-decoded value joins too', () => {
  const vals = stateSecretValues({
    cookies: [{ value: 'a%2Fb%3Dcd' }],
    origins: [{ localStorage: [{ value: JSON.stringify({ theme: 'undefined', other: 'UNDEFINED', k: 'secret-part-1' }) }] }],
  });
  const all = vals.map((v) => (typeof v === 'string' ? v : v.token));
  assert.ok(!all.some((v) => v.toLowerCase() === 'undefined'), 'undefined is in the set');
  assert.ok(all.includes('secret-part-1'));
  assert.ok(all.includes('a%2Fb%3Dcd') && all.includes('a/b=cd'), all.join(','));
});

test('P16: a session value is redacted in its URL-decoded form too, and after an escape such as %3D or %26', () => {
  const lines = [];
  const out = makeOut((l) => lines.push(l));
  out.setSecrets(stateSecretValues({ cookies: [{ value: 'q7%2Fwz9' }, { value: 'k9x4z' }, { value: 'zz%E0%A4' }] }));
  out.result(true, 'enc=q7%2Fwz9 enc-decoded=q7/wz9 pct=%3Dk9x4z url=?a=1%26mid%3Dk9x4z%26 inword=abk9x4zq');
  assert.equal(JSON.parse(lines[0]).output, 'enc=[redacted] enc-decoded=[redacted] pct=%3D[redacted] url=?a=1%26mid%3D[redacted]%26 inword=abk9x4zq');
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
  assert.equal(c.timezoneId, 'UTC');
});

test('identityFor: Language and Time zone empty, missing or unusable (an older install) are en-US and UTC, never the machine zone', () => {
  for (const settings of [undefined, {}, { languages: [], timeZone: '' }, { languages: null, timeZone: null }, { languages: ['<x>', 'en_US!'], timeZone: 'Mars/Olympus' }]) {
    const id = identityFor({ session: null, settings });
    assert.deepEqual([id.locale, id.languages, id.timezoneId], ['en-US', ['en-US', 'en'], 'UTC'], JSON.stringify(settings));
  }
  // The manifest's defaults, as the Host sends them to a member that set nothing.
  const d = identityFor({ session: null, settings: { languages: ['en-US'], timeZone: 'UTC' } });
  assert.deepEqual([d.locale, d.languages, d.timezoneId], ['en-US', ['en-US', 'en'], 'UTC']);
  const gb = identityFor({ session: null, settings: { languages: ['en-GB'], timeZone: 'Europe/London' } });
  assert.deepEqual([gb.locale, gb.languages, gb.timezoneId], ['en-GB', ['en-GB', 'en'], 'Europe/London']);
});

test('withBaseLanguages adds a regional language its base after the last of that base, unless listed', () => {
  assert.deepEqual(withBaseLanguages(['en-US']), ['en-US', 'en']);
  assert.deepEqual(withBaseLanguages(['en-US', 'en-GB', 'de-DE']), ['en-US', 'en-GB', 'en', 'de-DE', 'de']);
  assert.deepEqual(withBaseLanguages(['en-GB', 'en', 'fr']), ['en-GB', 'en', 'fr']);
  assert.deepEqual(withBaseLanguages(['zh-TW']), ['zh-TW', 'zh']);
});

test('the manifest: Language and Time zone are optional, under Advanced, default en-US and UTC, and choose from the Host\'s locale and time zone sets', () => {
  const manifest = JSON.parse(readFileSync(new URL('../plugin.json', import.meta.url), 'utf8'));
  const { languages, timeZone } = manifest.config;

  assert.equal(languages.type, 'list');
  assert.equal(languages.options, 'locale');
  assert.deepEqual(languages.default, ['en-US']);
  assert.equal(languages.label, 'Language');
  assert.equal(timeZone.type, 'string');
  assert.equal(timeZone.options, 'timeZone');
  assert.equal(timeZone.default, 'UTC');
  assert.equal(timeZone.label, 'Time zone');
  for (const field of [languages, timeZone]) {
    assert.notEqual(field.required, true);
    assert.equal(field.advanced, true);
    assert.equal(field.enum, undefined);
    assert.equal(field.setBy, 'person');
  }
  // The defaults are a valid locale and zone, and the descriptions say them.
  assert.deepEqual(Intl.getCanonicalLocales(languages.default), ['en-US']);
  assert.equal(new Intl.DateTimeFormat('en-US', { timeZone: timeZone.default }).resolvedOptions().timeZone, 'UTC');
  assert.match(languages.description, /Default: English \(United States\), en-US\.$/);
  assert.match(timeZone.description, /Default: UTC\.$/);
  assert.doesNotMatch(timeZone.description, /machine/);
  // Nothing else in the plugin's settings is required.
  assert.deepEqual(Object.entries(manifest.config).filter(([, f]) => f.required).map(([n]) => n), []);
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
