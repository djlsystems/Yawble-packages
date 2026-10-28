// Records on stdout (harness.member/1), every string redacted of each secret value the run holds
// before it is written. The platform redacts too; this is the plugin's own net under it.

export function makeOut(write = (line) => process.stdout.write(line + '\n')) {
  // Each secret as { v, whole }: a whole one is redacted only where no letter or digit touches it.
  let values = [];
  const redact = (s) => {
    if (typeof s !== 'string') return s;
    let out = s;
    for (const { v, whole } of values) {
      // Case-insensitive, longest first (values is sorted so).
      const escaped = v.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
      const pattern = whole ? `(?<![\\p{L}\\p{N}])${escaped}(?![\\p{L}\\p{N}])` : escaped;
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
    const byValue = new Map();
    for (const x of list) {
      const item = typeof x === 'string' ? { v: x, whole: false } : x && typeof x.token === 'string' ? { v: x.token, whole: true } : x;
      if (!item || typeof item.v !== 'string' || item.v.length < 4) continue;
      const seen = byValue.get(item.v);
      byValue.set(item.v, { v: item.v, whole: (seen ? seen.whole : true) && item.whole });
    }
    values = [...byValue.values()].sort((a, b) => b.v.length - a.v.length);
  };
  return {
    setSecrets,
    // More values to redact, learned during the run (the session a sign-in obtains).
    addSecrets: (list) => setSecrets([...values, ...list]),
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
