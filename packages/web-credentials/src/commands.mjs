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
import { verify } from './verify.mjs';
import { withPage, pruneState } from './browser.mjs';

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
    detail: s.reason || '',
  };
}

// Runs one verify and folds the outcome into the entry's state with the failure rules, the lockout
// guard and the status event. Answers { changed, event }.
async function checkEntry(entry, state, ctx) {
  const s = entryState(state, entry);
  const from = s.status;

  // A changed row (password, selectors, anything the person wrote) clears the lockout guard.
  const print = fingerprint(state, entry);
  if (s.fingerprint !== print) {
    s.fingerprint = print;
    if (s.held) {
      s.held = false;
      s.passwordFailures = 0;
    }
  }
  if (s.held) {
    return { changed: false };
  }

  ctx.progress(`Checking ${entry.name}…`);
  const result = await verify(entry);
  s.lastCheckAt = new Date().toISOString();
  s.sessionExpires = result.sessionExpires || '';

  if (result.status === Status.verified) {
    s.passwordFailures = 0;
    s.failedRuns = 0;
  } else if (result.status === Status.failed) {
    s.failedRuns = (s.failedRuns || 0) + 1;
    if (s.failedRuns >= MaxFailedRuns) {
      result.status = Status.needsYou;
      result.reason = `${entry.name} needs you: it has failed ${s.failedRuns} checks in a row. Last: ${result.reason}`;
    }
  } else {
    s.failedRuns = 0;
  }

  if (entry.method === 'password' && result.badPassword) {
    s.passwordFailures = (s.passwordFailures || 0) + 1;
    if (s.passwordFailures >= MaxPasswordFailures) {
      s.held = true;
      result.status = Status.needsYou;
      result.reason = `${entry.name} needs you: sign-in was refused ${s.passwordFailures} times, so checks are paused. Edit the row or run verify ${entry.name} to try again.`;
    }
  } else if (result.status === Status.verified) {
    s.passwordFailures = 0;
  }

  s.status = result.status;
  s.reason = result.reason;
  const changed = from !== s.status;
  if (changed) {
    if (s.status === Status.verified) s.notified = false;
    return { changed: true, event: { entry: entry.name, from, to: s.status, reason: result.reason }, result };
  }
  return { changed: false, result };
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

// A guard every session/check command runs first: the entry must be active and Verified. Answers
// null when it is usable, else the sentence to report.
function usableRefusal(entry, state) {
  if (!entry.active) return `${entry.name} is inactive; turn it on in this member’s settings to use it.`;
  const s = entryState(state, entry);
  if (s.status !== Status.verified) {
    return s.reason || `${entry.name} is ${s.status.toLowerCase()}.`;
  }
  return null;
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
      if (out.event) ctx.publish('status.changed', out.event);
      ctx.statusOf(e);
      changed = changed || out.changed;
    }
    return { text: `Checked ${due.length} due ${due.length === 1 ? 'entry' : 'entries'}.`, changed, scheduled: true };
  }
  const entry = findEntry(entries, arg);
  if (!entry) return { text: `No entry named ${arg}.`, refusal: true };
  if (!entry.active) return { text: `${entry.name} is inactive; turn it on in this member’s settings.`, refusal: true };
  // A person's explicit verify clears a held guard and tries again.
  const s = entryState(state, entry);
  s.held = false;
  s.passwordFailures = 0;
  const out = await checkEntry(entry, state, ctx);
  if (out.event) ctx.publish('status.changed', out.event);
  ctx.statusOf(entry);
  return { text: out.result ? out.result.reason : `${entry.name}: ${s.status}.`, changed: out.changed };
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
    state0 = pruneState(JSON.parse(entry.secrets.session), entry.hosts);
  } else {
    const out = await checkEntry(entry, state, ctx);
    if (out.event) ctx.publish('status.changed', out.event);
    ctx.statusOf(entry);
    if (!out.result || out.result.status !== Status.verified || !out.result.state) {
      return { text: out.result ? out.result.reason : `${entry.name} could not be signed in.`, refusal: true };
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

  const storageState = entry.method === 'interactive' ? JSON.parse(entry.secrets.session) : undefined;
  const run = await withPage(async (page) => {
    if (entry.method === 'password') {
      // Sign in first so the page load is authenticated.
      const out = await checkEntry(entry, state, ctx);
      if (out.result && out.result.status !== Status.verified) throw new Error(out.result.reason);
    }
    const response = await page.goto(target.url.toString(), { waitUntil: 'domcontentloaded', timeout: 20000 });
    const landed = new URL(page.url());
    if (!hostAllowed(entry, landed)) {
      return { refusal: `${entry.name}: the page redirected to ${landed.hostname}, off the entry's hosts.` };
    }
    const title = await page.title().catch(() => '');
    const heading = await page.$eval('h1', (el) => el.textContent.trim()).catch(() => '');
    const body = expect ? await page.content().catch(() => '') : '';
    const shot = shotPath(ctx, entry);
    await page.screenshot({ path: shot.file, fullPage: false }).catch(() => {});
    return {
      status: response ? response.status() : 0,
      url: landed.toString(),
      title,
      heading,
      found: expect ? body.includes(expect) : null,
      shot: shot.rel,
    };
  }, storageState ? { storageState } : {}).catch((e) => ({ refusal: `${entry.name}: ${String(e.message).split('\n')[0]}` }));

  if (run.refusal) return { text: run.refusal, refusal: true };
  const lines = [
    `${entry.name} ${path}:`,
    `  HTTP ${run.status}, ${run.url}`,
    `  title: ${run.title || '(none)'}`,
    `  heading: ${run.heading || '(none)'}`,
  ];
  if (expect !== null) lines.push(`  expected text ${run.found ? 'found' : 'NOT found'}: "${expect}"`);
  if (run.shot) lines.push(`  screenshot: ${run.shot}`);
  return { text: lines.join('\n'), changed: false, shot: run.shot };
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
