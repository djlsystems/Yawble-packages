// Shared test helpers: run the plugin like the Host does (one request in, the JSON-Lines records
// out) and read them back. The real out.mjs is used, so the plugin's own redaction is exercised.
import { mkdtempSync, rmSync, readdirSync, readFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { makeOut } from '../src/out.mjs';
import { run } from '../src/main.mjs';

// Tests share one Google Chrome folder rather than fetching it into every test workspace: the first
// run fetches it there (chrome.mjs), every later one reuses it. Typing is paced faster than a
// person's, still one key at a time.
process.env.WEB_CREDENTIALS_CHROME_ROOT ||= join(tmpdir(), 'web-credentials-chrome');
process.env.WEB_CREDENTIALS_PACE ||= '0.3';

export function workspace() {
  return mkdtempSync(join(tmpdir(), 'wc-'));
}

// Runs the plugin and answers every record it wrote, plus the raw lines (for a no-leak scan).
export async function runPlugin({ entries, secrets = {}, work, workingDirectory, settings = {} }) {
  const lines = [];
  const out = makeOut((line) => lines.push(line));
  const request = {
    protocol: 'harness.member/1',
    plugin: { id: 'web-credentials', version: '1.0.0' },
    member: { id: 'T/Creds', team: 'T', name: 'Creds' },
    workingDirectory,
    config: { entries, ...settings },
    secrets,
    work,
  };
  await run(request, out);
  const records = lines.map((l) => JSON.parse(l));
  return {
    lines,
    records,
    result: records.find((r) => r.t === 'result'),
    events: records.filter((r) => r.t === 'publish'),
    rowStatus: records.filter((r) => r.t === 'row.status'),
    decisions: records.filter((r) => r.t === 'needsDecision'),
    progress: records.filter((r) => r.t === 'progress'),
  };
}

export function listFilesUnder(dir) {
  const out = [];
  const walk = (d) => {
    if (!existsSync(d)) return;
    for (const name of readdirSync(d, { withFileTypes: true })) {
      const p = join(d, name.name);
      if (name.isDirectory()) walk(p);
      else out.push(p);
    }
  };
  walk(dir);
  return out;
}

export function readText(path) {
  return readFileSync(path, 'utf8');
}

export function cleanup(dir) {
  rmSync(dir, { recursive: true, force: true });
}
