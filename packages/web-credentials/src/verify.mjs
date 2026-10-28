// Confirming one entry is signed in, by its method. Each function answers
// { status, reason, sessionExpires?, captcha?, badPassword?, state? }:
//   status      one of Status.*
//   reason      one sentence, shown in the row's Detail and in the status event
//   captcha     true when a CAPTCHA or device approval stopped a password sign-in (Needs you)
//   badPassword true when the site rejected the password (feeds the lockout guard)
//   state       the storage state after a password sign-in, for a session handoff in the same run
import { Status, resolveOnSite, hostAllowed } from './entries.mjs';
import { withPage } from './browser.mjs';
import { totp } from './totp.mjs';

const nav = { waitUntil: 'domcontentloaded', timeout: 20000 };

function sessionExpiry(state) {
  let soonest = null;
  for (const c of state?.cookies ?? []) {
    if (typeof c.expires === 'number' && c.expires > 0) {
      const at = c.expires * 1000;
      if (soonest === null || at < soonest) soonest = at;
    }
  }
  return soonest ? new Date(soonest).toISOString() : null;
}

async function looksLikeCaptcha(page) {
  return page
    .evaluate(() => {
      const html = document.body ? document.body.innerHTML.toLowerCase() : '';
      if (/captcha|recaptcha|hcaptcha|are you a robot|verify you are human|device approval|approve this sign/.test(html)) return true;
      return !!document.querySelector('iframe[src*="recaptcha"],iframe[src*="hcaptcha"],[class*="captcha"],[id*="captcha"]');
    })
    .catch(() => false);
}

// Did the check page show a signed-in user? The check text (plain, or css=<selector>) decides when
// given; otherwise a 2xx that did not land back on the sign-in URL.
async function confirm(page, entry, response) {
  const status = response ? response.status() : 0;
  if (status === 0 || status >= 400) return { ok: false, why: `the check page answered ${status || 'no response'}` };
  if (entry.checkText) {
    if (entry.checkText.startsWith('css=')) {
      const found = await page.$(entry.checkText.slice(4)).catch(() => null);
      return found ? { ok: true } : { ok: false, why: `the signed-in marker (${entry.checkText}) was not on the check page` };
    }
    const body = await page.content().catch(() => '');
    return body.includes(entry.checkText)
      ? { ok: true }
      : { ok: false, why: `the signed-in text was not on the check page` };
  }
  return { ok: true };
}

export async function verifyToken(entry) {
  const token = entry.secrets.token;
  if (!token) return { status: Status.needsYou, reason: `${entry.name} needs you: it has no token. Edit the row and set the Token.` };
  const target = resolveOnSite(entry, entry.checkUrl, 'the signed-in check URL');
  if (target.refusal) return { status: Status.needsYou, reason: target.refusal };
  const header = entry.tokenHeader || 'Authorization';
  const value = header.toLowerCase() === 'authorization' ? `Bearer ${token}` : token;
  let response;
  try {
    response = await fetch(target.url, { headers: { [header]: value }, redirect: 'manual' });
  } catch (e) {
    return { status: Status.failed, reason: `${entry.name}: the check request failed (${e.code || e.name}).` };
  }
  if (response.status >= 300 && response.status < 400) {
    const location = response.headers.get('location');
    if (location && !hostAllowed(entry, new URL(location, target.url))) {
      return { status: Status.needsYou, reason: `${entry.name} needs you: the check redirected to ${new URL(location, target.url).hostname}, off the entry's hosts.` };
    }
  }
  if (response.status === 401 || response.status === 403) {
    return { status: Status.expired, reason: `${entry.name} is expired: the token was refused (${response.status}).` };
  }
  if (response.status < 200 || response.status >= 300) {
    return { status: Status.failed, reason: `${entry.name}: the check answered ${response.status}.` };
  }
  if (entry.checkText && !entry.checkText.startsWith('css=')) {
    const body = await response.text().catch(() => '');
    if (!body.includes(entry.checkText)) {
      return { status: Status.expired, reason: `${entry.name} is expired: the signed-in text was not in the response.` };
    }
  }
  return { status: Status.verified, reason: `${entry.name} is verified.` };
}

export async function verifyInteractive(entry) {
  if (!entry.secrets.session) {
    return { status: Status.needsYou, reason: `${entry.name} needs you: open its Sign in link to sign in once.` };
  }
  let storageState;
  try {
    storageState = JSON.parse(entry.secrets.session);
  } catch {
    return { status: Status.needsYou, reason: `${entry.name} needs you: its saved session could not be read. Open its Sign in link.` };
  }
  const target = resolveOnSite(entry, entry.checkUrl, 'the signed-in check URL');
  if (target.refusal) return { status: Status.needsYou, reason: target.refusal };
  return withPage(async (page) => {
    const response = await page.goto(target.url.toString(), nav).catch((e) => {
      throw new Error(`the check page could not be opened (${e.message.split('\n')[0]})`);
    });
    const landed = new URL(page.url());
    if (!hostAllowed(entry, landed)) {
      return { status: Status.needsYou, reason: `${entry.name} needs you: the check page redirected to ${landed.hostname}, off the entry's hosts.` };
    }
    const result = await confirm(page, entry, response);
    if (!result.ok) {
      return { status: Status.expired, reason: `${entry.name} is expired: ${result.why}. Open its Sign in link to sign in again.` };
    }
    return { status: Status.verified, reason: `${entry.name} is verified.`, sessionExpires: sessionExpiry(storageState) };
  }, { storageState }).catch((e) => ({ status: Status.failed, reason: `${entry.name}: ${e.message}` }));
}

