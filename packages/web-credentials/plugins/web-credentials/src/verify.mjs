// Confirming one entry is signed in, by its method. Each function answers
// { status, reason, sessionExpires?, captcha?, badPassword?, state? }:
//   status      one of Status.*
//   reason      one sentence, shown in the row's Detail and in the status event
//   captcha     true when a CAPTCHA or device approval stopped a password sign-in (Needs you)
//   badPassword true when the site rejected the password (feeds the lockout guard)
//   hold        true when a password was sent and then led off the entry's hosts: not counted as a
//               refusal, but nothing is sent again until the person changes the row
//   state       the storage state after a password sign-in, for a session handoff in the same run
import { Status, resolveOnSite, hostAllowed } from './entries.mjs';
import { withEntryBrowser } from './browser.mjs';
import { totp } from './totp.mjs';
import { type, click, pressEnter, pause } from './human.mjs';

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

// Why a request never got an answer, in words: Node's fetch throws a bare "fetch failed" TypeError
// with the network error as its cause.
const NetworkErrors = {
  ECONNREFUSED: 'connection refused',
  ECONNRESET: 'connection reset',
  ENOTFOUND: 'the host name was not found',
  EAI_AGAIN: 'the host name could not be looked up',
  ETIMEDOUT: 'the connection timed out',
  EHOSTUNREACH: 'the host is unreachable',
  ENETUNREACH: 'the network is unreachable',
  CERT_HAS_EXPIRED: 'the site’s certificate has expired',
  DEPTH_ZERO_SELF_SIGNED_CERT: 'the site’s certificate is self-signed',
  UNABLE_TO_VERIFY_LEAF_SIGNATURE: 'the site’s certificate could not be verified',
};
export function requestFailure(e) {
  const cause = e?.cause ?? e;
  const code = cause?.code || cause?.errors?.[0]?.code;
  if (code && NetworkErrors[code]) return NetworkErrors[code];
  if (e?.name === 'TimeoutError' || e?.name === 'AbortError') return 'it timed out';
  if (code) return code;
  return String(cause?.message || e?.message || 'no answer').split('\n')[0];
}

