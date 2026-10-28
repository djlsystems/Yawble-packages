import { test } from 'node:test';
import assert from 'node:assert/strict';
import { totp } from '../src/totp.mjs';
import { entryHosts, hostAllowed, resolveOnSite, readEntries, secretValues } from '../src/entries.mjs';
import { pruneState } from '../src/browser.mjs';
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

test('secretValues pulls cookie and local-storage values out of a saved session', () => {
  const session = JSON.stringify({ cookies: [{ value: 'cookievalue1234' }], origins: [{ localStorage: [{ value: 'lsvalue5678' }] }] });
  const vals = secretValues({ secrets: { entries: { e: { session } } } }, [{ secrets: { session } }]);
  assert.ok(vals.includes('cookievalue1234'));
  assert.ok(vals.includes('lsvalue5678'));
});
