// The browser as a site sees it: a fake-site probe page reports what its scripts can read and the
// server keeps the headers it was sent. Nothing headless or automated may show; the headers agree with
// the scripts; a v2 session's identity is applied; the row's profile comes back on the next run; and
// typing arrives one key at a time, paced.
import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { startFakeSite, USERNAME, PASSWORD } from './fakeSite.mjs';
import { runPlugin, workspace, cleanup, listFilesUnder } from './harness.mjs';
import { withEntryBrowser } from '../src/browser.mjs';
import { readEntries } from '../src/entries.mjs';
import { loadState, stateDir } from '../src/state.mjs';
import { installed, platformRoot, ownRoot } from '../src/chrome.mjs';

let site;
before(async () => {
  site = await startFakeSite();
});
after(async () => {
  await site.close();
});

function row(over = {}) {
  return { name: 'prod', siteUrl: site.origin, method: 'password', signInUrl: '/login', checkUrl: '/dashboard', checkText: 'Dashboard', username: USERNAME, verifyEvery: 24, active: true, ...over };
}

// Runs `verify prod` then `check prod /probe` in one run; answers the run and the probe's visit.
async function probe(ws, { entry = row(), secrets = { password: PASSWORD }, settings } = {}) {
  const before = site.probes.length;
  const r = await runPlugin({
    entries: [entry],
    secrets: { entries: { prod: secrets } },
    settings,
    work: [
      { source: 'T/Manager', instruction: 'verify prod' },
      { source: 'T/Manager', instruction: 'check prod /probe' },
    ],
    workingDirectory: ws,
  });
  assert.equal(r.result.ok, true, r.result.output);
  assert.equal(site.probes.length, before + 1, r.result.output);
  const visit = site.probes.at(-1);
  assert.ok(visit.report, 'the probe page reported');
  return { r, visit };
}

const brandsHeader = (brands) => brands.map((b) => `"${b.brand}";v="${b.version}"`).join(', ');

test('the probe sees an ordinary headed Google Chrome: nothing headless or automated, headers agree with scripts', async () => {
  const ws = workspace();
  try {
    const { visit } = await probe(ws, { settings: { languages: 'en-GB, en', timeZone: 'Europe/London' } });
    const { report, headers, reportHeaders } = visit;

    // Nothing says headless, anywhere a site looks.
    const everything = JSON.stringify({ report, headers, reportHeaders });
    assert.doesNotMatch(everything, /headless/i);
    assert.equal(headers['user-agent'], report.userAgent);
    assert.match(report.userAgent, /^Mozilla\/5\.0 \(X11; Linux x86_64\) AppleWebKit\/537\.36 \(KHTML, like Gecko\) Chrome\/\d+\.0\.0\.0 Safari\/537\.36$/);

    // Client hints: the low-entropy headers, and after Accept-CH the high-entropy ones, are the
    // scripts' own values; the brand is Google Chrome; the platform is the user agent's.
    assert.equal(headers['sec-ch-ua'], brandsHeader(report.uaData.brands));
    assert.ok(report.uaData.brands.some((b) => b.brand === 'Google Chrome'), JSON.stringify(report.uaData.brands));
    assert.equal(headers['sec-ch-ua-platform'], `"${report.uaData.platform}"`);
    assert.equal(report.uaData.platform, 'Linux');
    assert.equal(headers['sec-ch-ua-mobile'], '?0');
    assert.equal(report.uaData.mobile, false);
    assert.equal(reportHeaders['sec-ch-ua-full-version-list'], brandsHeader(report.uaHigh.fullVersionList));
    assert.equal(reportHeaders['sec-ch-ua-platform-version'], `"${report.uaHigh.platformVersion}"`);
    assert.equal(reportHeaders['sec-ch-ua-arch'], `"${report.uaHigh.architecture}"`);
    const major = report.userAgent.match(/Chrome\/(\d+)/)[1];
    const chrome = report.uaHigh.fullVersionList.find((b) => b.brand === 'Google Chrome');
    assert.equal(chrome.version.split('.')[0], major);

    // No automation tells.
    assert.equal(report.webdriver, false);
    assert.equal(report.pluginsLength, 5);
    assert.equal(report.chromeObject, true);

    // Ordinary desktop values.
    assert.ok(report.webgl && report.webgl.vendor && report.webgl.renderer, JSON.stringify(report.webgl));
    assert.doesNotMatch(`${report.webgl.vendor} ${report.webgl.renderer}`, /swiftshader/i);
    assert.deepEqual([report.screen.width, report.screen.height], [1920, 1080]);
    assert.equal(report.screen.colorDepth, 24);
    assert.notDeepEqual([report.viewport.width, report.viewport.height], [1280, 720]);
    assert.ok(report.viewport.width > 1800 && report.viewport.height > 900, JSON.stringify(report.viewport));
    assert.ok(report.outer.height > report.viewport.height, 'the window has its tabs and address bar');
    assert.equal(report.devicePixelRatio, 1);

    // The person's languages and time zone, from the plugin's settings.
    assert.deepEqual(report.languages, ['en-GB', 'en']);
    assert.match(headers['accept-language'], /^en-GB,en;q=0\.9$/);
    assert.equal(report.timeZone, 'Europe/London');
  } finally {
    cleanup(ws);
  }
});

