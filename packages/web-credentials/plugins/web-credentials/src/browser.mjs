// Drives the Chromium headless shell that ships in this plugin's folder (tools/browser.sh put it
// there) through playwright-core. Nothing is fetched at run time and nothing in the image is used.
import { chromium } from 'playwright-core';
import { readFileSync, existsSync, readdirSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(dirname(fileURLToPath(import.meta.url)));

export function shellPath() {
  const browsers = JSON.parse(readFileSync(join(root, 'node_modules/playwright-core/browsers.json'), 'utf8')).browsers;
  const revision = browsers.find((b) => b.name === 'chromium-headless-shell')?.revision;
  // The shipped plugin carries the shell at its root (build.sh ran tools/browser.sh). Running from
  // the source tree, tests point PLAYWRIGHT_BROWSERS_PATH at a shared cache instead of copying it.
  const roots = [root, process.env.PLAYWRIGHT_BROWSERS_PATH].filter(Boolean);
  for (const base of roots) {
    const named = join(base, `chromium_headless_shell-${revision}`, 'chrome-headless-shell-linux64', 'chrome-headless-shell');
    if (existsSync(named)) return named;
  }
  // Fall back to whichever headless shell is laid out, so a revision bump that forgot this file
  // still runs rather than failing to find a browser.
  for (const base of roots) {
    for (const name of readdirSync(base).filter((n) => n.startsWith('chromium_headless_shell-'))) {
      const p = join(base, name, 'chrome-headless-shell-linux64', 'chrome-headless-shell');
      if (existsSync(p)) return p;
    }
  }
  throw new Error('The Chromium headless shell is not in the plugin folder; its build did not run tools/browser.sh.');
}

// Opens a browser, hands `fn` a fresh context (with the saved storage state when one is given) and
// a page, and always closes them. `storageState` is Playwright's storageState object or string.
export async function withPage(fn, { storageState } = {}) {
  const browser = await chromium.launch({ executablePath: shellPath(), headless: true, chromiumSandbox: true });
  try {
    const context = await browser.newContext(storageState ? { storageState } : {});
    const page = await context.newPage();
    try {
      return await fn(page, context);
    } finally {
      await context.close();
    }
  } finally {
    await browser.close();
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
