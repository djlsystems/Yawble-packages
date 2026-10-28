// Records on stdout (harness.member/1), every string redacted of each secret value the run holds
// before it is written. The platform redacts too; this is the plugin's own net under it.

export function makeOut(write = (line) => process.stdout.write(line + '\n')) {
  let values = [];
  const redact = (s) => {
    if (typeof s !== 'string') return s;
    let out = s;
    for (const v of values) {
      if (!v) continue;
      // Case-insensitive, longest first (values is sorted so).
      const escaped = v.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
      out = out.replace(new RegExp(escaped, 'gi'), '[redacted]');
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
  const setSecrets = (list) => {
    values = [...new Set(list.filter((v) => typeof v === 'string' && v.length >= 4))].sort((a, b) => b.length - a.length);
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
