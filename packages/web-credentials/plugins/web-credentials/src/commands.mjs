// The command lines a run carries, one per work item. The first word is the command.
//   list
//   verify <name>|all-due
//   session <name> [for <minutes>]
//   check <name> <path> [expect "<text>"]
import { mkdirSync, writeFileSync, chmodSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { randomBytes } from 'node:crypto';
import { Status, findEntry, resolveOnSite, hostAllowed } from './entries.mjs';
import { entryState, fingerprint, sweepHandoffs } from './state.mjs';
export { guardState, reasonOf };
import { verify, ensureSignedIn, sessionExpiry } from './verify.mjs';
import { withEntryBrowser, pruneState } from './browser.mjs';

const MaxPasswordFailures = 2; // lockout guard
const MaxFailedRuns = 3; // Failed -> Needs you

// Splits a command line into words, keeping "quoted runs" as one word.
export function words(line) {
  const out = [];
  const re = /"([^"]*)"|(\S+)/g;
  let m;
  while ((m = re.exec(line)) !== null) out.push(m[1] ?? m[2]);
  return out;
}

// Re-reads an entry's row.status cells from its state, so the row shows what the plugin knows.
function statusCells(s) {
  return {
    status: s.status,
    lastCheck: s.lastCheckAt || '',
    sessionExpires: s.sessionExpires || '',
    detail: reasonOf(s),
  };
}

// THE LOCKOUT GUARD. Every command that may submit a password - a scheduled or asked-for verify, a
// session, a check - goes through it: a password the site did not take counts, the second in a row
// holds the entry (Needs you) and nothing submits its password again until the person changes the row:
// a new password, or a switch of its Try again setting, on or off (any edit and save counts; a run need
// not see the row in between). A change allows exactly one more password: refused, it holds again. A command from a member never lifts it, so no member can lock the person's account by
// repeating one. A password that was sent and then led somewhere the plugin may not follow (off the
// entry's hosts) holds the entry the same way without counting as a refusal: it is not known to be
// wrong, and it is not sent again into that page every hour.

// Notices an edit to the row (password, Try again, anything the person wrote): it lifts a held guard
// for one more password only - one short of the limit, so a refusal holds it again at once (P13).
// Answers the entry's state.
function guardState(entry, state) {
  const s = entryState(state, entry);
  const print = fingerprint(state, entry);
  if (s.fingerprint !== print) {
    s.fingerprint = print;
    if (s.held) {
      s.held = false;
      delete s.holdKind;
      s.passwordFailures = MaxPasswordFailures - 1;
    }
  }
  return s;
}

// The way out, as the control looks in the row editor: a switch, either way. A hold that is not about the
// password (the site took it, then left the hosts or showed a CAPTCHA) does not ask for a new one.
const SwitchTryAgain = 'switch its Try again setting (on or off, either way) and save, to let it try once more.';
const TryAgainPassword = `Set a new password if it changed, or ${SwitchTryAgain}`;
const TryAgain = `S${SwitchTryAgain.slice(1)}`;
const Locked = 'to keep the account from being locked.';
const Unfollowed = 'The plugin will not sign in again until the row changes.';

// The words a held entry is reported in, built from the hold's kind each time it is reported, so an entry
// held by an older plugin (whose stored reason carries the way-out words of its day) reads the current
// words too (P17). The stored reason is cut at the sentence that ends the hold's cause and the current
// way out is added: the password refusal ends "...from being locked.", any other hold "...until the row
// changes.". A state.json from before holdKind was kept is read by those same words. Not held: the
// stored reason as it is.
function reasonOf(s) {
  const reason = s.reason || '';
  if (!s.held) return reason;
  const kind = s.holdKind || (reason.includes(Locked) ? 'password' : 'hold');
  const marker = kind === 'password' ? Locked : Unfollowed;
  const at = reason.indexOf(marker);
  const cause = at >= 0 ? reason.slice(0, at + marker.length) : `${reason.trimEnd()} ${marker}`.trimStart();
  return `${cause} ${kind === 'password' ? TryAgainPassword : TryAgain}`;
}

