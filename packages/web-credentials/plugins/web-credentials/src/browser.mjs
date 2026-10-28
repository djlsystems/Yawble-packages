// THE BROWSER A SIGN-IN RUNS IN: Google Chrome (chrome.mjs), headed on its own virtual screen
// (display.mjs), in the row's own persistent profile with the row's identity (profile.mjs), driven by
// playwright-core over its pipe. A site must not tell it from the person on their own Chrome, so:
//   - none of Playwright's default switches (ignoreDefaultArgs: true): no --enable-automation, no
//     --headless, no --no-sandbox, no fixed viewport; only the arguments below, none a site can see;
//   - Chrome's OWN SANDBOX ON (chromiumSandbox: true, never --no-sandbox): it will not start without it;
//   - the identity's languages (Chrome's preference and --lang), time zone (TZ) and screen are the
//     browser's own, not an emulation laid over it, so headers and scripts agree.
import { chromium } from 'playwright-core';
import { chromeFor } from './chrome.mjs';
import { startDisplay } from './display.mjs';
import { hostAllowed } from './entries.mjs';
import { profileKey, profileDir, loadMeta, saveMeta, identityFor, preparePreferences, profileBusy, sessionDigest } from './profile.mjs';

// Chrome's arguments: only what makes it an ordinary browser on this machine.
//   --disable-blink-features=AutomationControlled  a browser with a DevTools pipe says
//       navigator.webdriver = true; a person's Chrome says false.
//   --test-type  no warning bar about that switch, which would take height from the page.
//   --use-angle=gl --ignore-gpu-blocklist  WebGL through the machine's own OpenGL, as a desktop's.
export function chromeArgs({ dir, identity, window }) {
  const args = [
    '--remote-debugging-pipe',
    `--user-data-dir=${dir}`,
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-blink-features=AutomationControlled',
    '--test-type',
    '--use-angle=gl',
    '--ignore-gpu-blocklist',
    '--disable-breakpad',
    '--password-store=basic',
    '--disable-dev-shm-usage',
    `--lang=${identity.locale}`,
    `--window-size=${window.width},${window.height}`,
    '--window-position=0,0',
  ];
  if (identity.deviceScaleFactor !== 1) args.push(`--force-device-scale-factor=${identity.deviceScaleFactor}`);
  args.push('about:blank');
  return args;
}

// Sizes the window so the page's viewport is the identity's; the window keeps its tabs and address
// bar, as a person's does.
async function fit(context, page, viewport) {
  const cdp = await context.newCDPSession(page);
  try {
    const { windowId, bounds } = await cdp.send('Browser.getWindowForTarget');
    const metrics = await cdp.send('Page.getLayoutMetrics');
    const v = metrics.cssLayoutViewport;
    await cdp.send('Browser.setWindowBounds', {
      windowId,
      bounds: { width: bounds.width + viewport.width - v.clientWidth, height: bounds.height + viewport.height - v.clientHeight },
    });
  } finally {
    await cdp.detach().catch(() => {});
  }
}

// What the browser says it is, for the row's kept identity.
async function browserVersion(context, page) {
  const cdp = await context.newCDPSession(page);
  try {
    const v = await cdp.send('Browser.getVersion');
    return { version: v.product.split('/')[1], userAgent: v.userAgent };
  } finally {
    await cdp.detach().catch(() => {});
  }
}

// THE HOST LIMIT, ENFORCED BEFORE A REQUEST LEAVES THE BROWSER. Every page or frame load (a
// navigation, and each hop of a redirect) to a host that is not one of the entry's is failed in the
// browser, so the other host never sees a request, a cookie or a Referer from it. Playwright's own
// route() is not enough: it sees only the first URL of a redirect. Chrome's Fetch domain pauses every
// hop. A page's images, scripts and styles from other hosts (a CDN) still load: a sign-in page needs
// them, and they are not where a redirect takes the browser - the limit is on where the browser goes
// (pages and frames), not a network allow-list. `blocked` names each host the PAGE was stopped from
// going to (the main frame: a redirect, a script or a meta refresh); `framesBlocked` each host a frame
// inside it was stopped from loading, which leaves the page itself where it is.
async function guardHosts(context, page, entry) {
  const blocked = [];
  const framesBlocked = [];
  const cdp = await context.newCDPSession(page);
  const { frameTree } = await cdp.send('Page.getFrameTree');
  const main = frameTree.frame.id;
  cdp.on('Fetch.requestPaused', (e) => {
    let off = null;
    if (e.resourceType === 'Document') {
      try {
        const u = new URL(e.request.url);
        if ((u.protocol === 'http:' || u.protocol === 'https:') && !hostAllowed(entry, u)) off = u.hostname;
      } catch {
        // not a URL Chrome would fetch over the network
      }
    }
    if (off) {
      const list = e.frameId && e.frameId !== main ? framesBlocked : blocked;
      if (!list.includes(off)) list.push(off);
      cdp.send('Fetch.failRequest', { requestId: e.requestId, errorReason: 'BlockedByClient' }).catch(() => {});
    } else {
      cdp.send('Fetch.continueRequest', { requestId: e.requestId }).catch(() => {});
    }
  });
  await cdp.send('Fetch.enable', { patterns: [{ urlPattern: '*', resourceType: 'Document', requestStage: 'Request' }] });
  return { blocked, framesBlocked };
}

