// Fetching and updating Google Chrome, against a local fake of Google's apt index and pool serving a
// tiny package laid out as Google's is (opt/google/chrome/chrome, and the setuid chrome-sandbox).
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdirSync, writeFileSync, readFileSync, existsSync, readdirSync, chmodSync } from 'node:fs';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { workspace, cleanup } from './harness.mjs';
import { chromeFor } from '../src/chrome.mjs';

let server, base, scratch;
const debs = new Map(); // pool path -> bytes
let index = '';

function makeDeb(version) {
  const root = join(scratch, `pkg-${version}`);
  mkdirSync(join(root, 'DEBIAN'), { recursive: true });
  mkdirSync(join(root, 'opt/google/chrome'), { recursive: true });
  writeFileSync(join(root, 'DEBIAN/control'), `Package: google-chrome-stable\nVersion: ${version}-1\nArchitecture: amd64\nMaintainer: test\nDescription: test\n`);
  writeFileSync(join(root, 'opt/google/chrome/chrome'), `#!/bin/sh\necho ${version}\n`);
  chmodSync(join(root, 'opt/google/chrome/chrome'), 0o755);
  writeFileSync(join(root, 'opt/google/chrome/chrome-sandbox'), 'setuid helper');
  chmodSync(join(root, 'DEBIAN'), 0o755);
  const out = join(scratch, `chrome-${version}.deb`);
  const r = spawnSync('dpkg-deb', ['--root-owner-group', '-b', root, out], { stdio: 'ignore' });
  assert.equal(r.status, 0, 'dpkg-deb could build the test package');
  return readFileSync(out);
}

// Publishes `version` as the newest stable; `corrupt` serves bytes that do not match the index.
function publish(version, { corrupt = false } = {}) {
  const bytes = makeDeb(version);
  const file = `pool/main/g/google-chrome-stable/google-chrome-stable_${version}-1_amd64.deb`;
  debs.set('/' + file, corrupt ? Buffer.concat([bytes, Buffer.from('x')]) : bytes);
  const sha = createHash('sha256').update(bytes).digest('hex');
  index = `Package: google-chrome-beta\nVersion: 999.0.0.0-1\nFilename: pool/beta.deb\nSHA256: ${'0'.repeat(64)}\n\nPackage: google-chrome-stable\nVersion: ${version}-1\nFilename: ${file}\nSHA256: ${sha}\n\n`;
}

before(async () => {
  scratch = workspace();
  server = createServer((req, res) => {
    if (req.url === '/dists/Packages') return res.end(index);
    const bytes = debs.get(req.url);
    if (!bytes) return (res.statusCode = 404), res.end();
    res.end(bytes);
  });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  base = `http://127.0.0.1:${server.address().port}`;
});
after(async () => {
  await new Promise((r) => server.close(r));
  cleanup(scratch);
});

function withEnv(env, fn) {
  const saved = {};
  for (const k of Object.keys(env)) (saved[k] = process.env[k]), (process.env[k] = env[k]);
  return fn().finally(() => {
    for (const k of Object.keys(env)) if (saved[k] === undefined) delete process.env[k];
    else process.env[k] = saved[k];
  });
}

test('chrome: fetched from the index into the plugin’s own folder, checksum-checked, setuid helper removed; a day later a newer stable is added and the newest two kept', async () => {
  const ws = workspace();
  const own = join(ws, 'chrome');
  try {
    await withEnv({ WEB_CREDENTIALS_CHROME_INDEX: `${base}/dists/Packages`, WEB_CREDENTIALS_CHROME_POOL: base, WEB_CREDENTIALS_CHROME_ROOT: own, HARNESS_SIGN_IN_CHROME: join(ws, 'no-platform-copy') }, async () => {
      const meta = {};
      publish('150.0.1.0');
      const said = [];
      const a = await chromeFor({ stateDir: ws, meta, progress: (s) => said.push(s) });
      assert.equal(a.version, '150.0.1.0');
      assert.equal(a.path, join(own, '150.0.1.0', 'chrome'));
      assert.ok(!existsSync(join(own, '150.0.1.0', 'chrome-sandbox')), 'no setuid helper');
      assert.ok(said.some((s) => s.includes('Fetching Google Chrome 150.0.1.0')));
      assert.ok(meta.chromeCheckedAt);

      // Within the day, a newer stable is not looked for.
      publish('151.0.1.0');
      assert.equal((await chromeFor({ stateDir: ws, meta })).version, '150.0.1.0');

      // A day on, it is installed beside the old one.
      meta.chromeCheckedAt = new Date(Date.now() - 25 * 3600 * 1000).toISOString();
      assert.equal((await chromeFor({ stateDir: ws, meta })).version, '151.0.1.0');
      // A session made on 150 still gets 150 while it is kept.
      assert.equal((await chromeFor({ stateDir: ws, meta, wanted: '150.0.1.0' })).version, '150.0.1.0');

      meta.chromeCheckedAt = null;
      publish('152.0.1.0');
      await chromeFor({ stateDir: ws, meta });
      assert.deepEqual(readdirSync(own).sort(), ['151.0.1.0', '152.0.1.0']);
    });
  } finally {
    cleanup(ws);
  }
});

test('chrome: a download that does not match the index is refused and nothing is installed', async () => {
  const ws = workspace();
  try {
    await withEnv({ WEB_CREDENTIALS_CHROME_INDEX: `${base}/dists/Packages`, WEB_CREDENTIALS_CHROME_POOL: base, WEB_CREDENTIALS_CHROME_ROOT: join(ws, 'chrome'), HARNESS_SIGN_IN_CHROME: join(ws, 'none') }, async () => {
      publish('160.0.0.1', { corrupt: true });
      await assert.rejects(chromeFor({ stateDir: ws, meta: {} }), /checksum/);
      assert.deepEqual(existsSync(join(ws, 'chrome')) ? readdirSync(join(ws, 'chrome')) : [], []);
    });
  } finally {
    cleanup(ws);
  }
});

test('chrome: the platform’s copy is used when it is current, and nothing is fetched', async () => {
  const ws = workspace();
  try {
    const platform = join(ws, 'platform');
    mkdirSync(join(platform, '170.0.0.1'), { recursive: true });
    writeFileSync(join(platform, '170.0.0.1', 'chrome'), '#!/bin/sh\n');
    await withEnv({ WEB_CREDENTIALS_CHROME_INDEX: `${base}/dists/Packages`, WEB_CREDENTIALS_CHROME_POOL: base, WEB_CREDENTIALS_CHROME_ROOT: join(ws, 'chrome'), HARNESS_SIGN_IN_CHROME: platform }, async () => {
      publish('170.0.0.1');
      const c = await chromeFor({ stateDir: ws, meta: {} });
      assert.equal(c.path, join(platform, '170.0.0.1', 'chrome'));
      assert.ok(!existsSync(join(ws, 'chrome')));
    });
  } finally {
    cleanup(ws);
  }
});