test('a v2 session envelope: its identity (languages, time zone, screen, viewport, build) is the browser’s', async () => {
  const ws = workspace();
  try {
    const copies = installed([platformRoot(), ownRoot(stateDir(ws))]);
    const version = copies.length ? copies[0].version : '156.0.0.0';
    const identity = {
      browser: 'chrome',
      version,
      userAgent: `Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/${version.split('.')[0]}.0.0.0 Safari/537.36`,
      userAgentMetadata: { brands: [], fullVersionList: [], platform: 'Linux', platformVersion: '6.0.0', architecture: 'x86', model: '', mobile: false },
      locale: 'de-DE',
      languages: ['de-DE', 'de', 'en'],
      timezoneId: 'Europe/Berlin',
      screen: { width: 2560, height: 1440 },
      viewport: { width: 1600, height: 900 },
      deviceScaleFactor: 1,
    };
    const session = JSON.stringify({ v: 2, storageState: site.liveSession(), identity });
    const entry = row({ method: 'interactive', checkUrl: '/dashboard' });
    const { r, visit } = await probe(ws, { entry, secrets: { session } });
    const { report, headers } = visit;
    assert.deepEqual(report.languages, ['de-DE', 'de', 'en']);
    assert.match(headers['accept-language'], /^de-DE,de;q=0\.9,en;q=0\.8$/);
    assert.equal(report.timeZone, 'Europe/Berlin');
    assert.deepEqual([report.screen.width, report.screen.height], [2560, 1440]);
    assert.deepEqual(report.viewport, { width: 1600, height: 900 });
    assert.equal(report.userAgent, identity.userAgent);
    assert.equal(report.uaHigh.fullVersionList.find((b) => b.brand === 'Google Chrome').version, version);
    assert.equal(report.webdriver, false);
    // The session itself never shows: not its cookie, in any record.
    const cookie = JSON.parse(session).storageState.cookies[0].value;
    assert.ok(!r.lines.join('\n').includes(cookie));
  } finally {
    cleanup(ws);
  }
});

test('the row’s browser comes back: a mark left in run 1 is there in run 2 (same profile)', async () => {
  const ws = workspace();
  try {
    const first = await probe(ws);
    assert.equal(first.visit.report.markBefore, null);
    assert.equal(first.visit.cookieMark, null);
    const second = await probe(ws);
    const n = site.probes.indexOf(first.visit) + 1;
    assert.equal(second.visit.report.markBefore, `visit-${n}`);
    assert.equal(second.visit.cookieMark, `visit-${n}`);
    // Run 2 was still signed in: the browser came back signed in, so nothing was typed again.
    const profiles = readdirSync(join(ws, '.web-credentials', 'profiles'));
    assert.equal(profiles.filter((p) => /^[0-9a-f]{32}$/.test(p)).length, 1);
    // A row that is gone takes its profile with it.
    await runPlugin({ entries: [row({ name: 'other' })], secrets: {}, work: [{ source: 'T/Manager', instruction: 'list' }], workingDirectory: ws });
    assert.equal(readdirSync(join(ws, '.web-credentials', 'profiles')).length, 0);
  } finally {
    cleanup(ws);
  }
});

