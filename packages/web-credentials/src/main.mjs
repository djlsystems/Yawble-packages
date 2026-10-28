// The Web Credentials plugin. One JSON request on stdin (harness.member/1), JSON-Lines records out.
import { readFileSync } from 'node:fs';
import { join, dirname, basename } from 'node:path';
import { makeOut } from './out.mjs';
import { readEntries, secretValues, Status } from './entries.mjs';
import { stateDir, loadState, saveState, entryState, sweepHandoffs } from './state.mjs';
import { words, cmdList, cmdVerify, cmdSession, cmdCheck, statusCells } from './commands.mjs';

const Usage = 'Commands: list; verify <name>|all-due; session <name> [for <minutes>]; check <name> <path> [expect "<text>"].';

export async function run(request, out) {
  const entries = readEntries(request);
  out.setSecrets(secretValues(request, entries));

  const dir = stateDir(request.workingDirectory || process.cwd());
  const state = loadState(dir);
  const sweptNow = sweepHandoffs(state);

  const myWorkspace = request.workingDirectory || process.cwd();
  const team = request.member?.team || basename(dirname(dirname(myWorkspace)));
  const ctx = {
    workingDirectory: myWorkspace,
    documentsDir: join('/data/documents', team, 'Web Credentials'),
    documentsRel: null,
    requester: null,
    requesterWorkspace: myWorkspace,
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
  // The board notice: while any active entry needs the person, one needsDecision names them, once.
  const needy = entries.filter((e) => e.active && (entryState(state, e).status === Status.needsYou || entryState(state, e).status === Status.expired));
  if (needy.length > 0) {
    for (const e of needy) entryState(state, e).notified = true;
    const lines = needy.map((e) => {
      const s = entryState(state, e);
      const how = e.method === 'interactive' ? ' Open its Sign in link.' : '';
      return `• ${e.name}: ${s.status}. ${s.reason}${how}`;
    });
    out.needsDecision(`${needy.length} website sign-in${needy.length === 1 ? '' : 's'} need you:\n${lines.join('\n')}`);
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
