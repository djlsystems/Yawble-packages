// The entries table as a run receives it: config.entries (every cell but a secret one) and
// secrets.entries.<row>.<column> (the secret cells, the saved session among them).

export const Methods = ['password', 'token', 'interactive'];

export const Status = {
  verified: 'Verified',
  expired: 'Expired',
  needsYou: 'Needs you',
  failed: 'Failed',
  never: 'Never checked',
};

const secretColumns = ['password', 'totpSecret', 'token', 'session'];

// lower-cased key -> value, so a row's secrets are found whatever case the settings spell it in.
function byLowerKey(object) {
  const out = new Map();
  if (object && typeof object === 'object') {
    for (const [k, v] of Object.entries(object)) out.set(k.toLowerCase(), v);
  }
  return out;
}

function text(value) {
  return typeof value === 'string' ? value.trim() : '';
}

// The hosts an entry may use: its Site URL's, then each in `hosts` (names or URLs, separated by
// commas or spaces), lower case, without port. Answers [] for a Site URL that is not http(s).
export function entryHosts(siteUrl, hosts) {
  const out = [];
  const add = (h) => {
    const host = h.toLowerCase().replace(/\.$/, '');
    if (host && !out.includes(host)) out.push(host);
  };
  let site;
  try {
    site = new URL(siteUrl);
  } catch {
    return [];
  }
  if (site.protocol !== 'https:' && site.protocol !== 'http:') return [];
  add(site.hostname);
  for (const part of text(hosts).split(/[\s,]+/)) {
    if (!part) continue;
    if (/^[a-z][a-z0-9+.-]*:\/\//i.test(part)) {
      try {
        add(new URL(part).hostname);
      } catch {
        // not a URL: skipped
      }
    } else {
      add(part.replace(/\/.*$/, '').replace(/:\d+$/, ''));
    }
  }
  return out;
}

export function hostAllowed(entry, url) {
  let u;
  try {
    u = typeof url === 'string' ? new URL(url) : url;
  } catch {
    return false;
  }
  if (u.protocol !== 'https:' && u.protocol !== 'http:') return false;
  return entry.hosts.includes(u.hostname.toLowerCase());
}

// Resolves a path or URL on the entry's site: a path (/dashboard) is taken against the Site URL; a
// full URL must be on the entry's hosts. Answers { url } or { refusal }.
export function resolveOnSite(entry, pathOrUrl, what) {
  const raw = text(pathOrUrl);
  if (!raw) return { url: new URL(entry.siteUrl) };
  let url;
  try {
    url = /^[a-z][a-z0-9+.-]*:/i.test(raw) ? new URL(raw) : new URL(raw.startsWith('/') ? raw : '/' + raw, entry.siteUrl);
  } catch {
    return { refusal: `${entry.name}: ${what} "${raw}" is not a path or URL.` };
  }
  if (!hostAllowed(entry, url)) {
    return { refusal: `${entry.name}: ${what} is on ${url.hostname || url.protocol}, which is not one of the entry's hosts (${entry.hosts.join(', ')}).` };
  }
  return { url };
}

// Every entry of the request, each with its secrets beside it (never in the row itself).
export function readEntries(request) {
  const rows = Array.isArray(request?.config?.entries) ? request.config.entries : [];
  const secretRows = byLowerKey(request?.secrets?.entries);
  const out = [];
  for (const row of rows) {
    if (!row || typeof row !== 'object') continue;
    const name = text(row.name);
    if (!name) continue;
    const cells = byLowerKey(secretRows.get(name.toLowerCase()));
    const secrets = {};
    for (const column of secretColumns) {
      const v = cells.get(column.toLowerCase());
      if (typeof v === 'string' && v !== '') secrets[column] = v;
    }
    const verifyEvery = Number.isFinite(row.verifyEvery) ? row.verifyEvery : 24;
    const session = readSession(secrets.session);
    const siteUrl = text(row.siteUrl);
    out.push({
      name,
      key: name.toLowerCase(),
      siteUrl,
      hosts: entryHosts(siteUrl, row.hosts),
      method: Methods.includes(row.method) ? row.method : '',
      signInUrl: text(row.signInUrl),
      checkUrl: text(row.checkUrl),
      checkText: text(row.checkText),
      username: text(row.username),
      tokenHeader: text(row.tokenHeader) || 'Authorization',
      selectors: {
        username: text(row.usernameSelector),
        password: text(row.passwordSelector),
        submit: text(row.submitSelector),
        totp: text(row.totpSelector),
      },
      verifyEvery,
      active: row.active !== false,
      status: text(row.status) || Status.never,
      secrets,
      // The saved session, parsed: { storageState, identity, text }, or null when there is none or it
      // could not be read (secrets.session is then set but this is null).
      session,
      // What a person writes in the row, for noticing an edit: every cell but the plugin's own.
      written: writtenCells(row),
    });
  }
  return out;
}