test('typing is one key at a time and paced like a person, never an instant fill', async () => {
  const ws = workspace();
  try {
    const before = site.keyTimes.length;
    const r = await runPlugin({ entries: [row()], secrets: { entries: { prod: { password: PASSWORD } } }, work: [{ source: 'T/Manager', instruction: 'verify prod' }], workingDirectory: ws });
    assert.equal(r.rowStatus.at(-1).values.status, 'Verified');
    const keys = site.keyTimes[before];
    assert.ok(keys, 'the sign-in page recorded its keys');
    for (const [field, value] of [['email', USERNAME], ['password', PASSWORD]]) {
      const times = keys[field];
      assert.equal(times.length, value.length, `${field}: a keydown per character`);
      const gaps = times.slice(1).map((t, i) => t - times[i]);
      const mean = gaps.reduce((a, b) => a + b, 0) / gaps.length;
      const spread = Math.sqrt(gaps.reduce((a, b) => a + (b - mean) ** 2, 0) / gaps.length);
      // Even at the tests' faster pace (0.3), keys are tens of milliseconds apart and uneven.
      assert.ok(Math.min(...gaps) >= 10, `${field}: shortest gap ${Math.min(...gaps)}ms`);
      assert.ok(mean >= 20, `${field}: mean gap ${mean}ms`);
      assert.ok(spread >= 3, `${field}: gaps vary (${spread}ms)`);
    }
  } finally {
    cleanup(ws);
  }
});

test('Chrome runs with its own sandbox: its children run in their own PID namespace under their own seccomp filter', async () => {
  const ws = workspace();
  try {
    const request = { config: { entries: [row()] }, secrets: { entries: { prod: { password: PASSWORD } } } };
    const [entry] = readEntries(request);
    const dir = stateDir(ws);
    const state = loadState(dir);
    const run = { stateDir: dir, state, member: 'T/Creds', settings: {}, progress: () => {} };
    await withEntryBrowser(entry, run, async (page) => {
      await page.goto(site.origin + '/');
      // Every process of this browser: the one started on the row's profile, and its descendants.
      const procs = new Map();
      for (const pid of readdirSync('/proc').filter((p) => /^\d+$/.test(p))) {
        try {
          const status = readFileSync(`/proc/${pid}/status`, 'utf8');
          const field = (name) => status.match(new RegExp(`^${name}:\\s*(.*)$`, 'm'))?.[1] ?? '';
          procs.set(pid, {
            ppid: field('PPid'),
            nspid: field('NSpid').split(/\s+/).length,
            filters: Number(field('Seccomp_filters') || 0),
            cmd: readFileSync(`/proc/${pid}/cmdline`, 'utf8').split('\0'),
          });
        } catch {
          // gone
        }
      }
      const root = [...procs].find(([, p]) => p.cmd.includes(`--user-data-dir=${join(dir, 'profiles')}`) || p.cmd.some((a) => a.startsWith(`--user-data-dir=${join(dir, 'profiles')}/`)));
      assert.ok(root, 'the row’s browser is running');
      const [rootPid, browser] = root;
      assert.ok(!browser.cmd.includes('--no-sandbox') && !browser.cmd.some((a) => a.startsWith('--headless')), browser.cmd.join(' '));
      const family = new Set([rootPid]);
      for (let grew = true; grew; ) {
        grew = false;
        for (const [pid, p] of procs) if (!family.has(pid) && family.has(p.ppid)) (family.add(pid), (grew = true));
      }
      // Chrome's namespace sandbox puts its sandboxed children in a PID namespace of their own (one
      // more NSpid level) under a seccomp-bpf filter of their own (one more filter).
      const sandboxed = [...family].map((pid) => procs.get(pid)).filter((p) => p.nspid > browser.nspid && p.filters > browser.filters);
      assert.ok(sandboxed.length >= 2, `sandboxed children: ${sandboxed.length} of ${family.size - 1}`);
    });
  } finally {
    cleanup(ws);
  }
});

test('no secret, session or storage state lands in a record, the documents or the profile’s own notes', async () => {
  const ws = workspace();
  try {
    const live = site.liveSession();
    const session = JSON.stringify({ v: 2, storageState: live, identity: { languages: ['en-US'], timezoneId: 'UTC' } });
    const { r } = await probe(ws, { entry: row({ method: 'interactive' }), secrets: { session } });
    const cookie = live.cookies[0].value;
    const haystack = [r.lines.join('\n')];
    for (const f of listFilesUnder(join(ws, '.web-credentials')).filter((f) => !f.includes('/profiles/') || f.endsWith('.json'))) {
      if (f.endsWith('.json')) haystack.push(readFileSync(f, 'utf8'));
    }
    for (const text of haystack) {
      assert.ok(!text.includes(cookie), 'the session cookie leaked');
      assert.ok(!text.includes('storageState'), 'a storage state leaked');
    }
  } finally {
    cleanup(ws);
  }
});
