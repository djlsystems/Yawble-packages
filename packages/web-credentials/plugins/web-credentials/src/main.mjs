// The Web Credentials plugin. One JSON request on stdin (harness.member/1), JSON-Lines records out.
import { readFileSync } from 'node:fs';
import { join, dirname, basename } from 'node:path';
import { makeOut } from './out.mjs';
import { readEntries, secretValues, stateSecretValues, Status } from './entries.mjs';
import { stateDir, loadState, saveState, entryState, sweepHandoffs } from './state.mjs';
import { profileKey, sweepProfiles } from './profile.mjs';
import { words, cmdList, cmdVerify, cmdSession, cmdCheck, statusCells } from './commands.mjs';

// The team's documents folder, where a check's screenshot goes, from where the platform keeps things
// (never a fixed path): a site's files folder is <documents>/<team>/sites/<site>/files, and a member's
// workspace is <data>/teams/<team>/workspaces/<name> beside <data>/documents/<team>. Answers null when
// neither says, and the screenshot stays in the plugin's own workspace.
export function teamDocuments(request, workspace) {
  const files = Array.isArray(request.siteFiles) ? request.siteFiles.find((f) => typeof f?.folder === 'string') : null;
  if (files && basename(dirname(dirname(files.folder))) === 'sites') return dirname(dirname(dirname(files.folder)));
  const workspaces = dirname(workspace);
  const team = dirname(workspaces);
  if (basename(workspaces) === 'workspaces' && basename(dirname(team)) === 'teams') {
    return join(dirname(dirname(team)), 'documents', basename(team));
  }
  return null;
}

// The board's notice names a condition: the entries that need the person and why. It is raised when
// that changes, not on every run.
function noticeKey(entry, s) {
  return `${s.status}|${s.reason}`;
}

const Usage = 'Commands: list; verify <name>|all-due; session <name> [for <minutes>]; check <name> <path> [expect "<text>"].';

export async function run(request, out) {
  const entries = readEntries(request);
  out.setSecrets(secretValues(request, entries));

  const dir = stateDir(request.workingDirectory || process.cwd());
  const state = loadState(dir);
  const sweptNow = sweepHandoffs(state);
  // One browser profile per member and row; a row that is gone takes its profile with it.
  const member = request.member?.id || request.member?.name || 'web-credentials';
  sweepProfiles(dir, entries.map((e) => profileKey(member, e.key)));

  const myWorkspace = request.workingDirectory || process.cwd();
  const documents = teamDocuments(request, myWorkspace);
  // Turning an entry off and on lifts the lockout guard, as an edit to it does.
  for (const e of entries) {
    if (e.active) continue;
    const s = entryState(state, e);
    s.held = false;
    s.passwordFailures = 0;
  }
  const ctx = {
    workingDirectory: myWorkspace,
    documentsDir: documents ? join(documents, 'Web Credentials') : null,
    documentsRel: documents ? 'Web Credentials' : null,
    requester: null,
    requesterWorkspace: myWorkspace,
    browserRun: {
      stateDir: dir,
      state,
      member,
      settings: { languages: request.config?.languages, timeZone: request.config?.timeZone },
      progress: out.progress,
      // The session a browser holds when it closes is redacted from all the run writes after.
      sessionSeen: (held) => out.addSecrets(stateSecretValues(held)),
    },
    progress: out.progress,
    publish: out.publish,
    statusOf: (entry) => {
      const s = entryState(state, entry);
      out.rowStatus(entry.name, statusCells(s));
    },
  };

  const work = Array.isArray(request.work) ? request.work : [];
  const outputs = [];
  let anyChange = sweptNow > 0;
  let anyScheduled = false;

  for (const item of work) {
    const source = item.source || '';
    // A handoff goes to the member that asked; a person or the plugin itself falls back to the
    // plugin's own workspace.
    if (source.includes('/') && !source.startsWith('schedule:') && !source.startsWith('trigger:')) {
      ctx.requester = source;
      ctx.requesterWorkspace = join(dirname(myWorkspace), source.split('/').slice(1).join('/'));
    } else {
      ctx.requester = request.member?.name || 'web-credentials';
      ctx.requesterWorkspace = myWorkspace;
    }

    const line = (item.instruction || item.payload?.instruction || '').trim();
    const parts = words(line);
    const command = (parts[0] || '').toLowerCase();
    const prefix = work.length > 1 ? `${command || '(empty)'}: ` : '';
    try {
      let r;
      switch (command) {
        case '':
        case 'list':
          r = { text: await cmdList(entries, state) };
          break;
        case 'verify':
          r = await cmdVerify(parts[1], entries, state, ctx);
          break;
        case 'session':
          r = await cmdSession(parts.slice(1), entries, state, ctx);
          break;
        case 'check':
          r = await cmdCheck(parts.slice(1), entries, state, ctx);
          break;
        default:
          r = { text: `Unknown command "${command}". ${Usage}` };
      }
      outputs.push(prefix + r.text);
      anyChange = anyChange || !!r.changed;
      anyScheduled = anyScheduled || !!r.scheduled;
    } catch (e) {
      outputs.push(prefix + `failed: ${String(e.message).split('\n')[0]}`);
    }
  }
  if (work.length === 0) outputs.push(Usage);

  // Row.status for every entry whose status the schedule refreshed is written as each check runs.
  // The board notice: while any active entry needs the person, one needsDecision names them all - raised
  // once for each new condition (an entry newly needing the person, or for a new reason), not again on
  // every run, so the Needs-you item stays on the card that found it.
  const needy = entries.filter((e) => e.active && (entryState(state, e).status === Status.needsYou || entryState(state, e).status === Status.expired));
  for (const e of entries) {
    if (!needy.includes(e)) entryState(state, e).notified = false;
  }
  const fresh = needy.filter((e) => entryState(state, e).notified !== noticeKey(e, entryState(state, e)));
  if (fresh.length > 0) {
    const lines = needy.map((e) => {
      const s = entryState(state, e);
      s.notified = noticeKey(e, s);
      const how = e.method === 'interactive' && !/Sign in link/.test(s.reason) ? ' Open its Sign in link.' : '';
      return `• ${e.name}: ${s.status}. ${s.reason}${how}`;
    });
    out.needsDecision(`${needy.length} website sign-in${needy.length === 1 ? ' needs' : 's need'} you:\n${lines.join('\n')}`);
  }

  saveState(dir, state);

  // A scheduled poll that changed nothing wakes nobody. A command from a member or person always
  // answers.
  const quiet = anyScheduled && work.every((w) => (w.source || '').startsWith('schedule:') || (w.source || '').startsWith('trigger:')) && !anyChange;
  out.result(true, outputs.join('\n\n'), quiet);
}

async function main() {
  const out = makeOut();
  let request;
  try {
    request = JSON.parse(readFileSync(0, 'utf8'));
  } catch {
    out.result(false, 'The request on stdin could not be read.');
    process.exit(1);
  }
  try {
    await run(request, out);
  } catch (e) {
    out.result(false, `The plugin failed: ${String(e.message).split('\n')[0]}`);
    process.exit(1);
  }
}

// Run only as the entry point, not when imported by a test.
if (process.argv[1] && basename(process.argv[1]) === 'main.mjs') {
  main();
}