// A page load that failed: blocked because it left the entry's hosts (named), or the browser's own
// network error (net::ERR_CONNECTION_REFUSED and the like), in words.
function loadFailure(e, guard) {
  if (guard?.blocked.length) return { offHost: guard.blocked[0] };
  const m = /net::ERR_([A-Z_]+)/.exec(String(e?.message));
  const words = m ? m[1].toLowerCase().replace(/_/g, ' ') : String(e?.message || 'no answer').split('\n')[0];
  return { why: words };
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
    return { status: Status.failed, reason: `${entry.name}: the check request failed (${requestFailure(e)}).` };
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

export { sessionExpiry };

export async function verifyInteractive(entry, run) {
  if (!entry.secrets.session) {
    return { status: Status.needsYou, reason: `${entry.name} needs you: open its Sign in link to sign in once.` };
  }
  if (!entry.session) {
    return { status: Status.needsYou, reason: `${entry.name} needs you: its saved session could not be read. Open its Sign in link.` };
  }
  const target = resolveOnSite(entry, entry.checkUrl, 'the signed-in check URL');
  if (target.refusal) return { status: Status.needsYou, reason: target.refusal };
  return withEntryBrowser(entry, run, async (page, context, guard) => {
    let response;
    try {
      response = await page.goto(target.url.toString(), nav);
    } catch (e) {
      const f = loadFailure(e, guard);
      if (f.offHost) return { status: Status.needsYou, reason: offHostReason(entry, 'the check page', f.offHost) };
      return { status: Status.failed, reason: `${entry.name}: the check page could not be opened (${f.why}).` };
    }
    const landed = new URL(page.url());
    if (!hostAllowed(entry, landed) || guard.blocked.length) {
      return { status: Status.needsYou, reason: offHostReason(entry, 'the check page', guard.blocked[0] || landed.hostname) };
    }
    const result = await confirm(page, entry, response);
    // A page that tried to leave after it loaded (a meta refresh, a script) was stopped: not a check.
    if (guard.blocked.length) return { status: Status.needsYou, reason: offHostReason(entry, 'the check page', guard.blocked[0]) };
    if (!result.ok) {
      return { status: Status.expired, reason: `${entry.name} is expired: ${result.why}. Open its Sign in link to sign in again.` };
    }
    return { status: Status.verified, reason: `${entry.name} is verified.`, sessionExpires: sessionExpiry(await context.storageState()) };
  }).catch((e) => ({ status: Status.failed, reason: `${entry.name}: ${e.message}` }));
}

function offHostReason(entry, what, host) {
  return `${entry.name} needs you: ${what} redirected to ${host}, off the entry's hosts; the browser did not go there. Add ${host} to the entry's Other hosts if it belongs to the site.`;
}

const CaptchaAdvice = (entry) =>
  entry.method === 'interactive'
    ? 'Open its Sign in link and sign in yourself.'
    : 'The plugin never gets around one: change the entry’s Method to interactive, then open its Sign in link and sign in yourself.';

// Signs in with the password form on the given page and CONFIRMS it took: after the form (and the
// two-step code) it opens the check page, which must show a signed-in user. Answers { ok } only then;
// otherwise a verify-shaped failure { status, reason, badPassword?, captcha? }. badPassword is set
// only when the password was actually submitted and the site did not take it, so the caller counts
// it against the lockout guard. Never tries a password sign-in for an interactive entry: that is the
// caller's to keep straight.
export async function signInOnPage(page, entry, guard) {
  const password = entry.secrets.password;
  if (!password) return { status: Status.needsYou, reason: `${entry.name} needs you: it has no password. Edit the row and set the Password.` };
  if (!entry.username) return { status: Status.needsYou, reason: `${entry.name} needs you: it has no username.` };
  const signIn = resolveOnSite(entry, entry.signInUrl || entry.siteUrl, 'the sign-in URL');
  if (signIn.refusal) return { status: Status.needsYou, reason: signIn.refusal };
  const check = resolveOnSite(entry, entry.checkUrl, 'the signed-in check URL');
  if (check.refusal) return { status: Status.needsYou, reason: check.refusal };

  try {
    await page.goto(signIn.url.toString(), nav);
  } catch (e) {
    const f = loadFailure(e, guard);
    if (f.offHost) return { status: Status.needsYou, reason: offHostReason(entry, 'the sign-in page', f.offHost) };
    return { status: Status.failed, reason: `${entry.name}: the sign-in page could not be opened (${f.why}).` };
  }
  if (guard?.blocked.length || !hostAllowed(entry, new URL(page.url()))) {
    return { status: Status.needsYou, reason: offHostReason(entry, 'the sign-in page', guard?.blocked[0] || new URL(page.url()).hostname) };
  }
  if (await looksLikeCaptcha(page)) {
    return { status: Status.needsYou, captcha: true, reason: `${entry.name} needs you: the sign-in page shows a CAPTCHA or device approval. ${CaptchaAdvice(entry)}` };
  }
  const userField = await findField(page, entry.selectors.username, ['input[type="email"]', 'input[autocomplete="username"]', 'input[name*="user" i]', 'input[name*="email" i]', 'input[type="text"]']);
  const passField = await findField(page, entry.selectors.password, ['input[type="password"]']);
  if (!userField || !passField) {
    return { status: Status.needsYou, reason: `${entry.name} needs you: the username or password field was not found on the sign-in page. Set the field selectors on the row.` };
  }
  await pause();
  await type(page, userField, entry.username);
  await type(page, passField, password);
  const submit = await findField(page, entry.selectors.submit, ['button[type="submit"]', 'input[type="submit"]', 'button']);
  await submitAndWait(page, submit);

  if (entry.secrets.totpSecret && !guard?.blocked.length) {
    const codeField = await findField(page, entry.selectors.totp, ['input[autocomplete="one-time-code"]', 'input[name*="otp" i]', 'input[name*="code" i]', 'input[name*="totp" i]']);
    if (codeField) {
      await pause(600, 1400);
      await type(page, codeField, totp(entry.secrets.totpSecret));
      const go = await findField(page, entry.selectors.submit, ['button[type="submit"]', 'input[type="submit"]', 'button']);
      await submitAndWait(page, go);
    }
  }

  if (guard?.blocked.length) {
    return { status: Status.needsYou, hold: true, reason: offHostReason(entry, 'the sign-in', guard.blocked[0]) };
  }
  if (await looksLikeCaptcha(page)) {
    return { status: Status.needsYou, captcha: true, hold: true, reason: `${entry.name} needs you: a CAPTCHA or device approval was shown during sign-in. ${CaptchaAdvice(entry)}` };
  }
  const now = new URL(page.url());
  if (!hostAllowed(entry, now)) {
    return { status: Status.needsYou, hold: true, reason: offHostReason(entry, 'the sign-in', now.hostname) };
  }

  // Did it take? The check page decides, as for a returning browser.
  await pause();
  let response;
  try {
    response = await page.goto(check.url.toString(), nav);
  } catch (e) {
    const f = loadFailure(e, guard);
    // The password was sent and the site may well have taken it: the check page leaving the hosts
    // says nothing about the password, so it is not counted, but it is not sent again either (hold).
    if (f.offHost) return { status: Status.needsYou, hold: true, reason: offHostReason(entry, 'the check page', f.offHost) };
    return { status: Status.failed, reason: `${entry.name}: the check page could not be opened after sign-in (${f.why}).` };
  }
  const after = new URL(page.url());
  if (!hostAllowed(entry, after) || guard?.blocked.length) {
    return { status: Status.needsYou, hold: true, reason: offHostReason(entry, 'the check page', guard?.blocked[0] || after.hostname) };
  }
  const result = await confirm(page, entry, response);
  if (guard?.blocked.length) {
    return { status: Status.needsYou, hold: true, reason: offHostReason(entry, 'the check page', guard.blocked[0]) };
  }
  if (!result.ok) {
    const onSignIn = after.pathname === signIn.url.pathname;
    return {
      status: Status.expired,
      badPassword: true,
      reason: onSignIn
        ? `${entry.name}: the sign-in was rejected. Check the username and password.`
        : `${entry.name}: the sign-in did not take: ${result.why}. Check the username and password.`,
    };
  }
  return { ok: true };
}

// Clicks the submit button (or presses Enter) and waits for the page that follows.
async function submitAndWait(page, button) {
  const navigated = page.waitForNavigation({ waitUntil: 'domcontentloaded', timeout: 20000 }).catch(() => {});
  if (button) await click(page, button);
  else await pressEnter(page);
  await navigated;
}

// A returning browser may still be signed in: the check page decides. Answers true when it shows a
// signed-in user, without typing anything.
async function stillSignedIn(page, entry, check, guard) {
  const signInPath = resolveOnSite(entry, entry.signInUrl || entry.siteUrl, 'the sign-in URL').url?.pathname;
  const response = await page.goto(check.url.toString(), nav).catch(() => null);
  // A redirect off the hosts here (blocked) is not a signed-in page; the sign-in that follows decides,
  // once Chrome has shown its own error page for the stopped load (else that interrupts the next one).
  if (guard?.blocked.length) {
    await page.waitForURL(/^chrome-error:/, { timeout: 5000 }).catch(() => {});
    await page.waitForLoadState('load', { timeout: 5000 }).catch(() => {});
  }
  guard?.blocked.splice(0);
  const landed = new URL(page.url());
  if (!hostAllowed(entry, landed) || landed.pathname === signInPath) return false;
  return (await confirm(page, entry, response)).ok;
}

// Signs the page in unless the profile already is: answers { ok, already? } or the sign-in's failure
// (see signInOnPage). `allowSignIn` is the lockout guard's word: when false, nothing is submitted
// and a page that is not signed in answers { held: true }.
export async function ensureSignedIn(page, entry, guard, allowSignIn = true) {
  const check = resolveOnSite(entry, entry.checkUrl, 'the signed-in check URL');
  if (check.refusal) return { status: Status.needsYou, reason: check.refusal };
  if (await stillSignedIn(page, entry, check, guard)) return { ok: true, already: true };
  if (!allowSignIn) return { held: true };
  return signInOnPage(page, entry, guard);
}

export async function verifyPassword(entry, run) {
  const check = resolveOnSite(entry, entry.checkUrl, 'the signed-in check URL');
  if (check.refusal) return { status: Status.needsYou, reason: check.refusal };

  return withEntryBrowser(entry, run, async (page, context, guard) => {
    const signedIn = await ensureSignedIn(page, entry, guard);
    if (!signedIn.ok) return signedIn;
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

export function verify(entry, run) {
  if (entry.method === 'token') return verifyToken(entry);
  if (entry.method === 'interactive') return verifyInteractive(entry, run);
  return verifyPassword(entry, run);
}
