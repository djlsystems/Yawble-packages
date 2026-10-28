// What the plugin keeps between runs, in its own workspace (.web-credentials/state.json, 0600):
// per entry its last status and check, the failure counts behind the lockout guard and the
// three-failures rule, and a fingerprint of what the person wrote in
// the row; and every session handed off, so it is deleted once it expires. Never a secret: the
// fingerprint is a salted scrypt digest, and the handoffs are paths.
import { mkdirSync, readFileSync, writeFileSync, renameSync, rmSync, existsSync, chmodSync } from 'node:fs';
import { join } from 'node:path';
import { randomBytes, scryptSync } from 'node:crypto';

export function stateDir(workingDirectory) {
  const dir = join(workingDirectory, '.web-credentials');
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  chmodSync(dir, 0o700);
  return dir;
}

export function loadState(dir) {
  const file = join(dir, 'state.json');
  let state = {};
  if (existsSync(file)) {
    try {
      state = JSON.parse(readFileSync(file, 'utf8'));
    } catch {
      state = {};
    }
  }
  state.entries ??= {};
  state.handoffs ??= [];
  state.salt ??= randomBytes(16).toString('hex');
  return state;
}

export function saveState(dir, state) {
  const file = join(dir, 'state.json');
  const temp = file + '.tmp';
  writeFileSync(temp, JSON.stringify(state, null, 2), { mode: 0o600 });
  renameSync(temp, file);
}

export function entryState(state, entry) {
  const s = (state.entries[entry.key] ??= {
    status: entry.status || 'Never checked',
    reason: '',
    lastCheckAt: null,
    failedRuns: 0,
    passwordFailures: 0,
    held: false,
    fingerprint: null,
  });
  return s;
}

// A digest of what the person wrote in the row, secrets included, so a changed row clears the
// lockout guard. Salted scrypt: the state file never holds a value or a fast hash of one.
export function fingerprint(state, entry) {
  const material = JSON.stringify({ written: entry.written, secrets: entry.secrets });
  return scryptSync(material, Buffer.from(state.salt, 'hex'), 32, { N: 1 << 14 }).toString('hex');
}

// Deletes every handed-off session whose time is up (or is past `now` by any amount) and forgets it.
export function sweepHandoffs(state, now = Date.now()) {
  const kept = [];
  let removed = 0;
  for (const h of state.handoffs) {
    if (Date.parse(h.expiresAt) <= now) {
      rmSync(h.path, { force: true });
      removed++;
    } else if (existsSync(h.path)) {
      kept.push(h);
    }
  }
  state.handoffs = kept;
  return removed;
}
