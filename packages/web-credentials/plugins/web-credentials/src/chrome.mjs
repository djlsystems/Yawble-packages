// GOOGLE CHROME, THE BROWSER EVERY SIGN-IN RUNS. A site must see the browser a person uses, so this is
// Google's own stable build, run headed - never a test build or a headless shell - and the same build
// the platform's sign-in page runs (its install-google-chrome.sh: the same index, the same layout).
//
// Where it is looked for, every folder laid out <root>/<version>/chrome:
//   1. the platform's copy, $HARNESS_SIGN_IN_CHROME or /data/google-chrome (read only to us);
//   2. the plugin's own, <plugin workspace>/.web-credentials/chrome (or $WEB_CREDENTIALS_CHROME_ROOT).
// Where it comes from: Google's apt index over HTTPS names the newest stable version, its .deb and the
// file's SHA-256; the download must match, and is unpacked without root (dpkg-deb -x) into the
// plugin's own folder. Its setuid sandbox helper is removed: Chrome runs with its user-namespace
// sandbox. How it stays current: at most once a day a run reads the index, and a newer stable than
// any copy found is installed beside the old one; the newest two of the plugin's own are kept.
import { existsSync, readdirSync, mkdirSync, rmSync, renameSync, createWriteStream, chmodSync } from 'node:fs';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';

const IndexUrl = process.env.WEB_CREDENTIALS_CHROME_INDEX || 'https://dl.google.com/linux/chrome/deb/dists/stable/main/binary-amd64/Packages';
const PoolUrl = process.env.WEB_CREDENTIALS_CHROME_POOL || 'https://dl.google.com/linux/chrome/deb';
const DayMs = 24 * 3600 * 1000;

export function platformRoot() {
  return process.env.HARNESS_SIGN_IN_CHROME || join(process.env.HARNESS_DATA_ROOT || '/data', 'google-chrome');
}

export function ownRoot(stateDir) {
  return process.env.WEB_CREDENTIALS_CHROME_ROOT || join(stateDir, 'chrome');
}

// Compares dotted versions numerically.
export function compareVersions(a, b) {
  const x = String(a).split('.').map(Number);
  const y = String(b).split('.').map(Number);
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    const d = (x[i] || 0) - (y[i] || 0);
    if (d !== 0) return d;
  }
  return 0;
}

const major = (v) => String(v || '').split('.')[0];

// Every installed Chrome under the roots: [{ version, path, root }], newest first.
export function installed(roots) {
  const out = [];
  for (const root of roots) {
    let names = [];
    try {
      names = readdirSync(root);
    } catch {
      continue;
    }
    for (const version of names.filter((n) => /^\d+(\.\d+)+$/.test(n))) {
      const path = join(root, version, 'chrome');
      if (existsSync(path)) out.push({ version, path, root });
    }
  }
  return out.sort((a, b) => compareVersions(b.version, a.version));
}

// The copy to run: the version a saved session's identity names, else one of its major, else the
// newest. Answers null when there is none.
export function choose(copies, wanted) {
  if (copies.length === 0) return null;
  if (wanted) {
    const exact = copies.find((c) => c.version === wanted);
    if (exact) return exact;
    const same = copies.find((c) => major(c.version) === major(wanted));
    if (same) return same;
  }
  return copies[0];
}

// The newest stable in Google's index: { version, file, sha256 } or null.
export async function newestStable(fetchImpl = fetch) {
  let text;
  try {
    const response = await fetchImpl(IndexUrl, { signal: AbortSignal.timeout(60000) });
    if (!response.ok) return null;
    text = await response.text();
  } catch {
    return null;
  }
  const block = text.split(/\n\n/).find((b) => /^Package: google-chrome-stable$/m.test(b));
  if (!block) return null;
  const version = block.match(/^Version: (\d[\d.]*)-/m)?.[1];
  const file = block.match(/^Filename: (pool\/[A-Za-z0-9._/-]*\.deb)$/m)?.[1];
  const sha256 = block.match(/^SHA256: ([0-9a-f]{64})$/m)?.[1];
  return version && file && sha256 ? { version, file, sha256 } : null;
}

// Downloads, checks and unpacks one stable build into <root>/<version>. Answers its path.
async function install(root, stable, progress) {
  mkdirSync(root, { recursive: true, mode: 0o700 });
  const work = join(root, `.download-${process.pid}`);
  rmSync(work, { recursive: true, force: true });
  mkdirSync(join(work, 'x'), { recursive: true });
  try {
    progress(`Fetching Google Chrome ${stable.version} (once; later runs reuse it)…`);
    const response = await fetch(`${PoolUrl}/${stable.file}`, { signal: AbortSignal.timeout(900000) });
    if (!response.ok || !response.body) throw new Error(`the download answered ${response.status}`);
    const hash = createHash('sha256');
    const deb = join(work, 'chrome.deb');
    const file = createWriteStream(deb);
    let got = 0;
    let told = Date.now();
    for await (const chunk of response.body) {
      hash.update(chunk);
      got += chunk.length;
      if (!file.write(chunk)) await new Promise((r) => file.once('drain', r));
      if (Date.now() - told > 10000) {
        told = Date.now();
        progress(`Fetching Google Chrome ${stable.version}: ${Math.round(got / 1048576)} MB…`);
      }
    }
    await new Promise((resolve, reject) => file.end((e) => (e ? reject(e) : resolve())));
    if (hash.digest('hex') !== stable.sha256) throw new Error('the download did not match the checksum in Google’s index');
    const unpack = spawnSync('dpkg-deb', ['-x', deb, join(work, 'x')], { stdio: 'ignore' });
    const chrome = join(work, 'x', 'opt', 'google', 'chrome');
    if (unpack.status !== 0 || !existsSync(join(chrome, 'chrome'))) throw new Error('the package could not be unpacked');
    // No setuid helper: Chrome runs with its user-namespace sandbox, never the setuid one.
    rmSync(join(chrome, 'chrome-sandbox'), { force: true });
    rmSync(join(root, stable.version), { recursive: true, force: true });
    renameSync(chrome, join(root, stable.version));
    chmodSync(join(root, stable.version), 0o755);
  } finally {
    rmSync(work, { recursive: true, force: true });
  }
  // The newest two are kept, so a copy a saved session still names is not pulled at once.
  const own = installed([root]);
  for (const old of own.slice(2)) rmSync(join(root, old.version), { recursive: true, force: true });
  return join(root, stable.version, 'chrome');
}

// The Chrome a run uses: { version, path } or throws with a sentence. `meta` is the plugin's state
// (its chromeCheckedAt is kept there), `wanted` the version a saved session's identity names.
export async function chromeFor({ stateDir, meta, wanted, progress = () => {} }) {
  const own = ownRoot(stateDir);
  const roots = [platformRoot(), own];
  let copies = installed(roots);
  const due = !meta.chromeCheckedAt || Date.now() - Date.parse(meta.chromeCheckedAt) >= DayMs;
  if (copies.length === 0 || due) {
    const stable = await newestStable();
    if (stable) {
      meta.chromeCheckedAt = new Date().toISOString();
      if (!copies.some((c) => compareVersions(c.version, stable.version) >= 0)) {
        try {
          await install(own, stable, progress);
        } catch (e) {
          if (copies.length === 0) throw new Error(`Google Chrome could not be installed: ${e.message}`);
        }
        copies = installed(roots);
      }
    }
  }
  const pick = choose(copies, wanted);
  if (!pick) throw new Error('no Google Chrome is installed and Google’s index could not be reached to fetch one');
  return pick;
}