// Opens the row's browser and hands `fn` its page and context; always closes them. `run` is the run's
// { stateDir, state, member, settings, progress }. A saved session (entry.session, parsed) is applied
// to the profile when it is new - the person signed in again - and otherwise the profile's own cookies
// carry on from the last run. For a password or token row, an edit to what the person wrote clears
// the profile's cookies, so the next sign-in tries the new values.
export async function withEntryBrowser(entry, run, fn) {
  const key = profileKey(run.member, entry.key);
  const dir = profileDir(run.stateDir, key);
  const meta = loadMeta(run.stateDir, key);
  if (profileBusy(dir)) throw new Error('its browser is already open in another run; try again shortly');
  const session = entry.session;
  const identity = identityFor({ session, settings: run.settings, kept: meta.identity });
  const chrome = await chromeFor({ stateDir: run.stateDir, meta: run.state, wanted: session?.identity?.version, progress: run.progress });

  const screen = identity.screen;
  const display = await startDisplay(run.stateDir, screen.width, screen.height);
  let context;
  try {
    preparePreferences(dir, identity.languages);
    const window = identity.viewport ? { width: Math.min(screen.width, identity.viewport.width + 16), height: Math.min(screen.height, identity.viewport.height + 120) } : screen;
    context = await chromium.launchPersistentContext(dir, {
      executablePath: chrome.path,
      headless: false,
      chromiumSandbox: true,
      ignoreDefaultArgs: true,
      args: chromeArgs({ dir, identity, window }),
      viewport: null,
      env: {
        ...process.env,
        DISPLAY: display.display,
        XAUTHORITY: display.authority,
        TZ: identity.timezoneId,
        LANG: 'en_US.UTF-8',
        LANGUAGE: identity.locale.replace('-', '_'),
      },
      timeout: 60000,
    });
    context.setDefaultTimeout(20000);
    const page = context.pages()[0] ?? (await context.newPage());
    for (const extra of context.pages().slice(1)) await extra.close().catch(() => {});
    if (identity.viewport) await fit(context, page, identity.viewport);

    if (session) {
      const digest = sessionDigest(run.state.salt, session.text);
      if (meta.appliedSession !== digest) {
        await context.setStorageState(session.storageState);
        meta.appliedSession = digest;
      }
    } else if (entry.method !== 'interactive') {
      const written = sessionDigest(run.state.salt, JSON.stringify({ written: entry.written, secrets: entry.secrets }));
      if (meta.written && meta.written !== written) await context.clearCookies();
      meta.written = written;
    }

    const seen = await browserVersion(context, page);
    meta.identity = { ...identity, browser: 'chrome', version: seen.version, userAgent: seen.userAgent };
    meta.row = entry.key;
    const guard = await guardHosts(context, page, entry);
    return await fn(page, context, guard);
  } finally {
    // Whatever session the browser holds now - a password sign-in's cookies too - is redacted from
    // every record the run still writes, value by value.
    if (context && run.sessionSeen) {
      const held = await context.storageState().catch(() => null);
      if (held) run.sessionSeen(held);
    }
    if (context) await context.close().catch(() => {});
    await display.close();
    saveMeta(run.stateDir, key, meta);
  }
}

// The storage state pruned to a set of hosts: the handoff a `session` command gives out never
// carries a cookie or an origin for any host but the entry's own.
export function pruneState(state, hosts) {
  const allowed = new Set(hosts.map((h) => h.toLowerCase()));
  const onHost = (domain) => {
    const d = String(domain || '').replace(/^\./, '').toLowerCase();
    return [...allowed].some((h) => h === d || h.endsWith('.' + d) || d.endsWith('.' + h));
  };
  return {
    cookies: (state.cookies ?? []).filter((c) => onHost(c.domain)),
    origins: (state.origins ?? []).filter((o) => {
      try {
        return allowed.has(new URL(o.origin).hostname.toLowerCase());
      } catch {
        return false;
      }
    }),
  };
}