// Folds one sign-in or verify outcome into the entry's state with the failure rules, the lockout guard
// and the status event, and writes the row's status. Answers { changed, result }.
function fold(entry, state, result, ctx) {
  const s = entryState(state, entry);
  const from = s.status;
  s.lastCheckAt = new Date().toISOString();
  s.sessionExpires = result.sessionExpires || '';

  if (result.status === Status.verified) {
    s.passwordFailures = 0;
    s.failedRuns = 0;
  } else if (result.status === Status.failed) {
    s.failedRuns = (s.failedRuns || 0) + 1;
    if (s.failedRuns >= MaxFailedRuns) {
      result.status = Status.needsYou;
      // No count in the words: they are the board notice's and the Detail's, and read the same each run
      // while the site stays down.
      result.reason = `${entry.name} needs you: its last ${MaxFailedRuns} checks or more failed. Last: ${result.reason}`;
    }
  } else {
    s.failedRuns = 0;
  }

  if (entry.method === 'password' && result.badPassword) {
    s.passwordFailures = (s.passwordFailures || 0) + 1;
    if (s.passwordFailures >= MaxPasswordFailures) {
      s.held = true;
      s.holdKind = 'password';
      result.status = Status.needsYou;
      // No count in the words: after a change, one refusal holds it again.
      result.reason = `${entry.name} needs you: the site refused the password, so the plugin has stopped trying, to keep the account from being locked. ${TryAgainPassword}`;
    }
  }
  if (entry.method === 'password' && result.hold) {
    s.held = true;
    s.holdKind = 'hold';
    result.reason = `${result.reason} The plugin will not sign in again until the row changes. ${TryAgain}`;
  }

  s.status = result.status;
  s.reason = result.reason;
  const changed = from !== s.status;
  if (changed) ctx.publish('status.changed', { entry: entry.name, from, to: s.status, reason: result.reason });
  ctx.statusOf(entry);
  return { changed, result };
}

// Runs one verify through the guard. Answers { changed, result }; a held entry is not tried.
async function checkEntry(entry, state, ctx) {
  const s = guardState(entry, state);
  if (s.held) return { changed: false, held: true, result: { status: s.status, reason: reasonOf(s) } };
  ctx.progress(`Checking ${entry.name}…`);
  const result = await verify(entry, ctx.browserRun);
  return fold(entry, state, result, ctx);
}

export async function cmdList(entries, state) {
  const active = entries.filter((e) => e.active);
  if (active.length === 0) return 'No active entries. Add one in this member’s settings.';
  const lines = active.map((e) => {
    const s = entryState(state, e);
    const when = s.lastCheckAt ? ` (checked ${s.lastCheckAt})` : '';
    const expires = s.sessionExpires ? `, session expires ${s.sessionExpires}` : '';
    return `${e.name} [${e.method}] — ${s.status}${when}${expires}`;
  });
  return lines.join('\n');
}

// A guard every session/check command runs first. Answers null when the entry is usable, else the
// sentence to report: it must be active, not held by the lockout guard, and Verified - except that a
// password entry may always sign in again, through the guard (a CAPTCHA or an off-host page stops it
// before anything is submitted).
function usableRefusal(entry, state) {
  if (!entry.active) return `${entry.name} is inactive; turn it on in this member’s settings to use it.`;
  const s = guardState(entry, state);
  if (s.held) return reasonOf(s);
  if (s.status === Status.verified || entry.method === 'password') return null;
  return s.reason || `${entry.name} is ${s.status.toLowerCase()}.`;
}

export async function cmdVerify(arg, entries, state, ctx) {
  if (!arg || arg.toLowerCase() === 'all-due') {
    const now = Date.now();
    const due = entries.filter((e) => {
      if (!e.active || !e.verifyEvery) return false;
      const s = entryState(state, e);
      if (!s.lastCheckAt) return true;
      return now - Date.parse(s.lastCheckAt) >= e.verifyEvery * 3600 * 1000;
    });
    if (due.length === 0) return { text: 'No entries are due.', changed: false, scheduled: true };
    let changed = false;
    for (const e of due) {
      const out = await checkEntry(e, state, ctx);
      changed = changed || out.changed;
    }
    return { text: `Checked ${due.length} due ${due.length === 1 ? 'entry' : 'entries'}.`, changed, scheduled: true };
  }
  const entry = findEntry(entries, arg);
  if (!entry) return { text: `No entry named ${arg}.`, refusal: true };
  if (!entry.active) return { text: `${entry.name} is inactive; turn it on in this member’s settings.`, refusal: true };
  // An asked-for verify goes through the guard too: a held entry is not tried again.
  const out = await checkEntry(entry, state, ctx);
  return { text: out.result.reason, changed: out.changed, refusal: !!out.held };
}

