// Records on stdout (harness.member/1), every string redacted of each secret value the run holds
// before it is written. The platform redacts too; this is the plugin's own net under it.

// Where a whole token may start, as the platform's ValueRedactor: after no letter or digit, or right
// after an escape that ends in one - \u0022 as JSON writes a quote, \n, %22 - so an escaped copy is
// caught. After it, an escape starts with \ or %, which is no letter or digit already.
const TOKEN_START = '(?:(?<![\\p{L}\\p{N}])|(?<=\\\\u[0-9A-Fa-f]{4})|(?<=\\\\[nrtbf])|(?<=%[0-9A-Fa-f]{2}))';

// A value, and its body inside a JSON string as .NET's default encoder writes it (+ < > & ' " ` and
// every non-ASCII character as \uXXXX), as the relaxed encoder does (only " \ and controls), and
// the relaxed one with / as \/ (PHP, some Java encoders).
export function spellings(value) {
  const control = { '\b': '\\b', '\f': '\\f', '\n': '\\n', '\r': '\\r', '\t': '\\t' };
  const u = (c) => '\\u' + c.charCodeAt(0).toString(16).toUpperCase().padStart(4, '0');
  const body = (strict) =>
    value
      .split('')
      .map((c) => {
        if (c === '\\') return '\\\\';
        if (control[c]) return control[c];
        const code = c.charCodeAt(0);
        if (code < 0x20) return u(c);
        if (c === '"') return strict ? u(c) : '\\"';
        if (strict && (code >= 0x7f || '+<>&\'`'.includes(c))) return u(c);
        return c;
      })
      .join('');
  const relaxed = body(false);
  return [...new Set([value, body(true), relaxed, relaxed.replaceAll('/', '\\/')])];
}

export function makeOut(write = (line) => process.stdout.write(line + '\n')) {
  // Each secret as { v, whole }: a whole one is redacted only where no letter or digit touches it.
  let values = [];
  let given = [];
  const redact = (s) => {
    if (typeof s !== 'string') return s;
    let out = s;
    for (const { v, whole } of values) {
      // Case-insensitive, longest first (values is sorted so).
      const escaped = v.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
      const pattern = whole ? `${TOKEN_START}${escaped}(?![\\p{L}\\p{N}])` : escaped;
      out = out.replace(new RegExp(pattern, 'giu'), '[redacted]');
    }
    return out;
  };
  const deep = (v) => {
    if (typeof v === 'string') return redact(v);
    if (Array.isArray(v)) return v.map(deep);
    if (v && typeof v === 'object') {
      const o = {};
      for (const [k, x] of Object.entries(v)) o[k] = deep(x);
      return o;
    }
    return v;
  };
  const emit = (record) => write(JSON.stringify(deep(record)));
  // A string is redacted wherever it appears; { token } only as a whole token.
  const setSecrets = (list) => {
    given = [...list];
    const byValue = new Map();
    for (const x of list) {
      const item = typeof x === 'string' ? { v: x, whole: false } : x && typeof x.token === 'string' ? { v: x.token, whole: true } : x;
      if (!item || typeof item.v !== 'string' || item.v.length < 4) continue;
      // Each value also as a JSON writer prints it, as the platform's ValueRedactor spells it.
      for (const v of spellings(item.v)) {
        const key = v.toLowerCase();
        const seen = byValue.get(key);
        byValue.set(key, { v, whole: (seen ? seen.whole : true) && item.whole });
      }
    }
    // Longest first; of two the same length, the one redacted anywhere first.
    values = [...byValue.values()].sort((a, b) => b.v.length - a.v.length || a.whole - b.whole);
  };
  return {
    setSecrets,
    // More values to redact, learned during the run (the session a sign-in obtains).
    addSecrets: (list) => setSecrets([...given, ...list]),
    redact,
    progress: (status) => emit({ t: 'progress', status }),
    needsDecision: (question) => emit({ t: 'needsDecision', question }),
    publish: (type, payload) => emit({ t: 'publish', type, payload }),
    rowStatus: (row, values) => emit({ t: 'row.status', field: 'entries', row, values }),
    result: (ok, words, quiet) => {
      const r = ok ? { t: 'result', ok: true, output: words } : { t: 'result', ok: false, error: words };
      if (ok && quiet) r.quiet = true;
      emit(r);
    },
  };
}