const readOnlyColumns = new Set(['status', 'lastcheck', 'sessionexpires', 'detail']);

function writtenCells(row) {
  const out = {};
  for (const k of Object.keys(row).sort()) {
    if (!readOnlyColumns.has(k.toLowerCase())) out[k] = row[k];
  }
  return out;
}

export function findEntry(entries, name) {
  const key = text(name).toLowerCase();
  return entries.find((e) => e.key === key);
}

// Every secret value the run was handed, for the plugin's own redaction (the platform redacts too).
export function secretValues(request, entries) {
  const values = new Set();
  const walk = (v) => {
    if (typeof v === 'string') {
      if (v.length >= 4) values.add(v);
    } else if (v && typeof v === 'object') {
      for (const x of Object.values(v)) walk(x);
    }
  };
  walk(request?.secrets);
  for (const e of entries) {
    // A session's cookie and storage values are credentials of their own.
    if (e.session) for (const v of stateSecretValues(e.session.storageState)) values.add(v);
  }
  return [...values].sort((a, b) => b.length - a.length);
}

// The values of a storage state that are credentials, part by part, as the platform redacts a saved
// session: each cookie value, each local-storage value, and each string of 8 characters or more
// inside a local-storage value that is JSON. Whatever the row's method, so a page that prints its own
// cookie does not put it in an answer.
export function stateSecretValues(state) {
  const out = new Set();
  const add = (v, min = 4) => {
    if (typeof v === 'string' && v.length >= min) out.add(v);
  };
  const inside = (v) => {
    if (typeof v === 'string') add(v, 8);
    else if (Array.isArray(v)) v.forEach(inside);
    else if (v && typeof v === 'object') Object.values(v).forEach(inside);
  };
  for (const c of state?.cookies ?? []) add(c.value);
  for (const o of state?.origins ?? []) {
    for (const s of o.localStorage ?? []) {
      add(s.value);
      try {
        inside(JSON.parse(s.value));
      } catch {
        // not JSON: the whole value is enough
      }
    }
  }
  return [...out];
}

// A saved session as the platform's sign-in page keeps it: the v2 envelope
// { "v": 2, "storageState": {...}, "identity": {...} }, or a bare storage state saved before the
// envelope (v1). Answers { storageState, identity (null for v1), text } or null when it is unreadable.
export function readSession(text) {
  if (typeof text !== 'string' || text === '') return null;
  let value;
  try {
    value = JSON.parse(text);
  } catch {
    return null;
  }
  if (!value || typeof value !== 'object') return null;
  const isState = (s) => s && typeof s === 'object' && (Array.isArray(s.cookies) || Array.isArray(s.origins));
  const tidy = (s) => ({ cookies: Array.isArray(s.cookies) ? s.cookies : [], origins: Array.isArray(s.origins) ? s.origins : [] });
  if (value.v === 2 && isState(value.storageState)) {
    const identity = value.identity && typeof value.identity === 'object' ? value.identity : null;
    return { storageState: tidy(value.storageState), identity, text };
  }
  if (isState(value)) return { storageState: tidy(value), identity: null, text };
  return null;
}