export async function cmdSession(args, entries, state, ctx) {
  const name = args[0];
  if (!name) return { text: 'Usage: session <name> [for <minutes>].', refusal: true };
  const entry = findEntry(entries, name);
  if (!entry) return { text: `No entry named ${name}.`, refusal: true };
  let minutes = 30;
  const forAt = args.indexOf('for');
  if (forAt >= 0 && args[forAt + 1]) {
    minutes = Math.max(1, Math.min(120, parseInt(args[forAt + 1], 10) || 30));
  }
  const refusal = usableRefusal(entry, state);
  if (refusal) return { text: refusal, refusal: true };

  const expiresAt = new Date(Date.now() + minutes * 60000).toISOString();
  const handoff = handoffPath(ctx, entry);

  if (entry.method === 'token') {
    const token = entry.secrets.token;
    const header = entry.tokenHeader || 'Authorization';
    const value = header.toLowerCase() === 'authorization' ? `Bearer ${token}` : token;
    writeHandoff(handoff.file, { kind: 'token', header, value, hosts: entry.hosts, expiresAt });
    state.handoffs.push({ path: handoff.file, expiresAt, entry: entry.name });
    return { text: `${entry.name}: a token handoff for ${entry.hosts.join(', ')} is at ${handoff.ref}, header ${header}, good until ${expiresAt}. It is deleted when it expires.`, changed: false };
  }

  // password or interactive: sign in (or reuse a still-valid interactive session), prune, hand off.
  let state0;
  if (entry.method === 'interactive') {
    state0 = pruneState(entry.session.storageState, entry.hosts);
  } else {
    const out = await checkEntry(entry, state, ctx);
    if (out.result.status !== Status.verified || !out.result.state) {
      return { text: `${entry.name}: no session was handed off: the sign-in failed. ${out.result.reason}`, refusal: true };
    }
    state0 = pruneState(out.result.state, entry.hosts);
  }
  writeHandoff(handoff.file, { kind: 'storageState', storageState: state0, hosts: entry.hosts, expiresAt });
  state.handoffs.push({ path: handoff.file, expiresAt, entry: entry.name });
  return { text: `${entry.name}: a browser session for ${entry.hosts.join(', ')} is at ${handoff.ref}, good until ${expiresAt}. Start your browser with its storageState; it is deleted when it expires.`, changed: false };
}

