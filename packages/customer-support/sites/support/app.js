// The documents the Manager keeps by the support-playbook skill: counts/running, the totals and the
// last summary, and inquiries, one per inquiry. Senders and subjects come from email, so every
// field is only ever set with textContent.

function when(text) {
  if (!text) return '';
  const d = new Date(text);
  return isNaN(d) ? String(text) : d.toLocaleString();
}

function number(v) {
  const n = Number(v);
  return Number.isFinite(n) && n >= 0 ? Math.floor(n) : 0;
}

// view turns the two collections, as site.data.list answers them, into what the page shows.
function view(counts, inquiries) {
  const running = (counts.find((item) => item.id === 'running') || {}).doc || null;
  const batch = running && running.lastBatch ? running.lastBatch : null;
  const all = inquiries.map((item) => item.doc || {}).sort((a, b) => String(a.since || '').localeCompare(String(b.since || '')));
  return {
    counts: running && {
      newCustomers: number(running.newCustomers),
      orders: number(running.orders),
      other: number(running.other),
      since: running.since || '',
      updatedAt: running.updatedAt || '',
    },
    batch: batch && { at: batch.at || '', lines: Array.isArray(batch.lines) ? batch.lines.map(String) : [] },
    open: all.filter((d) => d.status !== 'closed'),
    closed: all.filter((d) => d.status === 'closed').reverse(),
  };
}

function rows(body, docs, cells) {
  const out = [];
  for (const d of docs) {
    const tr = document.createElement('tr');
    for (const text of cells(d)) {
      const td = document.createElement('td');
      td.textContent = text || '';
      tr.append(td);
    }
    out.push(tr);
  }
  document.getElementById(body).replaceChildren(...out);
  document.getElementById(body + '-none').classList.toggle('hidden', out.length > 0);
  document.getElementById(body + '-table').classList.toggle('hidden', out.length === 0);
}

function render(v) {
  const c = v.counts;
  document.getElementById('counts-none').classList.toggle('hidden', !!c);
  document.getElementById('counts').classList.toggle('hidden', !c);
  if (c) {
    document.getElementById('new-customers').textContent = String(c.newCustomers);
    document.getElementById('orders').textContent = String(c.orders);
    document.getElementById('other').textContent = String(c.other);
    document.getElementById('since').textContent = when(c.since);
    document.getElementById('updated').textContent = when(c.updatedAt);
  }

  const lines = v.batch ? v.batch.lines : [];
  document.getElementById('batch-none').classList.toggle('hidden', lines.length > 0);
  document.getElementById('batch-at').textContent = v.batch && v.batch.at ? 'Texted ' + when(v.batch.at) : '';
  document.getElementById('batch').replaceChildren(...lines.map((line) => {
    const li = document.createElement('li');
    li.textContent = line;
    return li;
  }));

  rows('open', v.open, (d) => [d.from, d.subject, when(d.since), String(number(d.reminders)), d.lastRemindedAt ? when(d.lastRemindedAt) : 'Not yet']);
  rows('closed', v.closed, (d) => [d.from, d.subject, when(d.since), when(d.repliedAt || d.closedAt)]);
}

async function refresh() {
  try {
    const [counts, inquiries] = await Promise.all([site.data.list('counts'), site.data.list('inquiries')]);
    render(view(counts, inquiries));
    document.getElementById('status').textContent = 'Read ' + new Date().toLocaleTimeString() + '.';
  } catch (err) {
    document.getElementById('status').textContent = err.message;
  }
}

if (typeof module !== 'undefined') {
  module.exports = { view };
} else {
  refresh();
  setInterval(refresh, 10000);
  addEventListener('focus', refresh);
}