// Signs in with the password form on the given page (so a `check` can reuse the same context).
// Answers { ok } on success, else { status, reason, captcha?, badPassword? }. Never tries a
// password sign-in for an interactive entry: that is the caller's to keep straight.
export async function signInOnPage(page, entry) {
  const password = entry.secrets.password;
  if (!password) return { status: Status.needsYou, reason: `${entry.name} needs you: it has no password. Edit the row and set the Password.` };
  if (!entry.username) return { status: Status.needsYou, reason: `${entry.name} needs you: it has no username.` };
  const signIn = resolveOnSite(entry, entry.signInUrl || entry.siteUrl, 'the sign-in URL');
  if (signIn.refusal) return { status: Status.needsYou, reason: signIn.refusal };

  await page.goto(signIn.url.toString(), nav);
  if (!hostAllowed(entry, new URL(page.url()))) {
    return { status: Status.needsYou, reason: `${entry.name} needs you: the sign-in page went to ${new URL(page.url()).hostname}, off the entry's hosts. Use its Sign in link for a sign-in that passes through other hosts.` };
  }
  if (await looksLikeCaptcha(page)) {
    return { status: Status.needsYou, captcha: true, reason: `${entry.name} needs you: the sign-in page shows a CAPTCHA or device approval. Use its Sign in link.` };
  }
  const userField = await findField(page, entry.selectors.username, ['input[type="email"]', 'input[autocomplete="username"]', 'input[name*="user" i]', 'input[name*="email" i]', 'input[type="text"]']);
  const passField = await findField(page, entry.selectors.password, ['input[type="password"]']);
  if (!userField || !passField) {
    return { status: Status.needsYou, reason: `${entry.name} needs you: the username or password field was not found on the sign-in page. Set the field selectors on the row, or use its Sign in link.` };
  }
  await userField.fill(entry.username);
  await passField.fill(password);
  const submit = await findField(page, entry.selectors.submit, ['button[type="submit"]', 'input[type="submit"]', 'button']);
  await Promise.all([page.waitForLoadState('domcontentloaded', { timeout: 20000 }).catch(() => {}), submit ? submit.click() : passField.press('Enter')]);

  if (entry.secrets.totpSecret) {
    const codeField = await findField(page, entry.selectors.totp, ['input[autocomplete="one-time-code"]', 'input[name*="otp" i]', 'input[name*="code" i]', 'input[name*="totp" i]']);
    if (codeField) {
      await codeField.fill(totp(entry.secrets.totpSecret));
      const go = await findField(page, entry.selectors.submit, ['button[type="submit"]', 'input[type="submit"]', 'button']);
      await Promise.all([page.waitForLoadState('domcontentloaded', { timeout: 20000 }).catch(() => {}), go ? go.click() : codeField.press('Enter')]);
    }
  }

  if (await looksLikeCaptcha(page)) {
    return { status: Status.needsYou, captcha: true, reason: `${entry.name} needs you: a CAPTCHA or device approval was shown during sign-in. Use its Sign in link.` };
  }
  const now = new URL(page.url());
  if (!hostAllowed(entry, now)) {
    return { status: Status.needsYou, reason: `${entry.name} needs you: sign-in moved to ${now.hostname}, off the entry's hosts. Use its Sign in link.` };
  }
  return { ok: true, signInPath: signIn.url.pathname };
}

export async function verifyPassword(entry) {
  const check = resolveOnSite(entry, entry.checkUrl, 'the signed-in check URL');
  if (check.refusal) return { status: Status.needsYou, reason: check.refusal };

  return withPage(async (page, context) => {
    const signedIn = await signInOnPage(page, entry);
    if (!signedIn.ok) return signedIn;

    const response = await page.goto(check.url.toString(), nav).catch(() => null);
    const after = new URL(page.url());
    if (!hostAllowed(entry, after)) {
      return { status: Status.expired, badPassword: true, reason: `${entry.name}: the check page redirected to ${after.hostname}; the sign-in did not take.` };
    }
    const result = await confirm(page, entry, response);
    if (!result.ok) {
      const onSignIn = after.pathname === signedIn.signInPath;
      return {
        status: Status.expired,
        badPassword: onSignIn,
        reason: onSignIn
          ? `${entry.name}: the sign-in was rejected. Check the username and password.`
          : `${entry.name} is expired: ${result.why}.`,
      };
    }
    const state = await context.storageState();
    return { status: Status.verified, reason: `${entry.name} is verified.`, sessionExpires: sessionExpiry(state), state };
  }).catch((e) => ({ status: Status.failed, reason: `${entry.name}: the sign-in failed (${String(e.message).split('\n')[0]}).` }));
}

async function findField(page, override, fallbacks) {
  const tryOne = async (selector) => {
    const el = await page.$(selector).catch(() => null);
    if (el && (await el.isVisible().catch(() => false))) return el;
    return null;
  };
  if (override) return tryOne(override);
  for (const selector of fallbacks) {
    const el = await tryOne(selector);
    if (el) return el;
  }
  return null;
}

export function verify(entry) {
  if (entry.method === 'token') return verifyToken(entry);
  if (entry.method === 'interactive') return verifyInteractive(entry);
  return verifyPassword(entry);
}
