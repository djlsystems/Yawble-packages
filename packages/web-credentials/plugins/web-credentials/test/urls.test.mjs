import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { entryHosts, readEntries, resolveOnSite, withScheme } from '../src/entries.mjs';

// 22750: a Site URL, Sign-in URL or check URL written without a scheme is given https://, so a row
// saved before Yawble's "format": "url" (or by an older Yawble) works as the form now saves it.

test('withScheme gives a bare address https', () => {
  assert.equal(withScheme('mysite.com'), 'https://mysite.com');
  assert.equal(withScheme('www.mysite.com'), 'https://www.mysite.com');
  assert.equal(withScheme('  mysite.com/login  '), 'https://mysite.com/login');
  assert.equal(withScheme('mysite.com:8443'), 'https://mysite.com:8443');
  assert.equal(withScheme('localhost:3000/x'), 'https://localhost:3000/x');
  assert.equal(withScheme('127.0.0.1'), 'https://127.0.0.1');
});

test('withScheme keeps a scheme as written and leaves a path or junk alone', () => {
  for (const v of ['https://mysite.com', 'http://mysite.com', 'HTTP://MySite.com/', 'javascript:alert(1)', 'ftp://mysite.com',
    'https://', 'not a url', 'foo bar.com', '/login', 'dashboard', 'mysite', '']) {
    assert.equal(withScheme(v), v, v);
  }
  assert.equal(withScheme(undefined), '');
  assert.equal(withScheme(42), '');
});

test('readEntries gives the url columns a scheme, keeps http, and never touches Other hosts', () => {
  const [bare, www, plain] = readEntries({
    config: {
      entries: [
        { name: 'bare', siteUrl: 'mysite.com', signInUrl: 'mysite.com/login', checkUrl: '/dashboard', hosts: 'login.mysite.com', method: 'password' },
        { name: 'www', siteUrl: 'www.mysite.com', checkUrl: 'www.mysite.com/home', hosts: 'https://sso.mysite.com/start/', method: 'password' },
        { name: 'plain', siteUrl: 'http://mysite.com', method: 'password' },
      ],
    },
  });
  assert.equal(bare.siteUrl, 'https://mysite.com');
  assert.equal(bare.signInUrl, 'https://mysite.com/login');
  assert.equal(bare.checkUrl, '/dashboard');
  assert.deepEqual(bare.hosts, ['mysite.com', 'login.mysite.com']);
  assert.equal(bare.written.siteUrl, 'mysite.com', 'the fingerprint is of what was written');
  assert.equal(www.siteUrl, 'https://www.mysite.com');
  assert.equal(www.checkUrl, 'https://www.mysite.com/home');
  assert.deepEqual(www.hosts, ['www.mysite.com', 'sso.mysite.com'], 'a URL in Other hosts is cut to its host');
  assert.equal(plain.siteUrl, 'http://mysite.com');
  assert.ok(resolveOnSite(bare, bare.signInUrl, 'the sign-in URL').url.toString() === 'https://mysite.com/login');
});

test('junk stays junk after the prefix: the site has no hosts and its URLs are refused', () => {
  for (const siteUrl of ['not a url', 'https://', 'foo bar.com', 'javascript:alert(1)']) {
    const [entry] = readEntries({ config: { entries: [{ name: 'j', siteUrl, method: 'password' }] } });
    assert.equal(entry.siteUrl, siteUrl);
    assert.deepEqual(entry.hosts, [], siteUrl);
    assert.deepEqual(entryHosts(withScheme(siteUrl), ''), [], siteUrl);
  }
  const entry = readEntries({ config: { entries: [{ name: 'e', siteUrl: 'mysite.com', signInUrl: 'javascript:alert(1)', method: 'password' }] } })[0];
  assert.ok(resolveOnSite(entry, entry.signInUrl, 'the sign-in URL').refusal);
});

test('the manifest marks Site URL, Sign-in URL and check URL as url columns, and only those', () => {
  const manifest = JSON.parse(readFileSync(new URL('../plugin.json', import.meta.url), 'utf8'));
  const urls = manifest.config.entries.columns.filter((c) => c.format === 'url').map((c) => c.name);
  assert.deepEqual(urls, ['siteUrl', 'signInUrl', 'checkUrl']);
});