export async function cmdCheck(args, entries, state, ctx) {
  const name = args[0];
  const path = args[1];
  if (!name || !path) return { text: 'Usage: check <name> <path> [expect "<text>"].', refusal: true };
  const entry = findEntry(entries, name);
  if (!entry) return { text: `No entry named ${name}.`, refusal: true };
  let expect = null;
  const at = args.indexOf('expect');
  if (at >= 0 && args[at + 1]) expect = args[at + 1];
  const refusal = usableRefusal(entry, state);
  if (refusal) return { text: refusal, refusal: true };
  const target = resolveOnSite(entry, path, 'the path');
  if (target.refusal) return { text: target.refusal, refusal: true };

  const s = entryState(state, entry);
  const run = await withEntryBrowser(entry, ctx.browserRun, async (page, context, guard) => {
    let signedIn = null;
    if (entry.method === 'password') {
      // Sign in on this very page (unless the row's browser still is), so the path below is opened
      // by the signed-in browser. A sign-in here is confirmed and counted like any verify.
      ctx.progress(`Opening ${entry.name}…`);
      const outcome = await ensureSignedIn(page, entry, guard, !s.held);
      if (outcome.held) return { refusal: reasonOf(s) };
      if (!outcome.ok) return { signInFailed: outcome };
      if (!outcome.already) {
        const held = await context.storageState();
        signedIn = { status: Status.verified, reason: `${entry.name} is verified.`, sessionExpires: sessionExpiry(held) };
      }
    }
    let response;
    try {
      response = await page.goto(target.url.toString(), { waitUntil: 'domcontentloaded', timeout: 20000 });
    } catch (e) {
      if (guard.blocked.length) return { signedIn, refusal: `${entry.name}: ${path} redirected to ${guard.blocked[0]}, off the entry's hosts; the browser did not go there.` };
      return { signedIn, refusal: `${entry.name}: ${path} could not be opened (${String(e.message).split('\n')[0]}).` };
    }
    const landed = new URL(page.url());
    if (!hostAllowed(entry, landed)) {
      // A script's redirect that the browser stopped lands on Chrome's error page: name the host it tried.
      return { signedIn, refusal: `${entry.name}: ${path} redirected to ${guard.blocked[0] || landed.hostname}, off the entry's hosts; the browser did not go there.` };
    }
    // Let the page finish what it loads, as a person waits for it, before reading it.
    await page.waitForLoadState('networkidle', { timeout: 8000 }).catch(() => {});
    // A page that tried to leave after it loaded (a meta refresh, a script) was stopped in the browser;
    // what is on screen now is Chrome's own error page, not the site's.
    if (guard.blocked.length || !hostAllowed(entry, new URL(page.url()))) {
      return { signedIn, refusal: `${entry.name}: ${path} loaded, then tried to go to ${guard.blocked[0] || new URL(page.url()).hostname}, off the entry's hosts; the browser stopped it, so there is no page to report.` };
    }
    const title = await page.title().catch(() => '');
    const heading = await page.$eval('h1', (el) => el.textContent.trim()).catch(() => '');
    const body = expect ? await page.content().catch(() => '') : '';
    const shot = shotPath(ctx, entry);
    await page.screenshot({ path: shot.file, fullPage: false }).catch(() => {});
    return {
      signedIn,
      status: response ? response.status() : 0,
      url: landed.toString(),
      title,
      heading,
      found: expect ? body.includes(expect) : null,
      frames: [...guard.framesBlocked],
      shot: shot.rel,
    };
  }).catch((e) => ({ refusal: `${entry.name}: ${String(e.message).split('\n')[0]}` }));

  if (run.signInFailed) {
    const out = fold(entry, state, run.signInFailed, ctx);
    return { text: `${entry.name}: the sign-in FAILED, so ${path} was not opened. ${out.result.reason}`, refusal: true, changed: out.changed };
  }
  let changed = false;
  if (run.signedIn) changed = fold(entry, state, run.signedIn, ctx).changed;
  if (run.refusal) return { text: run.refusal, refusal: true, changed };
  const lines = [
    `${entry.name} ${path}:`,
    `  HTTP ${run.status}, ${run.url}`,
    `  title: ${run.title || '(none)'}`,
    `  heading: ${run.heading || '(none)'}`,
  ];
  if (expect !== null) lines.push(`  expected text ${run.found ? 'found' : 'NOT found'}: "${expect}"`);
  for (const host of run.frames || []) lines.push(`  a frame from ${host} was blocked: off the entry's hosts`);
  if (run.shot) lines.push(`  screenshot: ${run.shot}`);
  return { text: lines.join('\n'), changed, shot: run.shot };
}

// A handoff goes into the REQUESTING member's own workspace, readable by it and by no other member,
// never in documents, a site file, a commit, the card or the ledger.
function handoffPath(ctx, entry) {
  const dir = join(ctx.requesterWorkspace, '.web-credentials-sessions');
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  chmodSync(dir, 0o700);
  const file = join(dir, `${entry.key}-${randomBytes(6).toString('hex')}.json`);
  return { file, ref: `${ctx.requester}:.web-credentials-sessions/${file.split('/').pop()}` };
}

function writeHandoff(file, body) {
  writeFileSync(file, JSON.stringify(body), { mode: 0o600 });
  chmodSync(file, 0o600);
}

// A screenshot goes in the team's documents, under this plugin's site files folder when it has one,
// else the member's documents folder.
function shotPath(ctx, entry) {
  const dir = ctx.documentsDir;
  if (!dir) return { file: join(ctx.workingDirectory, `${entry.key}.png`), rel: null };
  mkdirSync(dir, { recursive: true });
  const name = `${entry.key}-${Date.now()}.png`;
  return { file: join(dir, name), rel: join(ctx.documentsRel || '', name) };
}

export { sweepHandoffs, statusCells };
